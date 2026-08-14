package imapserver

import (
	"compress/flate"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type literalRequest struct {
	tag        string
	descriptor *commandDescriptor
	info       imapwire.LiteralInfo
	reply      chan error
}

type readerRequest struct {
	read  func(*imapwire.Decoder) (any, error)
	reply chan readerResult
}

type readerResult struct {
	value any
	err   error
}

type compressedConn struct {
	net.Conn

	readOnce sync.Once
	reader   io.ReadCloser
	writeMu  sync.Mutex
	writer   *flate.Writer
}

func newCompressedConn(conn net.Conn) (*compressedConn, error) {
	writer, err := flate.NewWriter(conn, flate.DefaultCompression)
	if err != nil {
		return nil, err
	}
	return &compressedConn{Conn: conn, writer: writer}, nil
}

func (c *compressedConn) Read(p []byte) (int, error) {
	c.readOnce.Do(func() { c.reader = flate.NewReader(c.Conn) })
	return c.reader.Read(p)
}

func (c *compressedConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	n, err := c.writer.Write(p)
	if err == nil {
		err = c.writer.Flush()
	}
	return n, err
}

const maxLiteralMinusSize int64 = 4096

type conn struct {
	server *Server
	raw    net.Conn

	ctx    context.Context
	cancel context.CancelCauseFunc

	transportMu sync.Mutex
	transport   net.Conn
	encoder     *imapwire.Encoder // event-loop owned

	commands        chan *queuedCommand
	literalRequests chan literalRequest
	readerRequests  chan readerRequest
	budgetFreed     chan struct{}
	queuedBytes     atomic.Int64
	// inputPending reports that the reader has bytes it has not yet turned into
	// a command — that the client is pipelining. Written by the reader
	// goroutine, read by the event loop, which is why it is atomic.
	inputPending atomic.Bool
	utf8Accept   atomic.Bool
	resetDecoder atomic.Bool
	ready        chan struct{}

	// notifyQueue and notifyUpdater are the session-scoped NOTIFY channel,
	// distinct from the selection-scoped Updater by design. See ext_d_notify.go.
	notifyQueue   *sessionUpdateQueue
	notifyUpdater *SessionUpdater

	state sessionState // event-loop owned
	// executingSeqSensitive is event-loop owned, like state: it is set and
	// cleared inside execute and read only from drainUpdates.
	executingSeqSensitive bool
	logout                bool

	fatalMu sync.Mutex
	fatal   error

	closeOnce    sync.Once
	preAuthTimer *time.Timer
}

func newConn(parent context.Context, server *Server, netConn net.Conn) (*conn, error) {
	ctx, cancel := context.WithCancelCause(parent)
	tlsActive := false
	if _, ok := netConn.(*tls.Conn); ok {
		tlsActive = true
	}
	queueSize := server.options.Limits.MaxQueuedCommands
	if queueSize < 1 {
		queueSize = defaultMaxQueuedCommands
	}
	c := &conn{
		server:          server,
		raw:             netConn,
		ctx:             ctx,
		cancel:          cancel,
		transport:       netConn,
		commands:        make(chan *queuedCommand, queueSize),
		literalRequests: make(chan literalRequest),
		readerRequests:  make(chan readerRequest),
		budgetFreed:     make(chan struct{}, 1),
		ready:           make(chan struct{}),
		state:           newSessionState(tlsActive),
	}
	c.encoder = c.newEncoder(netConn)
	return c, nil
}

func (c *conn) newEncoder(writer io.Writer) *imapwire.Encoder {
	return imapwire.NewEncoder(writer, &imapwire.EncoderOptions{
		ServerResponse: true,
		WriteTimeout:   c.server.options.Limits.WriteTimeout,
	})
}

func (c *conn) newDecoder(reader io.Reader) *imapwire.Decoder {
	readTimeout := c.server.options.Limits.ReadTimeout
	if c.state.state == stateNotAuthenticated && c.server.options.Limits.PreAuthTimeout > 0 &&
		(readTimeout <= 0 || c.server.options.Limits.PreAuthTimeout < readTimeout) {
		readTimeout = c.server.options.Limits.PreAuthTimeout
	}
	decoder := imapwire.NewDecoder(reader, &imapwire.Options{
		MaxLiteralSize:         c.server.options.Limits.MaxLiteralBytes,
		MaxBufferedLiteralSize: c.server.options.Limits.MaxLiteralBytes,
		MaxLineLength:          c.server.options.Limits.MaxCommandLineBytes,
		ReadTimeout:            readTimeout,
	})
	decoder.SetUTF8Accept(c.utf8Accept.Load())
	return decoder
}

func (c *conn) serve() error {
	if timeout := c.server.options.Limits.PreAuthTimeout; timeout > 0 {
		c.preAuthTimer = time.AfterFunc(timeout, func() { c.cancel(errPreAuthTimeout) })
		defer c.preAuthTimer.Stop()
	}
	readerDone := make(chan error, 1)
	loopDone := make(chan error, 1)
	go func() { readerDone <- c.readCommands() }()
	go func() { loopDone <- c.eventLoop() }()

	err := <-loopDone
	c.cancel(err)
	c.closeTransport()
	readerErr := <-readerDone
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
		return err
	}
	if readerErr != nil && !errors.Is(readerErr, io.EOF) && !errors.Is(readerErr, net.ErrClosed) && !errors.Is(readerErr, context.Canceled) {
		return readerErr
	}
	return nil
}

func (c *conn) readCommands() error {
	select {
	case <-c.ready:
	case <-c.ctx.Done():
		return context.Cause(c.ctx)
	}

	var currentTag string
	var currentDescriptor *commandDescriptor
	newCommandDecoder := func() *imapwire.Decoder {
		decoder := c.newDecoder(c.currentTransport())
		decoder.SetLiteralDecision(func(info imapwire.LiteralInfo) error {
			request := literalRequest{
				tag:        currentTag,
				descriptor: currentDescriptor,
				info:       info,
				reply:      make(chan error, 1),
			}
			select {
			case c.literalRequests <- request:
			case <-c.ctx.Done():
				return context.Cause(c.ctx)
			}
			select {
			case err := <-request.reply:
				return err
			case <-c.ctx.Done():
				return context.Cause(c.ctx)
			}
		})
		return decoder
	}
	decoder := newCommandDecoder()

	commands := 0
	for {
		currentTag = ""
		currentDescriptor = nil
		tag, name, err := decoder.BeginCommand()
		currentTag = tag
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.cancel(io.EOF)
				return io.EOF
			}
			fatal := decoder.Fatal()
			if !fatal {
				_ = decoder.DiscardLine()
			}
			command := &queuedCommand{tag: tag, name: name, parseErr: err, done: make(chan struct{})}
			if queueErr := c.queueCommand(command); queueErr != nil {
				return queueErr
			}
			if fatal {
				c.cancel(err)
				return err
			}
			continue
		}

		commands++
		if max := c.server.options.Limits.MaxCommands; max >= 0 && commands > max {
			err := fmt.Errorf("imapserver: command limit exceeded")
			c.cancel(err)
			return err
		}

		descriptor := commandDescriptors[name]
		currentDescriptor = descriptor
		command := &queuedCommand{tag: tag, name: name, descriptor: descriptor, done: make(chan struct{})}
		if descriptor == nil {
			if err := discardUnknownCommand(decoder); err != nil {
				c.cancel(err)
				return err
			}
		} else {
			command.args, command.bytes, command.parseErr = descriptor.parse(decoder)
			if command.parseErr != nil {
				fatal := decoder.Fatal()
				if !fatal {
					_ = decoder.DiscardLine()
				}
				if fatal {
					if err := c.queueCommand(command); err != nil {
						return err
					}
					c.cancel(command.parseErr)
					return command.parseErr
				}
			}
		}
		command.bytes += int64(len(tag) + len(name) + 2)
		if max := c.server.options.Limits.MaxQueuedCommandBytes; max >= 0 && command.bytes > max {
			command.args = nil
			command.bytes = int64(len(tag) + len(name) + 2)
			command.parseErr = fmt.Errorf("command payload exceeds queue byte limit")
		}
		// Published after the whole command line has been consumed and before
		// the command is queued. Both halves matter: read it earlier and the
		// command's own unparsed arguments count as pipelined input, so it is
		// always true; publish it later and the event loop can see the command
		// without seeing that more follows, which is the race that lets an
		// expunge into a pipeline.
		c.inputPending.Store(decoder.Buffered() > 0)

		if err := c.queueCommand(command); err != nil {
			return err
		}
		if descriptor != nil && descriptor.barrier {
			if err := c.waitBarrier(decoder, command); err != nil {
				return err
			}
			if c.logout {
				return nil
			}
			if c.resetDecoder.Swap(false) {
				// Transport-changing barriers discard bytes buffered under the
				// previous framing (STARTTLS's injection defence relies on this).
				decoder = newCommandDecoder()
			} else {
				decoder.SetUTF8Accept(c.utf8Accept.Load())
			}
		}
	}
}

func (c *conn) waitBarrier(decoder *imapwire.Decoder, command *queuedCommand) error {
	for {
		select {
		case <-command.done:
			return nil
		case request := <-c.readerRequests:
			value, err := request.read(decoder)
			if err != nil && !decoder.Fatal() {
				_ = decoder.DiscardLine()
			}
			request.reply <- readerResult{value: value, err: err}
			if decoder.Fatal() {
				c.cancel(err)
				return err
			}
		case <-c.ctx.Done():
			return context.Cause(c.ctx)
		}
	}
}

func (c *conn) readClientData(ctx context.Context, read func(*imapwire.Decoder) (any, error)) (any, error) {
	request := readerRequest{read: read, reply: make(chan readerResult, 1)}
	if err := c.requestClientData(ctx, request); err != nil {
		return nil, err
	}
	for {
		select {
		case result := <-request.reply:
			return result.value, result.err
		case literal := <-c.literalRequests:
			// A read requested by a handler may itself meet a literal — a
			// MULTIAPPEND message after the first, or a CATENATE TEXT part.
			// Literal approval is normally serviced by the event loop, but the
			// handler that is waiting here *is* the event loop, so leaving it
			// to the outer select would deadlock: the reader waits for approval
			// that only this goroutine can give.
			//
			// Servicing it inline is safe for the same reason. handleLiteralRequest
			// reads connection state and writes the continuation through the
			// event-loop-owned encoder, and this is that goroutine.
			c.handleLiteralRequest(literal)
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-c.ctx.Done():
			return nil, context.Cause(c.ctx)
		}
	}
}

func (c *conn) requestClientData(ctx context.Context, request readerRequest) error {
	select {
	case c.readerRequests <- request:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-c.ctx.Done():
		return context.Cause(c.ctx)
	}
}

func (c *conn) continueLine(ctx context.Context, prompt string) (string, error) {
	c.encoder.BeginResponse(imapwire.ResponseContinuation, "").ContinuationText(prompt).CRLF()
	if err := c.encoder.Flush(); err != nil {
		return "", err
	}
	value, err := c.readClientData(ctx, func(decoder *imapwire.Decoder) (any, error) {
		var line string
		if !decoder.ExpectText(&line) || !decoder.ExpectCRLF() {
			return nil, decoder.Err()
		}
		return line, nil
	})
	if err != nil {
		return "", err
	}
	line, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("imapserver: reader returned an invalid continuation value")
	}
	return line, nil
}

//lint:ignore U1000 T22's IDLE handler supplies the tagged completion around this framework-owned wait.
func (c *conn) idleUntilDone(ctx context.Context) error {
	c.encoder.BeginResponse(imapwire.ResponseContinuation, "").ContinuationText("idling").CRLF()
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	request := readerRequest{
		read: func(decoder *imapwire.Decoder) (any, error) {
			var line string
			if !decoder.ExpectText(&line) || !decoder.ExpectCRLF() {
				return nil, decoder.Err()
			}
			return line, nil
		},
		reply: make(chan readerResult, 1),
	}
	if err := c.requestClientData(ctx, request); err != nil {
		return err
	}
	for {
		select {
		case result := <-request.reply:
			if result.err != nil {
				return result.err
			}
			line, ok := result.value.(string)
			if !ok || !strings.EqualFold(line, "DONE") {
				return fmt.Errorf("imapserver: IDLE expects DONE")
			}
			return nil
		case <-c.updateSignal():
			if err := c.drainUpdates(updateAccounting{}); err != nil {
				return err
			}
		case <-c.notifySignal():
			// NOTIFY events about unselected mailboxes reach the client during
			// IDLE too — that is the point of the extension.
			if err := c.drainNotify(); err != nil {
				return err
			}
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-c.ctx.Done():
			return context.Cause(c.ctx)
		}
	}
}

func discardUnknownCommand(decoder *imapwire.Decoder) error {
	err := decoder.DiscardLine()
	if err == nil || decoder.Fatal() {
		return err
	}
	// The first pass may itself discover and reject a literal. A second pass
	// clears that non-fatal decision error and either drains the remaining
	// non-synchronising command or honours the synchronising command boundary.
	return decoder.DiscardLine()
}

func (c *conn) queueCommand(command *queuedCommand) error {
	if err := c.acquireCommandBytes(command.bytes); err != nil {
		return err
	}
	select {
	case c.commands <- command:
		return nil
	case <-c.ctx.Done():
		c.releaseCommandBytes(command.bytes)
		return context.Cause(c.ctx)
	}
}

func (c *conn) acquireCommandBytes(size int64) error {
	max := c.server.options.Limits.MaxQueuedCommandBytes
	if max < 0 {
		c.queuedBytes.Add(size)
		return nil
	}
	for {
		current := c.queuedBytes.Load()
		if size <= max-current && c.queuedBytes.CompareAndSwap(current, current+size) {
			return nil
		}
		select {
		case <-c.budgetFreed:
		case <-c.ctx.Done():
			return context.Cause(c.ctx)
		}
	}
}

func (c *conn) releaseCommandBytes(size int64) {
	c.queuedBytes.Add(-size)
	select {
	case c.budgetFreed <- struct{}{}:
	default:
	}
}

func (c *conn) eventLoop() (retErr error) {
	defer c.cleanupBackend()
	if tlsConn, ok := c.currentTransport().(*tls.Conn); ok {
		handshakeCtx, cancel := c.preAuthContext(c.ctx)
		if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
			cancel()
			close(c.ready)
			return err
		}
		cancel()
	}
	if err := c.writeGreeting(); err != nil {
		close(c.ready)
		return err
	}
	close(c.ready)

	for {
		select {
		case request := <-c.literalRequests:
			c.handleLiteralRequest(request)
		case <-c.notifySignal():
			if err := c.drainNotify(); err != nil {
				c.failFatal(err)
				return
			}
		case command := <-c.commands:
			c.releaseCommandBytes(command.bytes)
			wasPreAuth := c.state.state == stateNotAuthenticated
			err := c.execute(command)
			if wasPreAuth && c.state.state != stateNotAuthenticated {
				if c.preAuthTimer != nil {
					c.preAuthTimer.Stop()
				}
				// Authentication changes both the allowed commands and the
				// connection's read deadline. AUTHENTICATE/LOGIN are barriers,
				// so rebuild the decoder before admitting another command.
				c.resetDecoder.Store(true)
			}
			close(command.done)
			if err != nil {
				return err
			}
			if c.logout {
				return nil
			}
		case <-c.updateSignal():
			if err := c.drainUpdates(updateAccounting{}); err != nil {
				return err
			}
		case <-c.notifySignal():
			// NOTIFY events about unselected mailboxes reach the client during
			// IDLE too — that is the point of the extension.
			if err := c.drainNotify(); err != nil {
				return err
			}
		case <-c.ctx.Done():
			cause := context.Cause(c.ctx)
			if errors.Is(c.fatalError(), ErrUpdateOverflow) {
				c.tryOverflowBye()
			}
			return cause
		}
	}
}

func (c *conn) execute(command *queuedCommand) error {
	if command.parseErr != nil {
		return c.writeBad(command.tag, "invalid command syntax")
	}
	if command.descriptor == nil {
		return c.writeBad(command.tag, "unknown command")
	}
	if !c.state.allows(command.descriptor.states) {
		return c.writeBad(command.tag, "command is not valid in this state")
	}
	// Scoped to the pre-command drain alone. For a sequence-sensitive command
	// that arrived in a pipeline, delivering here is as unsafe as delivering
	// mid-command: the client composed this command against the numbering it
	// has now. The handler's own drain, after its tagged response, is the point
	// at which delivery becomes correct — so the flag is cleared before the
	// handler runs, or that drain would defer forever and the update would
	// never be sent at all.
	c.executingSeqSensitive = sequenceSensitiveCommands[command.name]
	err := c.drainUpdates(updateAccounting{})
	c.executingSeqSensitive = false
	if err != nil {
		return err
	}
	ctx := c.ctx
	cancel := func() {}
	if timeout := c.server.options.Limits.CommandTimeout; timeout > 0 {
		ctx, cancel = context.WithTimeout(c.ctx, timeout)
	}
	defer cancel()
	return command.descriptor.handle(ctx, c, command)
}

func (c *conn) handleLiteralRequest(request literalRequest) {
	var err error
	if request.descriptor == nil || !c.state.allows(request.descriptor.states) {
		err = fmt.Errorf("literal is not accepted for this command")
	} else if max := c.server.options.Limits.MaxLiteralBytes; max >= 0 && request.info.Size > max {
		err = fmt.Errorf("literal exceeds the connection limit")
	} else if request.info.NonSynchronising && request.info.Size > maxLiteralMinusSize &&
		!slices.Contains(deriveCapabilities(&c.state, c.server), "LITERAL+") {
		err = fmt.Errorf("non-synchronising literal exceeds the LITERAL- limit")
	} else if !request.info.NonSynchronising {
		c.encoder.BeginResponse(imapwire.ResponseContinuation, "").ContinuationText("ready for literal").CRLF()
		err = c.encoder.Flush()
	}
	request.reply <- err
}

func (c *conn) writeGreeting() error {
	capabilities := deriveCapabilities(&c.state, c.server)
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").RespCond(imapwire.RespCond{
		Status: "OK",
		Text: imapwire.RespText{
			Code: "CAPABILITY",
			Args: strings.Join(capabilities, " "),
			Text: c.server.options.Greeting,
		},
	}).CRLF()
	return c.encoder.Flush()
}

func (c *conn) writeTagged(tag, status, text string) error {
	c.encoder.BeginResponse(imapwire.ResponseTagged, tag).RespCond(imapwire.RespCond{Status: status, Text: imapwire.RespText{Text: text}}).CRLF()
	return c.encoder.Flush()
}

func (c *conn) writeBad(tag, text string) error {
	if tag == "" {
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").RespCond(imapwire.RespCond{Status: "BAD", Text: imapwire.RespText{Text: text}}).CRLF()
		return c.encoder.Flush()
	}
	return c.writeTagged(tag, "BAD", text)
}

func (c *conn) upgradeTLS(ctx context.Context) error {
	config := c.server.options.TLSConfig
	if config == nil {
		return fmt.Errorf("imapserver: TLS is not configured")
	}
	old := c.currentTransport()
	tlsConn := tls.Server(old, config.Clone())
	handshakeCtx, cancel := c.preAuthContext(ctx)
	defer cancel()
	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		return err
	}
	c.transportMu.Lock()
	c.transport = tlsConn
	c.transportMu.Unlock()
	c.encoder = c.newEncoder(tlsConn)
	c.state.tls = true
	c.resetDecoder.Store(true)
	return nil
}

func (c *conn) preAuthContext(parent context.Context) (context.Context, context.CancelFunc) {
	if timeout := c.server.options.Limits.PreAuthTimeout; timeout > 0 {
		return context.WithTimeout(parent, timeout)
	}
	return context.WithCancel(parent)
}

func (c *conn) enableCompression() error {
	transport, err := newCompressedConn(c.currentTransport())
	if err != nil {
		return err
	}
	c.transportMu.Lock()
	c.transport = transport
	c.transportMu.Unlock()
	c.encoder = c.newEncoder(transport)
	c.state.compressed = true
	c.resetDecoder.Store(true)
	return nil
}

func (c *conn) currentTransport() net.Conn {
	c.transportMu.Lock()
	defer c.transportMu.Unlock()
	return c.transport
}

func (c *conn) cleanupBackend() {
	if selected := c.state.unselect(); selected != nil {
		selected.close()
		ctx, cancel := context.WithTimeout(context.Background(), c.server.options.Limits.CommandTimeout)
		_ = selected.mailbox.Unselect(ctx, nil)
		cancel()
	}
	if session := c.state.session; session != nil {
		ctx, cancel := context.WithTimeout(context.Background(), c.server.options.Limits.CommandTimeout)
		_ = session.Close(ctx, nil)
		cancel()
		c.state.session = nil
	}
}

func (c *conn) updateSignal() <-chan struct{} {
	if c.state.selected == nil || c.state.selected.queue == nil {
		return nil
	}
	return c.state.selected.queue.signal
}

// expungeDeliveryDeferred reports whether unsolicited updates must wait.
//
// RFC 3501 §7.4.1: an EXPUNGE response must not be sent while responding to a
// FETCH, STORE or SEARCH, "to prevent a loss of synchronization of message
// sequence numbers between client and server".
//
// Deferring past a command's tagged completion satisfies that for a client that
// waits, and Dovecot's imaptest showed it does not for a client that pipelines:
// "after the tagged OK of command n" is simultaneously "while command n+1 is in
// flight", so the expunge moves out of one forbidden window and into the next.
// The captured transcript is in docs/INTEROP.md.
//
// The condition is therefore the queue, not the command that just finished. A
// command already read but not yet executed was composed by the client against
// the sequence view as it stands now; renumbering before it runs invalidates
// the numbers it carries. Waiting until the connection has caught up is the
// only point at which the two views are known to agree.
//
// Deferral is for *unsolicited* updates only. A drain carrying an origin or an
// effect is accounting for changes this command caused, and it must run: that
// is what suppresses the duplicate responses the command already wrote itself.
// Those changes are also ones the client asked for, so they are not a surprise
// to it.
func (c *conn) expungeDeliveryDeferred(accounting updateAccounting) bool {
	if accounting.origin != 0 || accounting.effect != effectNone {
		return false
	}
	// Three ways to still be behind the client: a command parsed and waiting,
	// bytes read but not yet parsed, or the command running right now being one
	// whose numbering the client is relying on. The last is what makes the
	// *final* command of a pipeline safe — its own completion drain is the
	// first moment both views are known to agree.
	return len(c.commands) > 0 || c.inputPending.Load() || c.executingSeqSensitive
}

// sequenceSensitiveCommands are the commands whose arguments or responses carry
// message sequence numbers, and therefore the ones RFC 3501 §7.4.1 protects.
// UID covers UID FETCH, UID STORE and UID SEARCH, whose untagged responses
// still carry sequence numbers even though their arguments do not.
var sequenceSensitiveCommands = map[string]bool{
	"FETCH":  true,
	"STORE":  true,
	"SEARCH": true,
	"SORT":   true,
	"THREAD": true,
	"UID":    true,
}

func (c *conn) drainUpdates(accounting updateAccounting) error {
	selected := c.state.selected
	if selected == nil || selected.queue == nil {
		return nil
	}
	// Left queued, deliberately: nothing is popped, so the framework's sequence
	// view does not advance past the client's either. Popping and withholding
	// the responses would produce exactly the desynchronisation this prevents.
	if c.expungeDeliveryDeferred(accounting) {
		return nil
	}
	// removed collects the expunged UIDs so CONTEXT registrations can be told
	// which of their matches went away. See ext_e_context.go.
	var removed []imap.UID
	var pending []deliveredUpdate
	for _, batch := range selected.queue.popAll() {
		updates, err := selected.applyBatch(batch, accounting)
		if err != nil {
			// Once an accepted batch cannot be applied, the framework no longer
			// knows the selected mailbox's sequence view. Continuing would risk
			// emitting responses against a corrupt mapping.
			c.failFatal(err)
			return err
		}
		pending = append(pending, updates...)
	}
	for _, update := range coalesceWireUpdates(pending) {
		if update.kind == updateMessageExpunge || update.kind == updateMessageVanished {
			removed = append(removed, update.uid)
		}
		if err := c.writeUpdate(update); err != nil {
			return err
		}
	}
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	// CONTEXT registrations hear which of their matches were removed. This runs
	// after the ordinary updates so the client sees the expunge before the
	// REMOVEFROM that explains it.
	return notifySearchContexts(c, removed)
}

func (c *conn) writeUpdate(update deliveredUpdate) error {
	switch update.kind {
	case updateExists:
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Number(update.exists).SP().Atom("EXISTS").CRLF()
	case updateMessageFlags:
		data := &imap.FetchMessageData{
			SeqNum: update.seqNum,
			Items: map[imap.FetchDataKey][]imap.FetchData{
				imap.FetchDataKey("FLAGS"): {imap.FetchDataFlags(update.flags)},
			},
		}
		if update.modSeq != 0 {
			data.Items[imap.FetchDataKey("MODSEQ")] = []imap.FetchData{imap.FetchDataModSeq(update.modSeq)}
		}
		// UIDONLY reshapes unilateral responses too: a client that enabled it
		// has discarded the machinery for interpreting a sequence number, so
		// one arriving unsolicited is worse than none at all.
		// See ext_d_uidonly.go.
		if uidOnlyEnabled(c) {
			return imapcodec.WriteUIDFetchResponse(c.encoder, update.uid, data, nil)
		}
		return imapcodec.WriteFetchResponse(c.encoder, data, nil)
	case updateMessageExpunge:
		// An untagged EXPUNGE carries a sequence number, which UIDONLY forbids
		// outright (RFC 9586 section 3.3) and which QRESYNC replaces with
		// VANISHED for the life of the session (RFC 7162 section 3.2.7).
		if removalsUseVanished(c) {
			c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("VANISHED").SP().
				RawValue([]byte(imap.UIDSetNum(update.uid).String())).CRLF()
			break
		}
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Number(uint32(update.seqNum)).SP().Atom("EXPUNGE").CRLF()
	case updateMessageVanished:
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("VANISHED")
		if update.earlier {
			c.encoder.SP().Special('(').Atom("EARLIER").Special(')')
		}
		c.encoder.SP().RawValue([]byte(update.uids.String())).CRLF()
	}
	return c.encoder.Err()
}

func (c *conn) failFatal(err error) {
	c.fatalMu.Lock()
	if c.fatal == nil {
		c.fatal = err
	}
	c.fatalMu.Unlock()
	c.cancel(err)
}

func (c *conn) fatalError() error {
	c.fatalMu.Lock()
	defer c.fatalMu.Unlock()
	return c.fatal
}

func (c *conn) updateOverflow() {
	c.failFatal(ErrUpdateOverflow)
	time.AfterFunc(c.server.options.Limits.ForceCloseTimeout, c.closeTransport)
}

func (c *conn) tryOverflowBye() {
	transport := c.currentTransport()
	if timeout := c.server.options.Limits.OverflowWriteTimeout; timeout > 0 {
		_ = transport.SetWriteDeadline(time.Now().Add(timeout))
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").RespCond(imapwire.RespCond{Status: "BYE", Text: imapwire.RespText{Text: "update queue overflow"}}).CRLF()
	_ = c.encoder.Flush()
}

func (c *conn) closeTransport() {
	c.closeOnce.Do(func() {
		c.cancel(net.ErrClosed)
		_ = c.raw.Close()
		transport := c.currentTransport()
		if transport != c.raw {
			_ = transport.Close()
		}
	})
}

func (c *conn) close() { c.closeTransport() }
