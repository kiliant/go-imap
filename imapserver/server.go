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
	defaultMaxLiteral           = 64 << 10
	defaultMaxQueuedCommands    = 16
	defaultMaxQueuedCommandByte = 256 << 10
	defaultMaxCommands          = 10_000
	defaultMaxQueuedUpdates     = 256
	defaultMaxQueuedUpdateBytes = 1 << 20
	defaultMaxSelectedMessages  = 2_000_000
)

var (
	errConnectionLimit       = errors.New("imapserver: total connection limit reached")
	errSourceConnectionLimit = errors.New("imapserver: source connection limit reached")
	errPreAuthTimeout        = errors.New("imapserver: pre-authentication timeout")
)

// Limits bounds work retained or initiated by one connection. Non-positive
// fields select safe defaults. The retained command and update queues are
// always bounded.
// Construct with keyed fields only; fields may be added in a future release.
type Limits struct {
	// MaxCommandLineBytes bounds one protocol line excluding literal payloads.
	MaxCommandLineBytes int
	// MaxLiteralBytes bounds one client-supplied literal.
	MaxLiteralBytes int64
	// MaxQueuedCommands bounds parsed commands waiting for the event loop.
	MaxQueuedCommands int
	// MaxQueuedCommandBytes bounds their retained payload bytes in aggregate.
	MaxQueuedCommandBytes int64
	// MaxCommands bounds the lifetime command count of one connection.
	MaxCommands int
	// MaxQueuedUpdates bounds pending backend update batches.
	MaxQueuedUpdates int
	// MaxQueuedUpdateBytes bounds their retained payload bytes in aggregate.
	MaxQueuedUpdateBytes int64
	// MaxSelectedMessages bounds the per-selection UID-to-sequence map.
	MaxSelectedMessages int
	// MaxConnections bounds active connections across the server.
	MaxConnections int
	// MaxConnectionsPerIP bounds active connections sharing one source address.
	MaxConnectionsPerIP int
	// PreAuthTimeout bounds the entire unauthenticated connection lifetime and
	// individual pre-authentication reads and TLS handshakes.
	PreAuthTimeout time.Duration
	// CommandTimeout bounds one event-loop command and backend call.
	CommandTimeout time.Duration
	// ReadTimeout bounds an authenticated protocol read.
	ReadTimeout time.Duration
	// WriteTimeout bounds each protocol write.
	WriteTimeout time.Duration
	// OverflowWriteTimeout bounds the best-effort BYE after update overflow.
	OverflowWriteTimeout time.Duration
	// ForceCloseTimeout bounds how long update overflow may wait before closing
	// the transport unconditionally.
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
	if limits.MaxCommands <= 0 {
		limits.MaxCommands = defaultMaxCommands
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
	if limits.MaxConnections <= 0 {
		limits.MaxConnections = 1024
	}
	if limits.MaxConnectionsPerIP <= 0 {
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
	Greeting string
	// ServerID is the field map returned by ID. New copies the map.
	ServerID map[string]string
	// Limits controls per-connection and server-wide resource bounds.
	Limits Limits
	_      struct{}
}

// Server accepts IMAP connections and runs one isolated session per
// connection. A Server is safe for concurrent use.
type Server struct {
	backend   Backend
	options   Options
	framework map[frameworkComponent]bool

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

// Serve accepts connections until ctx is cancelled, listener is closed, or a
// permanent accept error occurs. Each accepted connection is closed by the
// server after its session ends.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
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
			return err
		}
		c, err := newConn(ctx, s, netConn)
		if err != nil {
			_ = netConn.Close()
			return err
		}
		if err := s.register(c); err != nil {
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
func (s *Server) ServeConn(ctx context.Context, netConn net.Conn) error {
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
		_ = netConn.Close()
		return err
	}
	return s.serveRegisteredConn(c)
}

func (s *Server) serveRegisteredConn(c *conn) error {
	defer s.unregister(c)
	defer c.close()
	return c.serve()
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
func (s *Server) Close(ctx context.Context) error {
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
