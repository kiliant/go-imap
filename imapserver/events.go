package imapserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"strings"

	"github.com/kiliant/go-imap"
)

// ServerEventKind identifies an operational diagnostic. Additional kinds may
// be introduced in future releases; callers must accept unknown values.
type ServerEventKind string

const (
	ServerEventBackendError      ServerEventKind = "backend-error"
	ServerEventConnectionError   ServerEventKind = "connection-error"
	ServerEventConnectionLimit   ServerEventKind = "connection-limit"
	ServerEventTimeout           ServerEventKind = "timeout"
	ServerEventAcceptError       ServerEventKind = "accept-error"
	ServerEventTLSHandshakeError ServerEventKind = "tls-handshake-error"
	ServerEventPanic             ServerEventKind = "panic"
)

// ServerEvent is an operational diagnostic delivered to Options.OnError.
// Construct with keyed fields only; fields may be added in a future release.
type ServerEvent struct {
	// Kind identifies the diagnostic category.
	Kind ServerEventKind
	// Conn is a transport snapshot, or nil for listener errors.
	Conn *ConnInfo
	// Command is the command name, including a UID subcommand when known.
	// Arguments and credentials are never included by the framework.
	Command string
	// Err retains the original cause, which may contain sensitive backend data.
	Err error
	// Stack contains the goroutine stack for a recovered panic only.
	Stack []byte
	// Dropped counts diagnostics discarded since the previous queued event.
	Dropped uint64
	_       struct{}
}

const maxPendingServerEvents = 64

type pendingServerEvent struct {
	ctx   context.Context
	event *ServerEvent
}

// A single worker exists only while callbacks are pending. A blocked callback
// retains at most one worker and a bounded queue, independently of connections.
func (s *Server) reportEvent(ctx context.Context, event *ServerEvent) {
	if s == nil || s.options.OnError == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	if len(s.events) == maxPendingServerEvents {
		s.droppedEvents++
		return
	}
	event.Dropped = s.droppedEvents
	s.droppedEvents = 0
	s.events = append(s.events, pendingServerEvent{ctx, event})
	if !s.deliveringEvents {
		s.deliveringEvents = true
		go s.deliverEvents()
	}
}

func (s *Server) deliverEvents() {
	for {
		s.eventMu.Lock()
		if len(s.events) == 0 {
			s.deliveringEvents = false
			s.events = nil
			s.eventMu.Unlock()
			return
		}
		pending := s.events[0]
		s.events[0] = pendingServerEvent{}
		s.events = s.events[1:]
		s.eventMu.Unlock()
		func() {
			// A diagnostic callback must not take down the server, including
			// when invoked to report another recovered panic.
			defer func() { _ = recover() }()
			s.options.OnError(pending.ctx, pending.event)
		}()
	}
}

func (c *conn) reportEvent(kind ServerEventKind, command string, err error) {
	if c.server == nil || c.server.options.OnError == nil {
		return
	}
	if command == "" {
		command, _ = c.eventCommand.Load().(string)
	}
	c.server.reportEvent(c.ctx, &ServerEvent{Kind: kind, Conn: connectionInfo(c), Command: command, Err: err})
}

func (c *conn) reportTerminal(kind ServerEventKind, err error) {
	if c.terminalReported.CompareAndSwap(false, true) {
		c.reportEvent(kind, "", err)
	}
}

func (c *conn) protect(run func() error) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = fmt.Errorf("imapserver: recovered panic: %v", value)
			// Preserve the panic even if closing the transport makes the other
			// goroutine return an unrelated I/O error first. ServeConn callers
			// and whole-server fuzzing must still see the internal failure.
			c.fatalMu.Lock()
			c.fatal = err
			c.fatalMu.Unlock()
			command, _ := c.eventCommand.Load().(string)
			c.terminalReported.Store(true)
			c.server.reportEvent(c.ctx, &ServerEvent{Kind: ServerEventPanic, Conn: connectionInfo(c), Command: command, Err: err, Stack: debug.Stack()})
			c.cancel(err)
			c.closeTransport()
		}
	}()
	return run()
}

func terminalEventKind(err error) ServerEventKind {
	var network net.Error
	if errors.Is(err, errPreAuthTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
		return ServerEventTimeout
	}
	if errors.Is(err, errCommandLimit) || errors.Is(err, ErrUpdateOverflow) || errors.Is(err, errConnectionLimit) || errors.Is(err, errSourceConnectionLimit) {
		return ServerEventConnectionLimit
	}
	return ServerEventConnectionError
}

func (c *conn) reportBackendError(operation string, err error) {
	var protocol *imap.Error
	if errors.As(err, &protocol) && protocol.Code != imap.CodeServerBug {
		code := strings.ToUpper(string(protocol.Code))
		if !strings.ContainsAny(code, "[] \t\r\n") && !strings.ContainsAny(protocol.CodeArgs, "]\x00\r\n") {
			return
		}
	}
	kind := ServerEventBackendError
	if errors.Is(err, context.DeadlineExceeded) {
		kind = ServerEventTimeout
	} else if errors.Is(err, context.Canceled) {
		return
	}
	command, _ := c.eventCommand.Load().(string)
	if command == "" {
		command = operation
	}
	c.reportEvent(kind, command, err)
}
