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
}

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
	_     struct{}
}

// Client is an IMAP session. Its zero value is not usable; obtain one with
// [Dial], [DialTLS], [DialStartTLS], or [NewClient].
type Client struct {
	mu sync.Mutex

	// authInProgress excludes unrelated commands while an AUTHENTICATE
	// exchange is consuming continuation requests. IMAP cannot safely
	// disambiguate a continuation once another command is pipelined beside it.
	authInProgress bool

	// continuationMu is deliberately separate from mu. A synchronising
	// literal is written while mu protects the encoder, while the reader must
	// be able to deliver its continuation without waiting for that write lock.
	continuationMu sync.Mutex
	continuation   *continuationHandler
	literalMu      sync.Mutex

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
	onComplete func(success bool)
}

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
	c := &Client{
		conn:               conn,
		opts:               o,
		dec:                imapwire.NewDecoder(conn, nil),
		enc:                imapwire.NewEncoder(conn, nil),
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

// WaitGreeting waits until the server greeting has been parsed.
func (c *Client) WaitGreeting(ctx context.Context) error {
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

// Noop issues NOOP and returns its command handle immediately.
func (c *Client) Noop() *Command {
	return c.beginCommand("NOOP", stateNotAuthenticated|stateAuthenticated|stateSelected, nil, nil)
}

// Logout asks the server to end the session, waits for its tagged completion,
// and closes the transport. ctx must be non-nil.
func (c *Client) Logout(ctx context.Context) error {
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

func (c *Client) beginCommand(name string, allowed stateMask, write func(*imapwire.Encoder), collector commandCollector) *Command {
	return c.beginCommandWithCompletion(name, allowed, write, collector, nil)
}

// beginAuthenticationCommand reserves the connection for LOGIN or
// AUTHENTICATE until its tagged completion. A SASL continuation has no command
// tag, so allowing another command to pipeline beside it would be ambiguous.
func (c *Client) beginAuthenticationCommand(name string, write func(*imapwire.Encoder), collector commandCollector) *Command {
	c.mu.Lock()
	if c.authInProgress {
		c.mu.Unlock()
		cmd := &Command{client: c, name: name, done: make(chan struct{})}
		cmd.complete(&imap.Error{Type: imap.ErrorTypeProtocol, Text: "authentication is already in progress"})
		return cmd
	}
	c.authInProgress = true
	c.mu.Unlock()

	finished := func(success bool) {
		c.mu.Lock()
		c.authInProgress = false
		c.mu.Unlock()
		if success {
			c.authenticationSucceeded()
		}
	}
	cmd := c.beginCommandWithCompletion(name, stateNotAuthenticated, write, collector, finished)
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
func (c *Client) beginCommandWithCompletion(name string, allowed stateMask, write func(*imapwire.Encoder), collector commandCollector, onComplete func(success bool)) *Command {
	cmd := &Command{client: c, name: name, done: make(chan struct{}), collector: collector, onComplete: onComplete}
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
	if allowed&c.state.mask() == 0 {
		state := c.state
		c.mu.Unlock()
		cmd.complete(&imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("%s is not valid in %s state", name, state)})
		return cmd
	}
	c.nextTag++
	cmd.tag = fmt.Sprintf("A%04d", c.nextTag)
	if name == "STARTTLS" && c.paused != nil {
		c.pauseTag = cmd.tag
	}
	c.pending[cmd.tag] = cmd
	c.pendingQ = append(c.pendingQ, cmd)
	c.enc.Tag(cmd.tag).SP()
	// A command name may be a compound IMAP command such as "UID FETCH".
	// Encode every word as its own atom: feeding the embedded space to Atom
	// rejects every UID variant before any bytes reach the server.
	for i, word := range strings.Fields(name) {
		if i > 0 {
			c.enc.SP()
		}
		c.enc.Atom(word)
	}
	if write != nil {
		write(c.enc)
	}
	c.enc.CRLF()
	err := c.enc.Flush()
	if err != nil {
		delete(c.pending, cmd.tag)
		c.removePendingLocked(cmd)
	}
	c.mu.Unlock()
	if err != nil {
		cmd.complete(protocolError(err))
		c.poison(err)
		return cmd
	}
	// The trace deliberately contains only the tag and command name. This is a
	// useful protocol summary while making LOGIN/AUTHENTICATE credential
	// disclosure impossible even if a future caller supplies their arguments via
	// write.
	c.trace(TraceClient, cmd.tag+" "+strings.ToUpper(name))
	return cmd
}

func pipelineConflict(name string, pending []*Command) bool {
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
	case "ENABLE", "SELECT", "EXAMINE":
		return true
	default:
		return false
	}
}

func pipelineConflictText(name string) string {
	if name == "ENABLE" {
		return "ENABLE cannot be pipelined with pending ENABLE, SELECT, or EXAMINE"
	}
	return fmt.Sprintf("%s cannot be pipelined with pending ENABLE, SELECT, or EXAMINE", name)
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
		cmd.onComplete(cond.Status == "OK")
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
