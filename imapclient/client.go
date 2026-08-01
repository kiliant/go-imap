package imapclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// Options configures a [Client]. The zero value is secure and valid; a nil
// *Options selects the same defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type Options struct {
	// TLSConfig supplies additional TLS settings for DialTLS and DialStartTLS.
	// It is cloned before use. TLS 1.2 is always the minimum and ServerName is
	// inferred from the dial address when this field leaves it empty.
	TLSConfig *tls.Config

	// InsecureSkipVerify disables TLS certificate verification. It is false by
	// default and must only be used for controlled test servers.
	InsecureSkipVerify bool

	// AllowInsecureAuth permits PLAIN and LOGIN authentication over a
	// cleartext connection. It is false by default; prefer DialTLS or
	// DialStartTLS instead.
	AllowInsecureAuth bool

	// UnilateralData receives connection-scoped untagged data. A nil handler is
	// valid. Its callbacks run on the reader goroutine and must not block.
	UnilateralData *UnilateralDataHandler

	// DebugWriter receives a serialized, redacted protocol summary. It is not a
	// byte-for-byte wire trace; credentials and authentication challenges are
	// never written to it.
	DebugWriter io.Writer

	// Trace receives serialized, redacted protocol-summary events. It is not a
	// byte-for-byte wire trace. It is called synchronously by the reader or
	// writer goroutine and must not block.
	Trace func(TraceEvent)

	// IdleTimeout is the maximum duration of one IDLE command. A positive value
	// makes Idle re-issue IDLE before the server's 29 minute limit; zero uses
	// the conservative 25 minute default. Values at or above 29 minutes are
	// clamped to that default.
	IdleTimeout time.Duration

	// IdlePollInterval controls the NOOP polling interval used by Idle when the
	// server does not advertise IDLE. Zero uses one minute. Polling cannot offer
	// push latency, so applications that need prompt notification should use a
	// server with IDLE support.
	IdlePollInterval time.Duration

	// ReadTimeout is the maximum time an underlying network read may make no
	// progress. Zero or a negative value uses the 30 minute default. The deadline
	// is refreshed as progress is made, so large streaming literals may take
	// longer as long as the server continues sending data.
	ReadTimeout time.Duration

	// WriteTimeout is the maximum time an underlying network write may make no
	// progress. Zero or a negative value uses the 5 minute default. The deadline
	// is refreshed as each write completes, so a large streaming APPEND may take
	// longer as long as the server keeps consuming it.
	//
	// It bounds a server that stops reading, which would otherwise block a
	// command in the kernel send buffer for as long as the connection stayed
	// open. A write that does time out desynchronises the stream and therefore
	// poisons the session; it is not recoverable.
	WriteTimeout time.Duration

	// MaxUntaggedResponses is the largest number of command-scoped untagged
	// responses retained for one command. Zero or a negative value uses the
	// default of 4096. Streaming FETCH responses are delivered directly and are
	// therefore not counted as retained responses.
	MaxUntaggedResponses int
}

const defaultReadTimeout = 30 * time.Minute

// defaultWriteTimeout is shorter than defaultReadTimeout because the two bound
// different things. A read may legitimately make no progress for a long time —
// that is what an idle session looks like. A write making no progress means the
// server has stopped draining its receive window, which no healthy server does
// for minutes at a time.
const defaultWriteTimeout = 5 * time.Minute

// TraceDirection identifies whether a trace event came from the client or the
// server.
type TraceDirection string

const (
	// TraceClient is a redacted command sent to the server.
	TraceClient TraceDirection = "client"
	// TraceServer is a response summary received from the server.
	TraceServer TraceDirection = "server"
)

// TraceEvent is one redacted protocol-summary event, not raw wire data.
//
// Construct with keyed fields only; fields may be added in a future release.
type TraceEvent struct {
	// Direction identifies the endpoint that produced this summary.
	Direction TraceDirection
	// Data is the redacted protocol summary, not raw wire bytes.
	Data string
	_    struct{}
}

// UnilateralDataHandler receives untagged data that no in-flight command
// claims. It is a struct of callbacks rather than an interface so future
// unsolicited response types can be added without breaking implementations.
//
// Construct with keyed fields only; fields may be added in a future release.
type UnilateralDataHandler struct {
	// Exists receives the updated message count for the selected mailbox.
	Exists func(numMessages uint32)
	// Expunge receives the sequence number removed from the selected mailbox.
	Expunge func(seqNum uint32)
	// Recent receives the updated count of recent messages.
	Recent func(numRecent uint32)
	// Fetch receives an unsolicited FETCH update, currently flag updates.
	Fetch func(data *imap.FetchMessageData)
	// Vanished receives UIDs removed from the selected mailbox after
	// ENABLE QRESYNC. Earlier is true for VANISHED (EARLIER), which reports
	// expunges that happened while disconnected and does not renumber
	// sequence numbers. See [VanishedData].
	Vanished func(data VanishedData)
	_        struct{}
}

// Client is an IMAP session. Its zero value is not usable; obtain one with
// [Dial], [DialTLS], [DialStartTLS], or [NewClient].
type Client struct {
	mu sync.Mutex

	// writeMu serialises command serialisation, and is deliberately not mu.
	// Writing a synchronising literal blocks until the server sends its
	// continuation request, and the reader goroutine needs mu to deliver
	// anything at all — the untagged responses that may precede the
	// continuation, the tagged rejection that can replace it, and the close
	// that cancellation triggers. Holding mu across a write therefore
	// deadlocks the whole session.
	//
	// Lock order is continuationOwnerMu, then writeMu, then mu.
	writeMu sync.Mutex

	// authInProgress excludes unrelated commands while an AUTHENTICATE
	// exchange is consuming continuation requests. IMAP cannot safely
	// disambiguate a continuation once another command is pipelined beside it.
	authInProgress bool

	// continuationMu is deliberately separate from mu. A synchronising
	// literal is written while writeMu protects the encoder, while the reader
	// must be able to deliver its continuation without waiting for that write
	// lock.
	continuationMu sync.Mutex
	continuation   *continuationHandler

	// continuationOwnerMu serialises the installation of continuation
	// handlers. AUTHENTICATE and IDLE own the slot across several responses
	// and claim it before issuing their command; every other command installs
	// a handler only for the duration of its own serialisation.
	continuationOwnerMu sync.Mutex

	traceMu sync.Mutex

	conn net.Conn
	opts Options
	dec  *imapwire.Decoder
	enc  *imapwire.Encoder

	state   State
	caps    map[string]struct{}
	enabled map[string]struct{}
	idle    *IdleCommand

	nextTag  uint64
	pending  map[string]*Command
	pendingQ []*Command
	closed   bool
	closeErr error

	greeting     chan struct{}
	greetingOnce sync.Once
	greetingErr  error

	// A STARTTLS tagged OK pauses the reader before any TLS bytes are read.
	pauseTag   string
	paused     chan struct{}
	resume     chan struct{}
	resumeOnce sync.Once
	readerDone chan struct{}

	// mailboxUIDValidity retains the last UIDVALIDITY seen for each selected
	// mailbox.  A changed value invalidates every cached UID for that mailbox,
	// so it is deliberately connection state rather than an incidental field on
	// a SELECT response.
	mailboxUIDValidity map[string]uint32
	selectedMailbox    string
}

type continuationHandler struct{ fn func(string) error }

// setContinuation installs the sole handler for a server continuation request.
// IMAP continuations are ordered with commands, so only one may be outstanding.
// The returned function removes this particular handler and is safe to call
// after a replacement has been installed.
func (c *Client) setContinuation(fn func(string) error) (clear func()) {
	h := &continuationHandler{fn: fn}
	c.continuationMu.Lock()
	c.continuation = h
	c.continuationMu.Unlock()
	return func() {
		c.continuationMu.Lock()
		if c.continuation == h {
			c.continuation = nil
		}
		c.continuationMu.Unlock()
	}
}

// setContinuationIfUnset installs fn only when no handler is currently
// installed, reporting whether it did. A command that owns the whole
// continuation exchange — AUTHENTICATE, IDLE — installs its handler before
// issuing the command, and keeps it: the generic per-command handler must not
// displace it.
func (c *Client) setContinuationIfUnset(fn func(string) error) (clear func(), installed bool) {
	h := &continuationHandler{fn: fn}
	c.continuationMu.Lock()
	if c.continuation != nil {
		c.continuationMu.Unlock()
		return func() {}, false
	}
	c.continuation = h
	c.continuationMu.Unlock()
	return func() {
		c.continuationMu.Lock()
		if c.continuation == h {
			c.continuation = nil
		}
		c.continuationMu.Unlock()
	}, true
}

func (c *Client) deliverContinuation(text string) error {
	c.continuationMu.Lock()
	h := c.continuation
	c.continuationMu.Unlock()
	if h == nil {
		return fmt.Errorf("unexpected continuation request")
	}
	return h.fn(text)
}

// Command is an issued IMAP command. It may be waited on exactly as often as
// needed; all callers observe the same completion result.
type Command struct {
	client *Client
	tag    string
	name   string

	done chan struct{}
	once sync.Once
	err  error

	collector  commandCollector
	onComplete taggedCompleteFunc
}

// taggedCompleteFunc is invoked on the reader goroutine with the tagged
// completion of a command, before Wait unblocks. On success, code and args are
// the resp-text-code and its arguments (empty when the server sent none). On
// failure they are empty; the error path already carries Code/CodeArgs on
// [*imap.Error].
type taggedCompleteFunc func(success bool, code, args string)

// Tag reports the command's unique client-generated tag.
func (cmd *Command) Tag() string { return cmd.tag }

// Wait blocks for the tagged completion of cmd. If ctx is cancelled while cmd
// is still in flight, the entire connection is closed because IMAP cannot
// abort an arbitrary command safely. It returns ctx.Err in that case.
func (cmd *Command) Wait(ctx context.Context) error {
	if cmd == nil {
		return errors.New("imapclient: nil command")
	}
	select {
	case <-cmd.done:
		return cmd.err
	default:
	}
	select {
	case <-cmd.done:
		return cmd.err
	case <-ctx.Done():
		select {
		case <-cmd.done:
			return cmd.err
		default:
			cmd.client.poison(ctx.Err())
			return ctx.Err()
		}
	}
}

func (cmd *Command) complete(err error) {
	cmd.once.Do(func() {
		cmd.err = err
		close(cmd.done)
	})
}

// NewClient starts an IMAP session on conn. It returns immediately; use
// [Client.WaitGreeting] when constructing a client around an existing
// connection and the greeting result matters. Dial helpers always wait for the
// greeting before returning.
func NewClient(conn net.Conn, opts *Options) *Client {
	o := Options{}
	if opts != nil {
		o = *opts
	}
	wopts := o.wireOptions()
	eopts := o.encoderOptions()
	c := &Client{
		conn:               conn,
		opts:               o,
		dec:                imapwire.NewDecoder(conn, &wopts),
		enc:                imapwire.NewEncoder(conn, &eopts),
		state:              StateNotAuthenticated,
		caps:               make(map[string]struct{}),
		enabled:            make(map[string]struct{}),
		pending:            make(map[string]*Command),
		greeting:           make(chan struct{}),
		readerDone:         make(chan struct{}),
		mailboxUIDValidity: make(map[string]uint32),
	}
	go c.readResponses()
	return c
}

func (o Options) wireOptions() imapwire.Options {
	readTimeout := o.ReadTimeout
	if readTimeout <= 0 {
		readTimeout = defaultReadTimeout
	}
	maxUntagged := o.MaxUntaggedResponses
	if maxUntagged <= 0 {
		maxUntagged = imapwire.DefaultMaxUntaggedPerCommand
	}
	return imapwire.Options{
		ReadTimeout:           readTimeout,
		MaxUntaggedPerCommand: maxUntagged,
	}
}

// encoderOptions carries the settings that survive an encoder being rebuilt.
// LiteralPlus, LiteralMinus and WaitContinuation are deliberately absent: they
// are re-applied per command from the current capabilities. WriteTimeout is
// not, so it has to be established at construction wherever an encoder is made.
func (o Options) encoderOptions() imapwire.EncoderOptions {
	writeTimeout := o.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = defaultWriteTimeout
	}
	return imapwire.EncoderOptions{WriteTimeout: writeTimeout}
}

func (c *Client) maxUntaggedResponses() int {
	return c.opts.wireOptions().MaxUntaggedPerCommand
}

func countUntaggedResponse(count *int, limit int, command string) error {
	if *count >= limit {
		return fmt.Errorf("too many untagged responses for %s (limit %d)", command, limit)
	}
	*count = *count + 1
	return nil
}

// WaitGreetingOptions configures [Client.WaitGreeting]. A nil pointer selects
// the defaults.
//
// It carries no fields today; it keeps the door open for future greeting
// handling policy without a signature change.
//
// Construct with keyed fields only; fields may be added in a future release.
type WaitGreetingOptions struct {
	_ struct{}
}

// WaitGreeting waits until the server greeting has been parsed. A nil options
// pointer selects the defaults.
func (c *Client) WaitGreeting(ctx context.Context, options *WaitGreetingOptions) error {
	select {
	case <-c.greeting:
		return c.greetingErr
	case <-ctx.Done():
		c.poison(ctx.Err())
		return ctx.Err()
	}
}

// State reports the current IMAP session state.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// NoopOptions configures NOOP. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type NoopOptions struct {
	_ struct{}
}

// Noop issues NOOP and returns its command handle after writing its bounded
// command prelude, without waiting for the server response. A nil options
// pointer selects the defaults.
func (c *Client) Noop(options *NoopOptions) *Command {
	return c.beginCommand("NOOP", stateNotAuthenticated|stateAuthenticated|stateSelected, nil, nil)
}

// LogoutOptions configures [Client.Logout]. A nil pointer selects the
// defaults.
//
// It carries no fields today; it keeps the door open for options such as
// declining to wait for the tagged completion, without a signature change.
//
// Construct with keyed fields only; fields may be added in a future release.
type LogoutOptions struct {
	_ struct{}
}

// Logout asks the server to end the session, waits for its tagged completion,
// and closes the transport. ctx must be non-nil. A nil options pointer selects
// the defaults.
func (c *Client) Logout(ctx context.Context, options *LogoutOptions) error {
	cmd := c.beginCommand("LOGOUT", stateNotAuthenticated|stateAuthenticated|stateSelected, nil, nil)
	err := cmd.Wait(ctx)
	closeErr := c.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// Close immediately closes the transport. Pending commands complete with the
// close error. It is safe to call more than once.
func (c *Client) Close() error {
	return c.closeWith(net.ErrClosed)
}

func (c *Client) closeWith(err error) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.closeErr = err
	c.state = StateLogout
	pending := c.pendingQ
	c.pendingQ = nil
	c.pending = make(map[string]*Command)
	conn := c.conn
	c.mu.Unlock()
	for _, cmd := range pending {
		cmd.complete(err)
	}
	if conn != nil {
		return conn.Close()
	}
	return nil
}

func (c *Client) poison(err error) { _ = c.closeWith(err) }

func (c *Client) setGreeting(err error) {
	c.greetingOnce.Do(func() {
		c.greetingErr = err
		close(c.greeting)
	})
}

func (c *Client) trace(direction TraceDirection, data string) {
	c.traceMu.Lock()
	defer c.traceMu.Unlock()
	event := TraceEvent{Direction: direction, Data: data}
	if c.opts.DebugWriter != nil {
		_, _ = fmt.Fprintf(c.opts.DebugWriter, "%s: %s\n", direction, data)
	}
	if c.opts.Trace != nil {
		c.opts.Trace(event)
	}
}

// commandOptions carries the per-command knobs of [Client.issue]. It is
// internal; new knobs are added here rather than as further positional
// parameters on the command helpers.
type commandOptions struct {
	allowed    stateMask
	write      func(*imapwire.Encoder)
	collector  commandCollector
	onComplete taggedCompleteFunc

	// ownsContinuation is set by AUTHENTICATE and IDLE, which install their
	// own continuation handler before issuing the command and keep it for the
	// remainder of the exchange.
	ownsContinuation bool
}

func (c *Client) beginCommand(name string, allowed stateMask, write func(*imapwire.Encoder), collector commandCollector) *Command {
	return c.issue(name, commandOptions{allowed: allowed, write: write, collector: collector})
}

// beginAuthenticationCommand reserves the connection for LOGIN or
// AUTHENTICATE until its tagged completion. A SASL continuation has no command
// tag, so allowing another command to pipeline beside it would be ambiguous.
// ownsContinuation is set for AUTHENTICATE, which installed its SASL handler
// before issuing the command; LOGIN has no continuation exchange of its own and
// uses the generic handler so that a credential needing a literal works.
func (c *Client) beginAuthenticationCommand(name string, write func(*imapwire.Encoder), collector commandCollector, ownsContinuation bool) *Command {
	c.mu.Lock()
	if c.authInProgress {
		c.mu.Unlock()
		cmd := &Command{client: c, name: name, done: make(chan struct{})}
		cmd.complete(&imap.Error{Type: imap.ErrorTypeProtocol, Text: "authentication is already in progress"})
		return cmd
	}
	c.authInProgress = true
	c.mu.Unlock()

	finished := func(success bool, _, _ string) {
		c.mu.Lock()
		c.authInProgress = false
		c.mu.Unlock()
		if success {
			c.authenticationSucceeded()
		}
	}
	cmd := c.issue(name, commandOptions{
		allowed:          stateNotAuthenticated,
		write:            write,
		collector:        collector,
		onComplete:       finished,
		ownsContinuation: ownsContinuation,
	})
	select {
	case <-cmd.done:
		// A local validation or write failure has no tagged completion to run
		// the callback above.
		c.mu.Lock()
		c.authInProgress = false
		c.mu.Unlock()
	default:
	}
	return cmd
}

// beginCommandWithCompletion is the command primitive for commands whose
// successful tagged completion changes session state or finalises collected
// response data.  The callback always runs on the reader goroutine before the
// command is made visible as complete.
func (c *Client) beginCommandWithCompletion(name string, allowed stateMask, write func(*imapwire.Encoder), collector commandCollector, onComplete taggedCompleteFunc) *Command {
	return c.issue(name, commandOptions{allowed: allowed, write: write, collector: collector, onComplete: onComplete})
}

// issue validates, registers and serialises one command.
//
// Only the validation and registration run under mu; serialisation runs under
// writeMu alone, because a command argument that needs a synchronising literal
// blocks here until the server answers, and the reader goroutine must stay free
// to deliver that answer.
func (c *Client) issue(name string, opts commandOptions) *Command {
	cmd := &Command{client: c, name: name, done: make(chan struct{}), collector: opts.collector, onComplete: opts.onComplete}

	if !opts.ownsContinuation {
		c.continuationOwnerMu.Lock()
		defer c.continuationOwnerMu.Unlock()
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		if err == nil {
			err = net.ErrClosed
		}
		c.mu.Unlock()
		cmd.complete(err)
		return cmd
	}
	if c.authInProgress && name != "AUTHENTICATE" && name != "LOGIN" {
		c.mu.Unlock()
		cmd.complete(&imap.Error{Type: imap.ErrorTypeProtocol, Text: "command is not valid while authentication is in progress"})
		return cmd
	}
	if c.idle != nil && name != "IDLE" {
		c.mu.Unlock()
		cmd.complete(&imap.Error{Type: imap.ErrorTypeProtocol, Text: "command is not valid while IDLE is active"})
		return cmd
	}
	if pipelineConflict(name, c.pendingQ) {
		c.mu.Unlock()
		cmd.complete(&imap.Error{Type: imap.ErrorTypeProtocol, Text: pipelineConflictText(name)})
		return cmd
	}
	if opts.allowed&c.state.mask() == 0 {
		state := c.state
		c.mu.Unlock()
		cmd.complete(&imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("%s is not valid in %s state", name, state)})
		return cmd
	}
	c.nextTag++
	cmd.tag = fmt.Sprintf("A%04d", c.nextTag)
	if (name == "STARTTLS" || name == "COMPRESS") && c.paused != nil {
		c.pauseTag = cmd.tag
	}
	c.pending[cmd.tag] = cmd
	c.pendingQ = append(c.pendingQ, cmd)
	enc := c.enc
	literalPlus, literalMinus := c.supportsLocked("LITERAL+"), c.supportsLocked("LITERAL-")
	c.mu.Unlock()

	// Any command argument can require a synchronising literal: one non-ASCII
	// octet in a password or a mailbox name is enough. The handler that
	// unblocks one is therefore installed for every command, not only for the
	// commands whose payload is obviously large.
	rejectedBeforeLiteral := false
	if !opts.ownsContinuation {
		continued := make(chan struct{}, 1)
		clearContinuation, installed := c.setContinuationIfUnset(func(string) error {
			// Never block the reader goroutine: a duplicate continuation must
			// not wedge the connection.
			select {
			case continued <- struct{}{}:
			default:
			}
			return nil
		})
		if installed {
			defer clearContinuation()
			enc.SetLiteralPlus(literalPlus)
			enc.SetLiteralMinus(literalMinus)
			enc.SetWaitContinuation(func() error {
				select {
				case <-continued:
					return nil
				case <-cmd.done:
					// The server answered the command line instead of
					// requesting the literal, or the session died. Either way
					// no continuation is coming.
					rejectedBeforeLiteral = true
					return errNoContinuation
				}
			})
			defer enc.SetWaitContinuation(nil)
		}
	}

	enc.Tag(cmd.tag).SP()
	// A command name may be a compound IMAP command such as "UID FETCH".
	// Encode every word as its own atom: feeding the embedded space to Atom
	// rejects every UID variant before any bytes reach the server.
	for i, word := range strings.Fields(name) {
		if i > 0 {
			enc.SP()
		}
		enc.Atom(word)
	}
	if opts.write != nil {
		opts.write(enc)
	}
	enc.CRLF()
	err := enc.Flush()
	if err == nil {
		// The trace deliberately contains only the tag and command name. This
		// is a useful protocol summary while making LOGIN/AUTHENTICATE
		// credential disclosure impossible even if a future caller supplies
		// their arguments via write.
		c.trace(TraceClient, cmd.tag+" "+strings.ToUpper(name))
		return cmd
	}

	if rejectedBeforeLiteral {
		// A command line ending in a synchronising literal announcement that
		// the server rejects leaves the stream synchronised: RFC 3501 section
		// 4.3 requires the client not to send the payload. Only the encoder's
		// sticky error has to be discarded, along with the buffered payload
		// that must never reach the wire.
		c.replaceEncoder(enc)
		return cmd
	}
	c.mu.Lock()
	delete(c.pending, cmd.tag)
	c.removePendingLocked(cmd)
	c.mu.Unlock()
	cmd.complete(protocolError(err))
	c.poison(err)
	return cmd
}

// errNoContinuation reports that a command needing a synchronising literal was
// answered, or the session ended, before the server requested the payload.
var errNoContinuation = errors.New("imapclient: server did not request the literal")

// replaceEncoder discards a sticky encoder error and the output buffered behind
// it. The caller must hold writeMu and must have established that the wire is
// still synchronised.
func (c *Client) replaceEncoder(stale *imapwire.Encoder) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.enc != stale {
		return
	}
	utf8Accept := c.enc.UTF8Accept()
	eopts := c.opts.encoderOptions()
	c.enc = imapwire.NewEncoder(c.conn, &eopts)
	c.enc.SetUTF8Accept(utf8Accept)
}

func pipelineConflict(name string, pending []*Command) bool {
	// RFC 4978: COMPRESS must not be pipelined with any other command.
	if name == "COMPRESS" && len(pending) > 0 {
		return true
	}
	for _, cmd := range pending {
		if cmd.name == "COMPRESS" {
			return true
		}
	}
	if !isNegotiationCommand(name) {
		return false
	}
	for _, cmd := range pending {
		if isNegotiationCommand(cmd.name) {
			return true
		}
	}
	return false
}

func isNegotiationCommand(name string) bool {
	switch name {
	case "ENABLE", "SELECT", "EXAMINE", "STARTTLS":
		return true
	default:
		return false
	}
}

func pipelineConflictText(name string) string {
	if name == "COMPRESS" {
		return "COMPRESS cannot be pipelined with pending commands (RFC 4978)"
	}
	if name == "ENABLE" {
		return "ENABLE cannot be pipelined with pending ENABLE, SELECT, EXAMINE, STARTTLS, or COMPRESS"
	}
	return fmt.Sprintf("%s cannot be pipelined with a pending COMPRESS or with pending ENABLE, SELECT, EXAMINE, or STARTTLS", name)
}

func (c *Client) removePendingLocked(want *Command) {
	for i, cmd := range c.pendingQ {
		if cmd == want {
			copy(c.pendingQ[i:], c.pendingQ[i+1:])
			c.pendingQ[len(c.pendingQ)-1] = nil
			c.pendingQ = c.pendingQ[:len(c.pendingQ)-1]
			return
		}
	}
}

func (c *Client) completeTagged(tag string, cond imapwire.RespCond) {
	if cond.Text.Code == "CAPABILITY" {
		c.addCapabilities(strings.Fields(cond.Text.Args))
	}
	c.mu.Lock()
	cmd := c.pending[tag]
	if cmd != nil {
		delete(c.pending, tag)
		c.removePendingLocked(cmd)
	}
	startTLS := cmd != nil && c.pauseTag == tag
	pause := startTLS && cond.Status == "OK"
	paused, resume := c.paused, c.resume
	if startTLS {
		c.pauseTag = ""
	}
	c.mu.Unlock()
	if cmd == nil {
		c.poison(&imap.Error{Type: imap.ErrorTypeProtocol, Text: "tagged response for unknown command", Tag: tag})
		return
	}
	c.trace(TraceServer, tag+" "+cond.Status)
	if cmd.onComplete != nil {
		if cond.Status == "OK" {
			cmd.onComplete(true, cond.Text.Code, cond.Text.Args)
		} else {
			cmd.onComplete(false, "", "")
		}
	}
	if cond.Status == "OK" {
		cmd.complete(nil)
	} else {
		cmd.complete(responseError(tag, cond))
	}
	if pause {
		close(paused)
		<-resume
	}
}

func responseError(tag string, cond imapwire.RespCond) *imap.Error {
	typeOf := imap.ErrorType(cond.Status)
	if cond.Status == "OK" || cond.Status == "PREAUTH" {
		typeOf = imap.ErrorTypeProtocol
	}
	return &imap.Error{Type: typeOf, Code: imap.ResponseCode(cond.Text.Code), CodeArgs: cond.Text.Args, Text: cond.Text.Text, Tag: tag}
}

func protocolError(err error) *imap.Error {
	if ierr, ok := err.(*imap.Error); ok {
		return ierr
	}
	return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "invalid server response", Err: err}
}
