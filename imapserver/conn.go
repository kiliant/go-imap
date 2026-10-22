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
	"github.com/kiliant/go-imap/internal/imapdeflate"
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

	reader  *imapdeflate.Reader
	writeMu sync.Mutex
	writer  *flate.Writer
}

func newCompressedConn(conn net.Conn) (*compressedConn, error) {
	input := io.Reader(conn)
	if plain, ok := conn.(*bufferedConn); ok {
		conn = plain.Conn
		input = imapdeflate.ResumeReader(plain.reader)
	}
	writer, err := flate.NewWriter(conn, flate.DefaultCompression)
	if err != nil {
		return nil, err
	}
	return &compressedConn{Conn: conn, writer: writer, reader: imapdeflate.NewReader(input)}, nil
}

func (c *compressedConn) finishRead() error { return c.reader.Finish() }

func (c *compressedConn) Read(p []byte) (int, error) {
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
	inputPending  atomic.Bool
	utf8Accept    atomic.Bool
	authenticated atomic.Bool // reader-facing mirror; sessionState is event-loop owned
	resetDecoder  atomic.Bool
	ready         chan struct{}

	// notifyQueue and notifyUpdater are the session-scoped NOTIFY channel,
	// distinct from the selection-scoped Updater by design. See ext_d_notify.go.
	notifyQueue   *sessionUpdateQueue
	notifyUpdater *SessionUpdater
	// notifySelected is the selected-mailbox half of the registration, which
	// the framework enforces itself on the selection's update stream.
	notifySelected selectedNotifyPolicy
	// releasingNotify is set for the implicit NOOP of a NOTIFY SET.
	releasingNotify bool

	state sessionState // event-loop owned
	// executingSeqSensitive is event-loop owned, like state: it is set and
	// cleared inside execute and read only from drainUpdates.
	executingSeqSensitive bool
	// commandInProgress is the other half of RFC 3501 §7.4.1, and is event-loop
	// owned for the same reason. See expungeDeliveryDeferred.
	commandInProgress bool
	// saveSearchOnNO scopes RFC 5182 failure handling to the current handler.
	saveSearchOnNO bool
	// updateEffects remembers command-side responses whose matching update
	// batch could not yet be applied because an earlier unsolicited removal
	// remains deferred. It is event-loop owned. When that batch is eventually
	// applied, its original accounting still suppresses the duplicate response.
	updateEffects map[ChangeToken]updateAccounting
	logout        bool

	fatalMu sync.Mutex
	fatal   error

	eventCommand     atomic.Value // most recent command name, never arguments
	terminalReported atomic.Bool

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
	decoder := imapwire.NewDecoder(reader, &imapwire.Options{
		MaxLiteralSize:         c.server.options.Limits.MaxLiteralBytes,
		MaxBufferedLiteralSize: c.server.options.Limits.MaxLiteralBytes,
		MaxLineLength:          c.server.options.Limits.MaxCommandLineBytes,
		ReadTimeout:            c.decoderReadTimeout(),
	})
	decoder.SetUTF8Accept(c.utf8Accept.Load())
	return decoder
}

func (c *conn) decoderReadTimeout() time.Duration {
	readTimeout := c.server.options.Limits.ReadTimeout
	if !c.authenticated.Load() && c.server.options.Limits.PreAuthTimeout > 0 &&
		(readTimeout <= 0 || c.server.options.Limits.PreAuthTimeout < readTimeout) {
		readTimeout = c.server.options.Limits.PreAuthTimeout
	}
	return readTimeout
}

func (c *conn) serve() error {
	if timeout := c.server.options.Limits.PreAuthTimeout; timeout > 0 {
		c.preAuthTimer = time.AfterFunc(timeout, func() {
			c.reportTerminal(ServerEventTimeout, errPreAuthTimeout)
			c.cancel(errPreAuthTimeout)
			c.closeTransport()
		})
		defer c.preAuthTimer.Stop()
	}
	readerDone := make(chan error, 1)
	loopDone := make(chan error, 1)
	go func() { readerDone <- c.protect(c.readCommands) }()
	go func() { loopDone <- c.protect(c.eventLoop) }()

	err := <-loopDone
	c.cancel(err)
	c.closeTransport()
	readerErr := <-readerDone
	if fatal := c.fatalError(); fatal != nil {
		return fatal
	}
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
			var timeout net.Error
			if errors.As(err, &timeout) && timeout.Timeout() {
				command := &queuedCommand{stopErr: err, done: make(chan struct{})}
				if queueErr := c.queueCommand(command); queueErr != nil {
					return queueErr
				}
				c.waitCommandDone(command)
				return err
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

		if max := c.server.options.Limits.MaxCommands; max > 0 {
			commands++
			if commands > max {
				err := errCommandLimit
				command := &queuedCommand{stopErr: err, done: make(chan struct{})}
				if queueErr := c.queueCommand(command); queueErr != nil {
					return queueErr
				}
				c.waitCommandDone(command)
				return err
			}
		}

		descriptor := commandDescriptors[name]
		currentDescriptor = descriptor
		command := &queuedCommand{tag: tag, name: name, descriptor: descriptor, done: make(chan struct{})}
		if descriptor == nil {
			if err := discardUnknownCommand(decoder); err != nil {
				if readTimedOut(err) {
					command.stopErr = err
					if queueErr := c.queueCommand(command); queueErr != nil {
						return queueErr
					}
					c.waitCommandDone(command)
					return err
				}
				c.cancel(err)
				return err
			}
		} else {
			if name == "UID" {
				command.args, command.bytes, command.parseErr = parseUIDCommandDescriptor(decoder, func(d *commandDescriptor) { currentDescriptor = d })
			} else {
				command.args, command.bytes, command.parseErr = descriptor.parse(decoder)
			}
			if command.parseErr != nil {
				var timeout net.Error
				if errors.As(command.parseErr, &timeout) && timeout.Timeout() {
					err := command.parseErr
					command.stopErr, command.parseErr, command.args = err, nil, nil
					if queueErr := c.queueCommand(command); queueErr != nil {
						return queueErr
					}
					c.waitCommandDone(command)
					return err
				}
				fatal := decoder.Fatal()
				if !fatal {
					_ = decoder.DiscardLine()
				}
				if fatal {
					if err := c.queueCommand(command); err != nil {
						return err
					}
					if appendFamilyCommand(command) && appendLiteralTooBig(command.parseErr) {
						// Flush the tagged policy refusal before closing an unread,
						// oversized non-synchronising payload.
						c.waitCommandDone(command)
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
		// Raised before the command is queued and settled after, so the
		// interval in between is always covered by one of the two signals.
		// Queueing can block — on the byte budget, or on a full command
		// channel — and in that window the command is not yet in c.commands;
		// without the pessimistic raise the event loop would see neither it nor
		// any sign that more input follows, which is the original bug.
		//
		// The settled value is read after the whole command line is consumed:
		// reading it earlier counts the command's own unparsed arguments as
		// pipelined input. A literal-bearing command still over-reports, since
		// the payload is buffered behind the line — that direction is safe and
		// self-correcting at the next parse.
		//
		// The count is taken *before* queueing, because queueing is the moment
		// the decoder stops being exclusively ours: a command carrying a
		// literal has its payload read by the handler, on the event loop, from
		// this same bufio.Reader. Reading Buffered() afterwards is a data race,
		// which is exactly what the race detector said the first time.
		settled := make(chan struct{})
		command.readerSettled = settled
		c.inputPending.Store(true)
		pending := decoder.Buffered() > 0
		if err := c.queueCommand(command); err != nil {
			return err
		}
		c.inputPending.Store(pending)
		close(settled)
		if commandIsBarrier(command) {
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
				decoder.SetReadTimeout(c.decoderReadTimeout())
			}
		}
	}
}

func (c *conn) waitBarrier(decoder *imapwire.Decoder, command *queuedCommand) error {
	// The reader is about to park until this command completes, so it will not
	// refresh inputPending again in the meantime. Leaving it raised freezes
	// every unsolicited update for the whole of the command — and IDLE is a
	// barrier, so the client most in need of updates is the one that would
	// never get them. Lowering it here is safe: anything genuinely pipelined
	// behind the barrier is read after it, and re-raises the flag then.
	c.inputPending.Store(false)
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
				if readTimedOut(err) || appendFamilyCommand(command) && appendLiteralTooBig(err) {
					c.waitCommandDone(command)
				}
				c.cancel(err)
				return err
			}
		case <-c.ctx.Done():
			return context.Cause(c.ctx)
		}
	}
}

// waitCommandDone keeps the reader from cancelling a connection while its
// event loop is still flushing a terminal policy response. Transport write
// deadlines and the connection context bound the wait.
func (c *conn) waitCommandDone(command *queuedCommand) {
	select {
	case <-command.done:
	case <-c.ctx.Done():
	}
}

func readTimedOut(err error) bool {
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}

// Called by the event loop after a handler-owned transport read fails. The
// reader barrier waits for command.done before cancelling a fatal decoder.
func (c *conn) writeReadTimeout(err error) error {
	if writeErr := c.writeBye("connection read timeout"); writeErr != nil {
		return writeErr
	}
	return err
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
	// The pre-command drain may have deferred: the reader raises inputPending
	// before queueing IDLE and lowers it only afterwards. The update signal
	// that would wake the loop below may already have been consumed by an
	// earlier drain that deferred too, so a removal withheld across an earlier
	// FETCH, STORE or SEARCH would sit out the whole IDLE. The reader is now
	// parked in waitBarrier, which cleared inputPending before accepting this
	// request, so this drain sees the connection caught up.
	if err := c.drainUpdates(updateAccounting{}); err != nil {
		return err
	}
	for {
		select {
		case result := <-request.reply:
			if result.err != nil {
				if readTimedOut(result.err) {
					return c.writeReadTimeout(result.err)
				}
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
			c.reportTerminal(ServerEventTLSHandshakeError, err)
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
			c.authenticated.Store(c.state.state != stateNotAuthenticated)
			if wasPreAuth && c.state.state != stateNotAuthenticated {
				if c.preAuthTimer != nil {
					c.preAuthTimer.Stop()
				}
			}
			if !wasPreAuth && c.state.state == stateNotAuthenticated && c.preAuthTimer != nil {
				c.preAuthTimer.Reset(c.server.options.Limits.PreAuthTimeout)
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
	name := command.name
	if args, ok := command.args.(*uidCommandArgs); ok && args.descriptor != nil {
		name += " " + args.descriptor.name
	}
	c.eventCommand.Store(name)
	// A channel send can wake this goroutine before the reader resumes after
	// queueCommand. Wait until it publishes the settled pending-input state;
	// otherwise a fast UID command can complete while its own handoff still
	// looks like a pipeline, missing its only legal EXPUNGE delivery point.
	if command.readerSettled != nil {
		select {
		case <-command.readerSettled:
		case <-c.ctx.Done():
			return context.Cause(c.ctx)
		}
	}
	// A command is in progress from here until its tagged response, which is
	// what makes a drain in between a legal moment to deliver an expunge. Set
	// before the early returns so a BAD is not a window either. It ends at the
	// tagged response, not when this call returns — see completeCommand.
	c.commandInProgress = true
	defer func() { c.commandInProgress = false }()
	if command.stopErr != nil {
		text := "command limit exceeded"
		var timeout net.Error
		if errors.As(command.stopErr, &timeout) && timeout.Timeout() {
			text = "connection read timeout"
		}
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").RespCond(imapwire.RespCond{
			Status: "BYE", Text: imapwire.RespText{Text: text},
		}).CRLF()
		if err := c.encoder.Flush(); err != nil {
			return err
		}
		return command.stopErr
	}
	if command.parseErr != nil {
		if appendFamilyCommand(command) {
			return writeAppendDecodeError(c, command.tag, command.parseErr, "invalid command syntax")
		}
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
	// has now. Handlers decide their own drains from here on. The plain forms of
	// FETCH, STORE and SEARCH drain only after their tagged response, where no
	// command is in progress and removals wait for a later command that permits
	// them. Their UID forms and UID COPY also drain before it, where removals
	// are permitted. See drainBeforeCompletion.
	c.executingSeqSensitive = sequenceSensitiveCommands[command.name]
	var err error
	// CLOSE forbids removal responses, including changes already queued by
	// other sessions. Its selection and pending updates will be discarded.
	if command.name != "CLOSE" {
		err = c.drainUpdates(updateAccounting{})
	}
	c.executingSeqSensitive = false
	if err != nil {
		return err
	}
	// NOTIFY STATUS responses carry no sequence numbers, so any command start
	// may deliver them. Draining here keeps a poll such as NOOP from completing
	// ahead of events already queued.
	if err := c.drainNotify(); err != nil {
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
		if request.descriptor.name == "APPEND" || request.descriptor.name == "REPLACE" {
			err = resolvedContentTooBig()
		} else {
			err = fmt.Errorf("literal exceeds the connection limit")
		}
	} else if request.info.NonSynchronising && request.info.Size > maxLiteralMinusSize &&
		!slices.Contains(connCapabilities(c), "LITERAL+") {
		err = &imap.Error{Type: imap.ErrorTypeBad, Code: imap.CodeTooBig, Text: "non-synchronising literal exceeds the LITERAL- limit"}
	} else if !request.info.NonSynchronising {
		c.encoder.BeginResponse(imapwire.ResponseContinuation, "").ContinuationText("ready for literal").CRLF()
		err = c.encoder.Flush()
	}
	request.reply <- err
}

func (c *conn) writeGreeting() error {
	capabilities := connCapabilities(c)
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
	c.writeDeferredModSeqFence()
	c.finishSearchSave(status)
	c.completeCommand()
	c.encoder.BeginResponse(imapwire.ResponseTagged, tag).RespCond(imapwire.RespCond{Status: status, Text: imapwire.RespText{Text: text}}).CRLF()
	return c.encoder.Flush()
}

// completeCommand ends the command in progress. Every tagged response is
// written through a caller of this, because the tagged response is where
// RFC 3501 §7.4.1's "in progress" ends: the client may send its next command
// the moment it reads it, and imaptest does, before it reads anything else.
// An EXPUNGE written after it can therefore arrive while that next command is
// in progress, which is the window the section exists to close.
//
// Ending it when the handler returns instead left a drain after the tagged
// response — FETCH, STORE and SEARCH all have one — free to deliver a removal
// whenever the reader had not yet seen the next command. The queue cannot see
// a command still on the wire. That was #69.
func (c *conn) completeCommand() {
	c.commandInProgress = false
}

// drainBeforeCompletion is the in-progress delivery point of FETCH, STORE,
// SEARCH, SORT and THREAD: after their untagged responses, before their tagged
// one. The plain forms retain removals while delivering additions and flags.
// §7.4.1 forbids an EXPUNGE while responding to the plain forms, and RFC 9051
// §7.5.1 permits one "during a UID command"; imaptest classifies UID FETCH the
// same way.
//
// Without it the UID forms would have no legal delivery point at all: their
// pre-command drain defers because "UID" is sequence-sensitive, and the drain
// after their tagged response runs once no command is in progress. A client
// that polls only with UID FETCH or UID SEARCH, which RFC 5465 §5.3 tells
// NOTIFY clients to do, would never be told of an expunge or of anything
// queued behind one.
func (c *conn) drainBeforeCompletion(command *queuedCommand, accounting updateAccounting) error {
	if !commandUsesUIDs(command) {
		// Apply additions and flags before completion, retaining the old
		// sequence positions of removals the command may not announce.
		c.executingSeqSensitive = true
		defer func() { c.executingSeqSensitive = false }()
	}
	return c.drainUpdates(accounting)
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
		c.reportTerminal(ServerEventTLSHandshakeError, err)
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

func (c *conn) writeBye(text string) error {
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").RespCond(imapwire.RespCond{
		Status: "BYE", Text: imapwire.RespText{Text: text},
	}).CRLF()
	return c.encoder.Flush()
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

// bufferedConn preserves plaintext read ahead by the compression layer when
// UNAUTHENTICATE returns to the underlying transport. It retains TLS, if any.
type bufferedConn struct {
	net.Conn
	reader io.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *conn) disableCompression() {
	compressed, ok := c.currentTransport().(*compressedConn)
	if !ok {
		return
	}
	if compressed.reader != nil {
		_ = compressed.reader.Close()
	}
	transport := &bufferedConn{Conn: compressed.Conn, reader: compressed.reader.PlainReader()}
	c.transportMu.Lock()
	c.transport = transport
	c.transportMu.Unlock()
	c.encoder = c.newEncoder(transport)
	c.state.compressed = false
	c.resetDecoder.Store(true)
}

func (c *conn) cleanupBackend() {
	if selected := c.state.unselect(); selected != nil {
		selected.close()
		ctx, cancel := context.WithTimeout(context.Background(), c.server.options.Limits.CommandTimeout)
		if err := selected.mailbox.Unselect(ctx, nil); err != nil {
			c.reportCleanupError("UNSELECT", err)
		}
		cancel()
	}
	if session := c.state.session; session != nil {
		ctx, cancel := context.WithTimeout(context.Background(), c.server.options.Limits.CommandTimeout)
		if err := session.Close(ctx, nil); err != nil {
			c.reportCleanupError("LOGOUT", err)
		}
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
// Deferral is for *unsolicited* removals only, and that exemption is decided
// per batch by the caller rather than here. It used to be decided per drain: a
// drain carrying any origin or effect returned early, which disabled every
// condition below for the whole queue. STORE's own drain then delivered
// unrelated sessions' expunges while responding to STORE with two commands
// pipelined behind it — §7.4.1's forbidden window, reached through the
// exemption meant for the command's own changes. See drainUpdates.
func (c *conn) expungeDeliveryDeferred() bool {
	// NOTIFY's SELECTED specifier asks for removals as they happen; a client
	// registering it has stopped using sequence numbers in commands (RFC 5465
	// sections 5.3 and 6.1.1). SELECTED-DELAYED keeps the rules below.
	if c.notifySelected.immediateRemovals() {
		return false
	}
	// §7.4.1 opens with the condition this missed for a release: "An EXPUNGE
	// response MUST NOT be sent when no command is in progress". Between
	// commands the server knows nothing about what the client has already put on
	// the wire — inputPending sees bytes the reader has buffered, never a
	// command still in flight — so the gap is exactly where a client numbering
	// against the pre-expunge view cannot be detected. Delivering while a
	// command is in progress is what closes it: the client sees the EXPUNGE
	// before that command's tagged response, so anything it sends afterwards is
	// numbered against the new view, and anything it pipelined beforehand is
	// caught by the two conditions below. "Between commands" starts at the
	// tagged response, not when the handler returns — the drain FETCH, STORE
	// and SEARCH run after their tagged OK is in that gap. See completeCommand.
	//
	// IDLE is not an exception to this, it is an instance of it: the IDLE
	// command is in progress for as long as the client is idling, which is why
	// the extension can deliver expunges at all.
	if !c.commandInProgress {
		return true
	}
	// Three ways to still be behind the client: a command parsed and waiting,
	// bytes read but not yet parsed, or the command about to run being one
	// whose numbering the client is relying on. The last keeps a pipelined
	// FETCH, STORE or SEARCH from being renumbered by the drain just before it.
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

type updateDrainMode int

const (
	// drainModeDeferred honours RFC 3501 §7.4.1: unsolicited removals wait.
	drainModeDeferred updateDrainMode = iota
	// drainModeThroughOrigin applies the prefix ending at the command's own
	// batch, including otherwise-deferred removals ahead of it.
	drainModeThroughOrigin
	// drainModeAllowRemovals applies every currently queued batch. EXPUNGE and
	// MOVE may send EXPUNGE responses, so a later pipelined command must not
	// keep an older removal (and every ADD stuck behind it) deferred across
	// those handlers.
	drainModeAllowRemovals
)

func (c *conn) drainUpdates(accounting updateAccounting) error {
	return c.drainUpdatesInternal(accounting, drainModeDeferred)
}

// drainUpdatesThrough applies the revision prefix through accounting.origin,
// even when unsolicited removals would otherwise be deferred. Commands that
// themselves remove messages need that prefix atomically: their backend writer
// reports UIDs from the current backend revision, and only the ordered batches
// can convert those removals to the client's sequence-number view correctly.
func (c *conn) drainUpdatesThrough(accounting updateAccounting) error {
	return c.drainUpdatesInternal(accounting, drainModeThroughOrigin)
}

// drainUpdatesAllowingRemovals applies the whole current queue. Callers must
// already be in a command that §7.4.1 permits to emit EXPUNGE.
func (c *conn) drainUpdatesAllowingRemovals(accounting updateAccounting) error {
	return c.drainUpdatesInternal(accounting, drainModeAllowRemovals)
}

func (c *conn) drainUpdatesInternal(accounting updateAccounting, mode updateDrainMode) error {
	selected := c.state.selected
	if selected == nil || selected.queue == nil {
		return nil
	}
	// Every origin a command of this connection drains with is recorded, even
	// without an effect: it is also what identifies the command's own
	// batches, whose changes are command results that NOTIFY never filters.
	if accounting.origin != 0 {
		if c.updateEffects == nil {
			c.updateEffects = make(map[ChangeToken]updateAccounting)
		}
		c.updateEffects[accounting.origin] = accounting
	}
	// NOTIFY's selected policy decides what other sessions' changes may
	// announce. A NOTIFY SET's implicit NOOP releases everything accumulated
	// under the previous registration first (RFC 5465 section 3.1).
	suppressMessages := c.notifySelected.suppressesMessages() && !c.releasingNotify
	suppressFlags := c.notifySelected.suppressesFlags() && !c.releasingNotify
	// Revisions always advance in order. Only removal announcements and the
	// corresponding client sequence-number changes wait; later EXISTS and
	// FLAGS updates use that unchanged client view.
	deferred := mode == drainModeDeferred && c.expungeDeliveryDeferred()
	var batches []*UpdateBatch
	if mode == drainModeThroughOrigin {
		batches = selected.queue.popThroughOrigin(accounting.origin)
	} else {
		batches = selected.queue.popAll()
	}
	// Keep accounting only for origins not yet applied. Withheld removals have
	// already passed origin accounting and do not retain STORE command entries.
	queuedOrigins := selected.queue.origins()
	defer func() {
		for origin := range c.updateEffects {
			if _, queued := queuedOrigins[origin]; !queued {
				delete(c.updateEffects, origin)
			}
		}
	}()
	releasable := !suppressFlags && len(selected.pendingFlags) != 0 || !suppressMessages && (len(selected.pendingAdds) != 0 || selected.hasPendingRecent || !deferred && len(selected.suppressed) != 0)
	if len(batches) == 0 && !releasable && (deferred || len(selected.withheld) == 0) {
		return nil
	}
	// removed collects the expunged UIDs so CONTEXT registrations can be told
	// which of their matches went away. See ext_e_context.go.
	removed := make(map[imap.UID]imap.SeqNum)
	beforeSequences := make(map[imap.UID]imap.SeqNum)
	if len(selected.contexts) > 0 {
		for i, uid := range selected.uids {
			beforeSequences[uid] = imap.SeqNum(i + 1)
		}
	}
	var pending []deliveredUpdate
	if !suppressMessages {
		pending = selected.releaseSuppressed(!deferred)
	}
	if !deferred && len(selected.withheld) != 0 {
		if suppressMessages {
			selected.suppressWithheld()
		} else {
			delivered, err := selected.deliverWithheldRemovals()
			if err != nil {
				c.failFatal(err)
				return err
			}
			pending = append(pending, delivered...)
		}
	}
	for _, batch := range batches {
		batchAccounting, own := c.updateEffects[batch.Origin]
		if !own {
			batchAccounting = updateAccounting{origin: batch.Origin}
		}
		if batch.Origin != 0 && batch.Origin == accounting.origin {
			batchAccounting, own = accounting, true
		}
		updates, err := selected.applyBatchWith(batch, batchAccounting, batchDelivery{
			deferRemovals:    deferred,
			suppressMessages: suppressMessages && !own,
			suppressFlags:    suppressFlags && !own,
		})
		if err != nil {
			// Once an accepted batch cannot be applied, the framework no longer
			// knows the selected mailbox's sequence view. Continuing would risk
			// emitting responses against a corrupt mapping.
			c.failFatal(err)
			return err
		}
		pending = append(pending, updates...)
	}
	if !suppressFlags {
		pending = append(pending, selected.releaseFlags()...)
	}
	pending, err := c.completeUpdateFlags(pending)
	if err != nil {
		return err
	}
	wireUpdates := coalesceWireUpdates(pending)
	for _, update := range wireUpdates {
		if update.kind == updateMessageExpunge {
			removed[update.uid] = beforeSequences[update.uid]
		} else if update.kind == updateMessageVanished {
			// Only registered matches need individual notification; do not
			// expand a possibly enormous VANISHED UID range.
			for _, held := range selected.contexts {
				for uid := range held.uids {
					if update.uids.Contains(uid) {
						removed[uid] = beforeSequences[uid]
					}
				}
			}
		}
	}
	// Contexts report expunged matches before EXPUNGE renumbers sequence IDs,
	// and every other change after the FETCH or EXISTS that caused it.
	deltas := refreshSearchContexts(c, removed, beforeSequences)
	if err := writeSearchContextDeltas(c, deltas, true); err != nil {
		return err
	}
	for _, update := range wireUpdates {
		if err := c.writeUpdate(update); err != nil {
			return err
		}
	}
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	return writeSearchContextDeltas(c, deltas, false)
}

// RFC 7162 sections 3.2 and 6 require an explicit safe checkpoint when
// higher per-message MODSEQs pass an unreported flag change or removal.
// A client MUST prefer this code over larger FETCH values at completion.
func (c *conn) writeDeferredModSeqFence() {
	selected := c.state.selected
	if selected == nil || !condStoreMailboxEnabled(c) {
		return
	}
	// A backend may emit a FETCH and then fail before the handler's normal
	// drain. Its already-published changes still need to fence that response.
	if !c.hasUnreportedChanges() {
		return
	}
	writeUntaggedOK(c, imap.CodeHighestModSeq, fmt.Sprint(max(uint64(1), selected.snapshotModSeq)), "change announcements pending")
}

// hasUnreportedChanges also checks queued changes: a backend may publish
// and then fail before the handler drains its updates.
func (c *conn) hasUnreportedChanges() bool {
	s := c.state.selected
	return s != nil && (s.withholdsRemovals() || s.queue.hasRemovals() ||
		len(s.pendingFlags) != 0 || s.queue.hasFlags())
}

func (c *conn) writeUpdate(update deliveredUpdate) error {
	update.flags = wireFlags(c, update.flags)
	switch update.kind {
	case updateExists:
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Number(update.exists).SP().Atom("EXISTS").CRLF()
	case updateRecent:
		if c.state.revision != revisionIMAP4rev2 {
			c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Number(update.exists).SP().Atom("RECENT").CRLF()
		}
	case updateMailboxFlags:
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("FLAGS").SP().List(len(update.flags), func(i int) {
			c.encoder.Flag(string(update.flags[i]))
		}).CRLF()
	case updateMessageFlags:
		data := &imap.FetchMessageData{
			SeqNum: update.seqNum,
			Items: map[imap.FetchDataKey][]imap.FetchData{
				imap.FetchDataKey("FLAGS"): {imap.FetchDataFlags(update.flags)},
			},
		}
		if update.modSeq != 0 && condStoreMailboxEnabled(c) {
			data.Items[imap.FetchDataKey("MODSEQ")] = []imap.FetchData{imap.FetchDataModSeq(update.modSeq)}
		}
		if err := ensureFetchFlagsAdvertised(c, data); err != nil {
			return err
		}
		// UIDONLY reshapes unilateral responses too: a client that enabled it
		// has discarded the machinery for interpreting a sequence number, so
		// one arriving unsolicited is worse than none at all.
		// See ext_d_uidonly.go.
		if uidOnlyEnabled(c) {
			return imapcodec.WriteUIDFetchResponse(c.encoder, update.uid, data, nil)
		}
		// Otherwise the UID is a data item: RFC 7162 section 3.2.4 requires it
		// once CONDSTORE is enabled, and RFC 5465 section 5.1 in a FlagChange
		// notification for the selected mailbox, with or without CONDSTORE.
		if condStoreRequiresUID(c) || c.notifySelected.flagChangeRegistered() {
			data.Items[imap.FetchDataKey("UID")] = []imap.FetchData{imap.FetchDataUID(update.uid)}
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

// UpdateFlags permits an unavailable MODSEQ. Once CONDSTORE is active the
// wire requires it. Fetch FLAGS and MODSEQ together, once for the whole drain,
// and replace every pending flag report for these UIDs with that snapshot.
// Otherwise a later report in this drain could overwrite the fetched state
// with older flags at a lower MODSEQ.
func (c *conn) completeUpdateFlags(updates []deliveredUpdate) ([]deliveredUpdate, error) {
	if !condStoreMailboxEnabled(c) {
		return updates, nil
	}
	type currentFlags struct {
		flags    []imap.Flag
		modSeq   uint64
		hasFlags bool
	}
	current := make(map[imap.UID]*currentFlags)
	var missing []imap.UID
	for _, update := range updates {
		if update.kind == updateMessageFlags && update.modSeq == 0 && current[update.uid] == nil {
			current[update.uid] = &currentFlags{}
			missing = append(missing, update.uid)
		}
	}
	if len(missing) == 0 {
		return updates, nil
	}
	ctx, cancel := context.WithTimeout(c.ctx, c.server.options.Limits.CommandTimeout)
	defer cancel()
	writer := newFetchWriter(func(_ context.Context, data *imap.FetchMessageData) error {
		uid, ok := extractFetchUID(data)
		flags := current[uid]
		if !ok || flags == nil {
			return fmt.Errorf("imapserver: flag-update FETCH returned an unexpected UID")
		}
		for _, values := range data.Items {
			for _, value := range values {
				switch value := value.(type) {
				case imap.FetchDataFlags:
					flags.flags = slices.Clone([]imap.Flag(value))
					flags.hasFlags = true
				case imap.FetchDataModSeq:
					flags.modSeq = uint64(value)
				}
			}
		}
		return nil
	})
	err := c.state.selected.mailbox.Fetch(ctx, writer, imap.UIDSetNum(missing...), &FetchOptions{
		Items: []imap.FetchItem{imap.FetchItemUID, imap.FetchItemFlags, imap.FetchItemModSeq},
	})
	writer.core.close()
	if err == nil {
		for _, flags := range current {
			if flags.hasFlags != (flags.modSeq != 0) {
				err = fmt.Errorf("imapserver: flag-update FETCH omitted FLAGS or MODSEQ")
				break
			}
		}
	}
	if err != nil {
		c.reportBackendError("FETCH", err)
		// A missing CONDSTORE clock cannot be announced safely. End the
		// session explicitly so the client knows it must resynchronise.
		_ = c.writeBye("unable to complete mailbox updates")
		return nil, err
	}
	kept := updates[:0]
	for _, update := range updates {
		if update.kind == updateMessageFlags {
			if flags := current[update.uid]; flags != nil {
				// FETCH skips a concurrently removed UID. Its removal update
				// supplies the announcement instead of stale FLAGS.
				if !flags.hasFlags {
					continue
				}
				update.flags, update.modSeq = flags.flags, flags.modSeq
			}
		}
		kept = append(kept, update)
	}
	return kept, nil
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
	c.reportTerminal(ServerEventConnectionLimit, ErrUpdateOverflow)
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
