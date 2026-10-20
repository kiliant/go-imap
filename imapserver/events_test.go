package imapserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap/internal/imapwire"
)

func awaitServerEvent(t *testing.T, events <-chan *ServerEvent) *ServerEvent {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("no diagnostic received")
		return nil
	}
}

func TestServerEventPreservesHiddenBackendError(t *testing.T) {
	events := make(chan *ServerEvent, 1)
	s := New(nil, &Options{OnError: func(_ context.Context, event *ServerEvent) { events <- event }})
	var output bytes.Buffer
	c := &conn{server: s, ctx: context.Background(), encoder: imapwire.NewEncoder(&output, &imapwire.EncoderOptions{ServerResponse: true})}
	c.eventCommand.Store("UID SEARCH")
	cause := errors.New("private database password")
	if err := writeBackendError(c, "A1", "SEARCH", cause); err != nil {
		t.Fatal(err)
	}
	event := awaitServerEvent(t, events)
	if event.Kind != ServerEventBackendError || !errors.Is(event.Err, cause) || event.Command != "UID SEARCH" {
		t.Fatalf("diagnostic = %+v", event)
	}
	if strings.Contains(output.String(), cause.Error()) || !strings.Contains(output.String(), "NO [SERVERBUG] SEARCH failed") {
		t.Fatalf("wire = %q", output.String())
	}
}

func TestServerEventCallbackIsBoundedAndDoesNotBlockClose(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	s := New(nil, &Options{OnError: func(_ context.Context, event *ServerEvent) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
	}})
	s.reportEvent(context.Background(), &ServerEvent{})
	<-entered
	defer close(release)
	for range 1000 {
		s.reportEvent(context.Background(), &ServerEvent{})
	}
	s.eventMu.Lock()
	queued, dropped := len(s.events), s.droppedEvents
	s.eventMu.Unlock()
	if queued != maxPendingServerEvents || dropped != 1000-maxPendingServerEvents {
		t.Fatalf("queued=%d dropped=%d", queued, dropped)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// Discard queued test events while the worker is parked, then verify that
	// overflow is visible on the next successfully queued diagnostic.
	s.eventMu.Lock()
	s.events = nil
	s.eventMu.Unlock()
	s.reportEvent(context.Background(), &ServerEvent{Command: "last"})
	s.eventMu.Lock()
	got := s.events[0].event.Dropped
	s.eventMu.Unlock()
	if got != dropped {
		t.Fatalf("Dropped=%d want %d", got, dropped)
	}
}

func TestServerEventCallbackPanicIsContained(t *testing.T) {
	done := make(chan struct{})
	var calls atomic.Int64
	s := New(nil, &Options{OnError: func(context.Context, *ServerEvent) {
		if calls.Add(1) == 1 {
			panic("callback")
		}
		close(done)
	}})
	s.reportEvent(context.Background(), &ServerEvent{})
	s.reportEvent(context.Background(), &ServerEvent{})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker stopped after callback panic")
	}
}

func TestServerEventConnectionFailures(t *testing.T) {
	for _, scenario := range []string{"commands", "preauth", "overflow", "tls", "panic"} {
		t.Run(scenario, func(t *testing.T) {
			events := make(chan *ServerEvent, 8)
			timeout := 2 * time.Second
			if scenario == "preauth" {
				timeout = 30 * time.Millisecond
			}
			s := New(nil, &Options{OnError: func(_ context.Context, e *ServerEvent) { events <- e }, Limits: Limits{MaxCommands: 1, PreAuthTimeout: timeout}})
			serverSide, clientSide := net.Pipe()
			defer clientSide.Close()
			_ = clientSide.SetDeadline(time.Now().Add(3 * time.Second))
			var transport net.Conn = serverSide
			if scenario == "tls" {
				transport = tls.Server(serverSide, &tls.Config{})
			}
			done := make(chan error, 1)
			if scenario == "panic" {
				commandDescriptors["PANICTEST"] = &commandDescriptor{name: "PANICTEST", states: stateMaskAny, parse: parseNoArgs, handle: func(context.Context, *conn, *queuedCommand) error { panic("backend panic") }}
				defer delete(commandDescriptors, "PANICTEST")
			}
			go func() { done <- s.ServeConn(context.Background(), transport, nil) }()
			reader := bufio.NewReader(clientSide)
			if scenario != "tls" {
				if _, err := reader.ReadString('\n'); err != nil {
					t.Fatal(err)
				}
			}
			want := ServerEventConnectionLimit
			switch scenario {
			case "commands":
				_, _ = clientSide.Write([]byte("A NOOP\r\n"))
				_, _ = reader.ReadString('\n')
				_, _ = clientSide.Write([]byte("B NOOP\r\n"))
			case "preauth":
				want = ServerEventTimeout
			case "tls":
				want = ServerEventTLSHandshakeError
				_, _ = clientSide.Write([]byte("not a tls handshake\r\n"))
			case "panic":
				want = ServerEventPanic
				_, _ = clientSide.Write([]byte("A PANICTEST\r\n"))
			case "overflow":
				s.mu.Lock()
				for c := range s.active {
					c.updateOverflow()
				}
				s.mu.Unlock()
				go func() { _, _ = reader.ReadString('\n') }()
			}
			event := awaitServerEvent(t, events)
			if event.Kind != want || event.Err == nil || event.Conn == nil || event.Conn.RemoteAddr == nil {
				t.Fatalf("event=%+v want %s", event, want)
			}
			if scenario == "panic" && (event.Command != "PANICTEST" || len(event.Stack) == 0) {
				t.Fatalf("panic diagnostic=%+v", event)
			}
			select {
			case err := <-done:
				if scenario == "panic" && (err == nil || !strings.HasPrefix(err.Error(), "imapserver: recovered panic:")) {
					t.Fatalf("ServeConn hid recovered panic: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("ServeConn did not stop")
			}
		})
	}
}

type failedListener struct{ err error }

func (l failedListener) Accept() (net.Conn, error) { return nil, l.err }
func (failedListener) Close() error                { return nil }
func (failedListener) Addr() net.Addr              { return nil }

func TestServerEventAcceptAndAdmissionErrors(t *testing.T) {
	events := make(chan *ServerEvent, 4)
	s := New(nil, &Options{OnError: func(_ context.Context, e *ServerEvent) { events <- e }, Limits: Limits{MaxConnections: 1}})
	cause := errors.New("accept failed")
	if err := s.Serve(context.Background(), failedListener{cause}, nil); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if event := awaitServerEvent(t, events); event.Kind != ServerEventAcceptError || event.Conn != nil || !errors.Is(event.Err, cause) {
		t.Fatalf("event=%+v", event)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c, _ := newConn(context.Background(), s, a)
	if err := s.register(c); err != nil {
		t.Fatal(err)
	}
	defer s.unregister(c)
	x, y := net.Pipe()
	defer y.Close()
	if err := s.ServeConn(context.Background(), x, nil); !errors.Is(err, errConnectionLimit) {
		t.Fatal(err)
	}
	if event := awaitServerEvent(t, events); event.Kind != ServerEventConnectionLimit || !errors.Is(event.Err, errConnectionLimit) {
		t.Fatalf("event=%+v", event)
	}
}
