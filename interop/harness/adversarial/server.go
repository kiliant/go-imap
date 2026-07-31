// Package adversarial provides a deterministic fake IMAP server for hostile
// response tests. It has no dependency on the production client and can be
// reused by its tests once that client lands.
package adversarial

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Case is one scripted server behaviour. Responses are written after one
// complete client command is received. When HoldOpen is true the connection is
// left open until the case context is cancelled.
type Case struct {
	Name      string
	Greeting  []byte
	Responses [][]byte
	HoldOpen  bool
}

// Server is one loopback-only scripted server.
type Server struct {
	listener net.Listener
	cancel   context.CancelFunc
	done     chan struct{}

	mu  sync.Mutex
	err error
}

// Start begins serving a case on an ephemeral loopback port.
func Start(parent context.Context, scenario Case) (*Server, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	server := &Server{listener: listener, cancel: cancel, done: make(chan struct{})}
	go server.serve(ctx, scenario)
	return server, nil
}

// Pipe begins serving a case over an in-memory full-duplex connection. It is
// useful in sandboxes which prohibit binding a loopback socket.
func Pipe(parent context.Context, scenario Case) (net.Conn, *Server) {
	client, peer := net.Pipe()
	ctx, cancel := context.WithCancel(parent)
	server := &Server{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(server.done)
		server.serveConn(ctx, peer, scenario)
	}()
	return client, server
}

// Address returns the loopback listener address.
func (s *Server) Address() string { return s.listener.Addr().String() }

func (s *Server) serve(ctx context.Context, scenario Case) {
	defer close(s.done)
	conn, err := s.listener.Accept()
	if err != nil {
		if ctx.Err() == nil {
			s.setError(err)
		}
		return
	}
	s.serveConn(ctx, conn, scenario)
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn, scenario Case) {
	defer conn.Close()
	if scenario.Greeting == nil {
		scenario.Greeting = []byte("* OK adversarial server ready\r\n")
	}
	if err := writeAll(conn, scenario.Greeting); err != nil {
		s.setError(err)
		return
	}
	reader := bufio.NewReader(io.LimitReader(conn, 1<<20))
	if _, err := reader.ReadString('\n'); err != nil {
		s.setError(fmt.Errorf("read command: %w", err))
		return
	}
	for _, response := range scenario.Responses {
		if err := writeAll(conn, response); err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.setError(err)
			}
			return
		}
	}
	if scenario.HoldOpen {
		<-ctx.Done()
	}
}

func writeAll(dst io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := dst.Write(data)
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func (s *Server) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// Close stops the server and waits for its goroutine.
func (s *Server) Close() error {
	s.cancel()
	if s.listener != nil {
		_ = s.listener.Close()
	}
	select {
	case <-s.done:
	case <-time.After(time.Second):
		return errors.New("adversarial: server did not stop")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Cases returns the hostile-response catalogue required by T13. Individual
// client tests select the cases relevant to their parser entry point.
func Cases() []Case {
	deep := make([]byte, 0, 2048)
	deep = append(deep, "* 1 FETCH (BODYSTRUCTURE "...)
	for range 1000 {
		deep = append(deep, '(')
	}
	deep = append(deep, "NIL"...)
	for range 1000 {
		deep = append(deep, ')')
	}
	deep = append(deep, ")\r\n"...)
	return []Case{
		{Name: "truncated-literal", Responses: [][]byte{[]byte("* 1 FETCH (BODY[] {20}\r\nshort")}},
		{Name: "wrong-literal-length", Responses: [][]byte{[]byte("* 1 FETCH (BODY[] {2}\r\ntoolong)\r\n")}},
		{Name: "unknown-tag", Responses: [][]byte{[]byte("Z999 OK never sent\r\n")}},
		{Name: "bye-mid-command", Responses: [][]byte{[]byte("* BYE shutting down\r\n")}},
		{Name: "ten-megabyte-line", Responses: [][]byte{append(append([]byte("* OK "), make([]byte, 10<<20)...), '\r', '\n')}},
		{Name: "unterminated-list", Responses: [][]byte{[]byte("* LIST (\\HasChildren \"/\" \"broken\"\r\n")}},
		{Name: "deep-nesting", Responses: [][]byte{deep}},
		{Name: "impossible-star", Responses: [][]byte{[]byte("* * *\r\n")}},
		{Name: "nul-in-atom", Responses: [][]byte{[]byte("* OK bad\x00atom\r\n")}},
		{Name: "cr-without-lf", Responses: [][]byte{[]byte("* OK broken\r")}},
		{Name: "stalled-literal", Responses: [][]byte{[]byte("* 1 FETCH (BODY[] {20}\r\n")}, HoldOpen: true},
	}
}
