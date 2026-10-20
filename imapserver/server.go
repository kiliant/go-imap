package imapserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

const (
	defaultMaxCommandLine       = 8 << 10
	defaultMaxLiteral           = 32 << 20
	defaultMaxQueuedCommands    = 16
	defaultMaxQueuedCommandByte = 256 << 10
	defaultMaxQueuedUpdates     = 256
	defaultMaxQueuedUpdateBytes = 1 << 20
	defaultMaxSelectedMessages  = 2_000_000
)

var (
	errConnectionLimit       = errors.New("imapserver: total connection limit reached")
	errSourceConnectionLimit = errors.New("imapserver: source connection limit reached")
	errCommandLimit          = errors.New("imapserver: command limit exceeded")
	errPreAuthTimeout        = errors.New("imapserver: pre-authentication timeout")
)

// Limits bounds work retained or initiated by one connection. Non-positive
// fields select the documented defaults, except MaxCommands, where they mean
// unlimited, and the connection limits, where negative values mean unlimited.
// The retained command and update queues are always bounded.
// Defaults may be retuned in a minor release; set positive values to pin an
// application's limits across upgrades.
// Construct with keyed fields only; fields may be added in a future release.
type Limits struct {
	// MaxCommandLineBytes bounds one protocol line excluding literal payloads.
	// The default is 8 KiB.
	MaxCommandLineBytes int
	// MaxLiteralBytes bounds each client-supplied literal, the aggregate staged
	// APPEND payload and the aggregate resolved URLFETCH payload per command.
	// The default is 32 MiB. STATUS and LIST-STATUS APPENDLIMIT replies are
	// capped at this value, including a backend's unlimited (NIL) reply.
	MaxLiteralBytes int64
	// MaxQueuedCommands bounds parsed commands waiting for the event loop.
	// The default is 16.
	MaxQueuedCommands int
	// MaxQueuedCommandBytes bounds their retained payload bytes in aggregate.
	// The default is 256 KiB.
	MaxQueuedCommandBytes int64
	// MaxCommands bounds the lifetime command count of one connection.
	// Non-positive values mean unlimited (the default). A positive limit
	// closes the connection with BYE before executing the next command.
	MaxCommands int
	// MaxQueuedUpdates bounds pending backend update batches, including
	// retained removal announcements the protocol does not yet permit.
	// The default is 256.
	MaxQueuedUpdates int
	// MaxQueuedUpdateBytes bounds their retained payload bytes in aggregate.
	// The default is 1 MiB.
	MaxQueuedUpdateBytes int64
	// MaxSelectedMessages bounds the per-selection UID-to-sequence map.
	// The default is 2,000,000 messages.
	MaxSelectedMessages int
	// MaxConnections bounds active connections across the server.
	// Zero selects the default of 1024; a negative value means unlimited.
	MaxConnections int
	// MaxConnectionsPerIP bounds active connections sharing one source address.
	// Zero selects the default of 32; a negative value means unlimited.
	MaxConnectionsPerIP int
	// PreAuthTimeout bounds the entire unauthenticated connection lifetime and
	// individual pre-authentication reads and TLS handshakes.
	// The default is 2 minutes.
	PreAuthTimeout time.Duration
	// CommandTimeout bounds one event-loop command and backend call.
	// The default is 5 minutes.
	CommandTimeout time.Duration
	// ReadTimeout bounds an authenticated protocol read.
	// The default is 30 minutes.
	ReadTimeout time.Duration
	// WriteTimeout bounds each protocol write.
	// The default is 5 minutes.
	WriteTimeout time.Duration
	// OverflowWriteTimeout bounds the best-effort BYE after update overflow.
	// The default is 250 milliseconds.
	OverflowWriteTimeout time.Duration
	// ForceCloseTimeout bounds how long update overflow may wait before closing
	// the transport unconditionally. The default is 1 second.
	ForceCloseTimeout time.Duration
	_                 struct{}
}

func (limits Limits) withDefaults() Limits {
	if limits.MaxCommandLineBytes <= 0 {
		limits.MaxCommandLineBytes = defaultMaxCommandLine
	}
	if limits.MaxLiteralBytes <= 0 {
		limits.MaxLiteralBytes = defaultMaxLiteral
	}
	if limits.MaxQueuedCommands <= 0 {
		limits.MaxQueuedCommands = defaultMaxQueuedCommands
	}
	if limits.MaxQueuedCommandBytes <= 0 {
		limits.MaxQueuedCommandBytes = defaultMaxQueuedCommandByte
	}
	if limits.MaxQueuedUpdates <= 0 {
		limits.MaxQueuedUpdates = defaultMaxQueuedUpdates
	}
	if limits.MaxQueuedUpdateBytes <= 0 {
		limits.MaxQueuedUpdateBytes = defaultMaxQueuedUpdateBytes
	}
	if limits.MaxSelectedMessages <= 0 {
		limits.MaxSelectedMessages = defaultMaxSelectedMessages
	}
	if limits.MaxConnections == 0 {
		limits.MaxConnections = 1024
	}
	if limits.MaxConnectionsPerIP == 0 {
		limits.MaxConnectionsPerIP = 32
	}
	if limits.PreAuthTimeout <= 0 {
		limits.PreAuthTimeout = 2 * time.Minute
	}
	if limits.CommandTimeout <= 0 {
		limits.CommandTimeout = 5 * time.Minute
	}
	if limits.ReadTimeout <= 0 {
		limits.ReadTimeout = 30 * time.Minute
	}
	if limits.WriteTimeout <= 0 {
		limits.WriteTimeout = 5 * time.Minute
	}
	if limits.OverflowWriteTimeout <= 0 {
		limits.OverflowWriteTimeout = 250 * time.Millisecond
	}
	if limits.ForceCloseTimeout <= 0 {
		limits.ForceCloseTimeout = time.Second
	}
	return limits
}

// Options configures a Server. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type Options struct {
	// TLSConfig enables STARTTLS. New clones it and enforces TLS 1.2. Callers
	// supplying an already wrapped TLS connection configure that wrapper too.
	TLSConfig *tls.Config
	// RequireTLS disables cleartext authentication even when
	// AllowInsecureAuth is also set.
	RequireTLS bool
	// AllowInsecureAuth permits cleartext password authentication only when
	// RequireTLS is false.
	AllowInsecureAuth bool
	// Greeting is the human-readable text after the initial OK response.
	// Empty selects "go-imap ready".
	Greeting string
	// ServerID is the field map returned by ID. New copies the map.
	// Nil or empty returns ID NIL.
	ServerID map[string]string
	// Limits controls per-connection and server-wide resource bounds.
	Limits Limits
	// OnError receives backend failures hidden behind SERVERBUG, connection
	// limits/timeouts, accept/TLS errors and recovered connection panics.
	// Nil disables diagnostics. Calls run serially on a separate goroutine,
	// outside server locks, and may overlap ServeConn/Close returning. Close
	// does not wait for callbacks; they may call Close themselves. The context
	// is the originating Serve/ServeConn context (and may be cancelled).
	// Delivery is best effort: at most 64 events wait behind a callback, with
	// further events dropped and counted on the next queued ServerEvent.
	// Callback panics are recovered. Callbacks should return promptly.
	OnError func(context.Context, *ServerEvent)
	_       struct{}
}

// Server accepts IMAP connections and runs one isolated session per
// connection. A Server is safe for concurrent use.
type Server struct {
	backend   Backend
	options   Options
	framework map[frameworkComponent]bool

	eventMu          sync.Mutex
	events           []pendingServerEvent
	deliveringEvents bool
	droppedEvents    uint64

	mu       sync.Mutex
	closed   bool
	active   map[*conn]struct{}
	bySource map[string]int
	changed  chan struct{}
}

// New returns a server using backend. Authentication capabilities remain
// unavailable when backend is nil, but framework-owned pre-authentication
// commands can still be used, which is useful for protocol frontends.
func New(backend Backend, options *Options) *Server {
	var opts Options
	if options != nil {
		opts = *options
	}
	opts.Limits = opts.Limits.withDefaults()
	if opts.Greeting == "" {
		opts.Greeting = "go-imap ready"
	}
	if opts.TLSConfig != nil {
		opts.TLSConfig = opts.TLSConfig.Clone()
		if opts.TLSConfig.MinVersion < tls.VersionTLS12 {
			opts.TLSConfig.MinVersion = tls.VersionTLS12
		}
	}
	if opts.ServerID != nil {
		serverID := make(map[string]string, len(opts.ServerID))
		for key, value := range opts.ServerID {
			serverID[key] = value
		}
		opts.ServerID = serverID
	}
	return &Server{
		backend:   backend,
		options:   opts,
		framework: compiledFrameworkSupport(),
		active:    make(map[*conn]struct{}),
		bySource:  make(map[string]int),
		changed:   make(chan struct{}, 1),
	}
}

// ServeOptions configures one listener. A nil pointer selects the defaults,
// which are the server-wide [Options].
//
// Empty today, and present because per-listener settings are the shape this
// grows into. RFC 8314 wants implicit TLS on 993 while RFC 2595 wants
// LOGINDISABLED on cleartext 143, so a server listening on both needs
// RequireTLS and AllowInsecureAuth to differ per listener; today they are
// server-wide and cannot. Adding the parameter after imapserver v1.0 would
// break every caller, so it is here before there is one.
//
// Construct with keyed fields only; fields may be added in a future release.
type ServeOptions struct{ _ struct{} }

// ServeConnOptions configures one already-accepted connection. A nil pointer
// selects the defaults, which are the server-wide [Options].
//
// Separate from [ServeOptions] because the two answer different questions. A
// caller doing its own accept loop already knows things the framework cannot
// infer — that a connection arrived on an implicit-TLS port, or the original
// client address behind a PROXY-protocol header (RFC 7239) — and that is
// per-connection, not per-listener.
//
// Construct with keyed fields only; fields may be added in a future release.
type ServeConnOptions struct{ _ struct{} }

// ServerCloseOptions configures shutdown. A nil pointer selects the defaults.
//
// Empty today. Graceful drain — stop accepting, let existing sessions finish,
// bounded by the context — is the field this exists to be able to add without
// a break. Note that [Server.Close] takes a context and so is deliberately not
// [io.Closer]; the root module's Close exemption does not reach it.
//
// Construct with keyed fields only; fields may be added in a future release.
type ServerCloseOptions struct{ _ struct{} }

// Serve accepts connections until ctx is cancelled, listener is closed, or a
// permanent accept error occurs. Each accepted connection is closed by the
// server after its session ends.
//
// options may be nil.
func (s *Server) Serve(ctx context.Context, listener net.Listener, options *ServeOptions) error {
	if s == nil || listener == nil {
		return fmt.Errorf("imapserver: nil server or listener")
	}
	if ctx == nil {
		return fmt.Errorf("imapserver: nil context")
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-stop:
		}
	}()
	for {
		netConn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			s.reportEvent(ctx, &ServerEvent{Kind: ServerEventAcceptError, Err: err})
			return err
		}
		c, err := newConn(ctx, s, netConn)
		if err != nil {
			_ = netConn.Close()
			return err
		}
		if err := s.register(c); err != nil {
			if !errors.Is(err, net.ErrClosed) {
				c.reportTerminal(terminalEventKind(err), err)
			}
			_ = netConn.Close()
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			if errors.Is(err, errConnectionLimit) || errors.Is(err, errSourceConnectionLimit) {
				continue
			}
			return err
		}
		go func() { _ = s.serveRegisteredConn(c) }()
	}
}

// ServeConn runs one already-accepted connection until logout, disconnect,
// cancellation or a fatal protocol error. It always closes netConn.
//
// options may be nil.
func (s *Server) ServeConn(ctx context.Context, netConn net.Conn, options *ServeConnOptions) error {
	if s == nil || netConn == nil {
		return fmt.Errorf("imapserver: nil server or connection")
	}
	if ctx == nil {
		_ = netConn.Close()
		return fmt.Errorf("imapserver: nil context")
	}
	c, err := newConn(ctx, s, netConn)
	if err != nil {
		_ = netConn.Close()
		return err
	}
	if err := s.register(c); err != nil {
		if !errors.Is(err, net.ErrClosed) {
			c.reportTerminal(terminalEventKind(err), err)
		}
		_ = netConn.Close()
		return err
	}
	return s.serveRegisteredConn(c)
}

func (s *Server) serveRegisteredConn(c *conn) error {
	defer s.unregister(c)
	defer c.close()
	err := c.serve()
	if err != nil {
		c.reportTerminal(terminalEventKind(err), err)
	}
	return err
}

func (s *Server) register(c *conn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	limits := s.options.Limits
	if limits.MaxConnections >= 0 && len(s.active) >= limits.MaxConnections {
		return errConnectionLimit
	}
	source := sourceAddress(c.raw.RemoteAddr())
	if limits.MaxConnectionsPerIP >= 0 && s.bySource[source] >= limits.MaxConnectionsPerIP {
		return errSourceConnectionLimit
	}
	s.active[c] = struct{}{}
	s.bySource[source]++
	return nil
}

func (s *Server) unregister(c *conn) {
	s.mu.Lock()
	if _, ok := s.active[c]; ok {
		delete(s.active, c)
		source := sourceAddress(c.raw.RemoteAddr())
		s.bySource[source]--
		if s.bySource[source] == 0 {
			delete(s.bySource, source)
		}
	}
	s.mu.Unlock()
	s.signalChanged()
}

func (s *Server) signalChanged() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func sourceAddress(addr net.Addr) string {
	if addr == nil {
		return "<unknown>"
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err == nil {
		return host
	}
	return addr.String()
}

// Close terminates active connections and waits for all ServeConn calls to
// return. It does not close listeners passed to Serve; cancel the corresponding
// Serve context or close the listener as well.
//
// options may be nil.
func (s *Server) Close(ctx context.Context, options *ServerCloseOptions) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		return fmt.Errorf("imapserver: nil context")
	}
	for {
		s.mu.Lock()
		s.closed = true
		connections := make([]*conn, 0, len(s.active))
		for c := range s.active {
			connections = append(connections, c)
		}
		s.mu.Unlock()
		if len(connections) == 0 {
			return nil
		}
		for _, c := range connections {
			c.close()
		}
		select {
		case <-s.changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
