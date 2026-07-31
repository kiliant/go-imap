// Package imapclient implements an IMAP4rev1 and IMAP4rev2 client.
//
// Commands are pipelined: methods that issue a command return a [Command]
// immediately, and [Command.Wait] waits for its tagged completion. One reader
// goroutine owns response parsing and dispatches unsolicited mailbox updates to
// [UnilateralDataHandler]. A command-specific collector gets first refusal of
// an untagged response; responses no collector claims are connection-scoped.
//
// IMAP has no general command abort. Cancelling [Command.Wait] after its
// command is on the wire closes and invalidates the connection rather than
// leaving the stream desynchronised. IDLE is the one protocol exception and
// has its own clean cancellation path.
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

	// UnilateralData receives connection-scoped untagged data. A nil handler is
	// valid. Its callbacks run on the reader goroutine and must not block.
	UnilateralData *UnilateralDataHandler

	// DebugWriter receives a redacted, line-oriented protocol trace. Credentials
	// and authentication challenges are never written to it.
	DebugWriter io.Writer

	// Trace receives redacted protocol trace events. It is called synchronously
	// by the reader or writer goroutine and must not block.
	Trace func(TraceEvent)
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

// TraceEvent is one redacted protocol trace event.
//
// Construct with keyed fields only; fields may be added in a future release.
type TraceEvent struct {
	Direction TraceDirection
	Data      string
	_         struct{}
}

// UnilateralDataHandler receives untagged data that no in-flight command
// claims. It is a struct of callbacks rather than an interface so future
// unsolicited response types can be added without breaking implementations.
//
// Construct with keyed fields only; fields may be added in a future release.
type UnilateralDataHandler struct {
	Exists  func(numMessages uint32)
	Expunge func(seqNum uint32)
	Recent  func(numRecent uint32)
	Fetch   func(data *imap.FetchMessageData)
	_       struct{}
}

// Client is an IMAP session. Its zero value is not usable; obtain one with
// [Dial], [DialTLS], [DialStartTLS], or [NewClient].
type Client struct {
	mu sync.Mutex

	traceMu sync.Mutex

	conn net.Conn
	opts Options
	dec  *imapwire.Decoder
	enc  *imapwire.Encoder

	state State
	caps  map[string]struct{}

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

	collector commandCollector
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
		conn:       conn,
		opts:       o,
		dec:        imapwire.NewDecoder(conn, nil),
		enc:        imapwire.NewEncoder(conn, nil),
		state:      StateNotAuthenticated,
		caps:       make(map[string]struct{}),
		pending:    make(map[string]*Command),
		greeting:   make(chan struct{}),
		readerDone: make(chan struct{}),
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

// Capabilities returns a snapshot of the capability names learned from the
// greeting or a CAPABILITY response. Names are upper-cased. The returned map
// is owned by the caller.
func (c *Client) Capabilities() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[string]bool, len(c.caps))
	for cap := range c.caps {
		result[cap] = true
	}
	return result
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
	cmd := &Command{client: c, name: name, done: make(chan struct{}), collector: collector}
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
	c.enc.Tag(cmd.tag).SP().Atom(name)
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
	// useful wire trace while making LOGIN/AUTHENTICATE credential disclosure
	// impossible even if a future caller supplies their arguments via write.
	c.trace(TraceClient, cmd.tag+" "+strings.ToUpper(name))
	return cmd
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
