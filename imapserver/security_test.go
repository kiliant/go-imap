package imapserver_test

// Stateful security tests: SERVER-DESIGN.md §7's list, each one a named
// historical vulnerability class that parser fuzzing structurally cannot reach.
// A fuzzer varies the bytes on a connection; these vary what the *peer does* —
// vanishing mid-literal, refusing to read, retrying authentication — which is
// where the connection lifecycle bugs live.
//
// Three of §7's items are deliberately absent because T19 already owns them and
// the spec asks for de-duplication rather than a second copy:
//
//   - STARTTLS plaintext command injection: TestStartTLSDiscardsBufferedPlaintextCommands
//   - update-queue overflow under a non-reading client: TestUpdateOverflowForceClosesBlockedWriter
//   - disconnect cancelling a blocked backend call: TestReaderCancelsBlockedEventLoopOnDisconnect

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// TestMain adds the suite-teardown leak detection §7 asks for. Both checks are
// whole-suite rather than per-test on purpose: a connection is torn down by
// several goroutines that unwind independently, so the only point at which
// "nothing is left behind" is unambiguous is after every test has finished.
func TestMain(m *testing.M) {
	before := tempSpoolFiles()
	code := m.Run()
	if code == 0 {
		if leaked := leakedGoroutines(); leaked != "" {
			fmt.Fprintf(os.Stderr, "goroutines leaked after the suite:\n%s\n", leaked)
			code = 1
		}
		// The FETCH spool (cmd_common.go) is the one path that puts a file on
		// disk. A connection that dies mid-FETCH must still remove it, or a
		// long-lived server accumulates them until the filesystem fills — a
		// denial of service that leaves no trace in the protocol.
		if after := tempSpoolFiles(); len(after) > len(before) {
			fmt.Fprintf(os.Stderr, "FETCH spool files leaked: %v\n", after)
			code = 1
		}
	}
	os.Exit(code)
}

func tempSpoolFiles() []string {
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "go-imap-fetch-*"))
	return matches
}

// leakedGoroutines polls until the goroutine count settles, then reports any
// stack still mentioning this module. Polling rather than sampling once is what
// keeps it from failing on a connection that is merely halfway through an
// orderly shutdown.
func leakedGoroutines() string {
	for attempt := range 50 {
		stacks := goroutineStacks()
		if !strings.Contains(stacks, "kiliant/go-imap") {
			return ""
		}
		if attempt == 49 {
			return stacks
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ""
}

func goroutineStacks() string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	var interesting []string
	for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
		// The test binary's own goroutines are not leaks. That includes the one
		// running this function: it is in package imapserver_test, so it
		// matches the module filter below and would otherwise report itself.
		if strings.Contains(stack, "imapserver_test.goroutineStacks") ||
			strings.Contains(stack, "testing.(*M).Run") ||
			strings.Contains(stack, "_testmain.go") {
			continue
		}
		if strings.Contains(stack, "kiliant/go-imap") {
			interesting = append(interesting, stack)
		}
	}
	return strings.Join(interesting, "\n\n")
}

// securityHarness is one authenticated connection to a fresh server.
type securityHarness struct {
	t      *testing.T
	client net.Conn
	reader *bufio.Reader
	served chan error
	cancel context.CancelFunc
	// returned records that ServeConn's single result has already been
	// observed. Tests assert the unwind themselves and Cleanup asserts it for
	// the ones that do not, so without this the second read waits on a channel
	// that will never carry a second value.
	returned bool
}

func newSecurityHarness(t *testing.T, limits imapserver.Limits) *securityHarness {
	t.Helper()
	backend := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	server := imapserver.New(backend, &imapserver.Options{
		AllowInsecureAuth: true,
		Limits:            limits,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	clientConn, serverConn := net.Pipe()
	_ = clientConn.SetDeadline(time.Now().Add(30 * time.Second))
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(ctx, serverConn) }()
	h := &securityHarness{t: t, client: clientConn, reader: bufio.NewReader(clientConn), served: served, cancel: cancel}
	t.Cleanup(func() {
		_ = clientConn.Close()
		h.requireServeReturns()
		cancel()
	})
	if _, err := h.reader.ReadString('\n'); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	return h
}

func (h *securityHarness) login() {
	h.t.Helper()
	h.write("a LOGIN alice secret\r\n")
	if line := readTagged(h.t, h.reader, "a"); !strings.HasPrefix(line, "a OK") {
		h.t.Fatalf("LOGIN = %q", line)
	}
}

func (h *securityHarness) write(s string) {
	h.t.Helper()
	if _, err := io.WriteString(h.client, s); err != nil {
		h.t.Fatalf("write %q: %v", s, err)
	}
}

// requireServeReturns is the hang check. Every test here ends by proving the
// connection actually unwound, because "the server survived" and "the server
// leaked a wedged connection per attack" look identical from the client.
func (h *securityHarness) requireServeReturns() {
	h.t.Helper()
	if h.returned {
		return
	}
	select {
	case <-h.served:
		h.returned = true
	case <-time.After(20 * time.Second):
		h.t.Error("ServeConn did not return after the peer disconnected")
	}
}

// TestIncompleteLiteralThenDisconnect covers the classic: announce a literal,
// send none of it, vanish. The reader is parked waiting for bytes that will
// never arrive, and must notice the close rather than hold the connection —
// and its announced buffer — until a timeout expires.
func TestIncompleteLiteralThenDisconnect(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{})
	h.login()
	h.write("b APPEND INBOX {4096}\r\n")
	// Do not wait for the continuation and do not send the payload.
	if err := h.client.Close(); err != nil {
		t.Fatal(err)
	}
	h.requireServeReturns()
}

// TestDisconnectDuringAppendPayload sends part of a literal and then vanishes,
// which is the harder half of the same class: the payload reader is mid-stream
// with a byte count it will never reach, and the backend already holds an open
// Append call.
func TestDisconnectDuringAppendPayload(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{})
	h.login()
	h.write("b APPEND INBOX {4096}\r\n")
	line, err := h.reader.ReadString('\n')
	if err != nil {
		t.Fatalf("continuation: %v", err)
	}
	if !strings.HasPrefix(line, "+") {
		t.Fatalf("expected a continuation request, got %q", line)
	}
	h.write(strings.Repeat("x", 100)) // 4096 announced, 100 delivered.
	if err := h.client.Close(); err != nil {
		t.Fatal(err)
	}
	h.requireServeReturns()
}

// TestSlowReaderDuringLargeFetch is the resource-exhaustion direction: a client
// asks for a large body and then stops reading. The server must not buffer the
// response without bound while waiting, and when the client finally goes away
// it must release the FETCH spool file — which TestMain then confirms globally.
func TestSlowReaderDuringLargeFetch(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{
		MaxLiteralBytes: 1 << 20,
		WriteTimeout:    2 * time.Second,
	})
	h.login()

	body := strings.Repeat("abcdefgh", 64<<10) // 512 KiB
	message := "Subject: large\r\n\r\n" + body
	// A synchronising literal, because this server advertises LITERAL- rather
	// than LITERAL+ and so caps an unsolicited non-synchronising literal at
	// 4 KiB. Sending 512 KiB as "{n+}" is refused, correctly, by that bound.
	h.write(fmt.Sprintf("b APPEND INBOX {%d}\r\n", len(message)))
	if line, err := h.reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+") {
		t.Fatalf("continuation = %q, %v", line, err)
	}
	h.write(message + "\r\n")
	if line := readTagged(t, h.reader, "b"); !strings.HasPrefix(line, "b OK") {
		t.Fatalf("APPEND = %q", line)
	}
	h.write("c SELECT INBOX\r\n")
	if line := readTagged(t, h.reader, "c"); !strings.HasPrefix(line, "c OK") {
		t.Fatalf("SELECT = %q", line)
	}

	// Ask for the whole body, read one line of it, then stall. The write
	// deadline is what has to fire; without it this connection is pinned for as
	// long as the peer cares to hold it, which is the whole attack.
	h.write("d FETCH 1 (BODY[])\r\n")
	if _, err := h.reader.ReadString('\n'); err != nil {
		t.Fatalf("first response line: %v", err)
	}
	if err := h.client.Close(); err != nil {
		t.Fatal(err)
	}
	h.requireServeReturns()
}

// TestRepeatedFailedAuthentication checks that a connection cannot be used as
// an unbounded password-guessing channel. MaxCommands is the bound that applies
// before authentication, so the connection must be closed rather than serving
// attempt after attempt.
func TestRepeatedFailedAuthentication(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{MaxCommands: 8})
	failures := 0
	for attempt := range 40 {
		if _, err := io.WriteString(h.client, fmt.Sprintf("t%d LOGIN alice wrong%d\r\n", attempt, attempt)); err != nil {
			break // The server hung up, which is the outcome under test.
		}
		line, err := h.reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.Contains(line, " NO ") || strings.Contains(line, " BAD ") {
			failures++
		}
	}
	if failures > 8 {
		t.Fatalf("server served %d failed authentications on one connection, want at most MaxCommands=8", failures)
	}
	_ = h.client.Close()
	h.requireServeReturns()
}

// TestSelectCloseUpdateRace drives selection churn against a second session
// mutating the same mailbox. The race it looks for is a selection torn down
// while an update for it is in flight — reported by -race, which is why this
// test's value is entirely in being run with it.
func TestSelectCloseUpdateRace(t *testing.T) {
	backend := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The mutating session appends continuously; the churning session selects
	// and closes underneath it.
	mutator := newRawSession(t, ctx, server)
	defer mutator.close()
	churner := newRawSession(t, ctx, server)
	defer churner.close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 25 {
			message := fmt.Sprintf("Subject: race %d\r\n\r\nbody\r\n", i)
			mutator.write(fmt.Sprintf("m%d APPEND INBOX {%d+}\r\n%s\r\n", i, len(message), message))
			if line := readTagged(t, mutator.reader, fmt.Sprintf("m%d", i)); !strings.HasPrefix(line, "m") {
				return
			}
		}
	}()

	for i := range 25 {
		churner.write(fmt.Sprintf("s%d SELECT INBOX\r\n", i))
		_ = readTagged(t, churner.reader, fmt.Sprintf("s%d", i))
		churner.write(fmt.Sprintf("c%d CLOSE\r\n", i))
		_ = readTagged(t, churner.reader, fmt.Sprintf("c%d", i))
	}
	<-done
}

// rawSession is an authenticated connection without the per-test server that
// securityHarness creates, so several can share one server.
type rawSession struct {
	t      *testing.T
	conn   net.Conn
	reader *bufio.Reader
	served chan error
}

func newRawSession(t *testing.T, ctx context.Context, server *imapserver.Server) *rawSession {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	_ = clientConn.SetDeadline(time.Now().Add(30 * time.Second))
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(ctx, serverConn) }()
	s := &rawSession{t: t, conn: clientConn, reader: bufio.NewReader(clientConn), served: served}
	if _, err := s.reader.ReadString('\n'); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	s.write("a LOGIN alice secret\r\n")
	if line := readTagged(t, s.reader, "a"); !strings.HasPrefix(line, "a OK") {
		t.Fatalf("LOGIN = %q", line)
	}
	return s
}

func (s *rawSession) write(text string) {
	s.t.Helper()
	if _, err := io.WriteString(s.conn, text); err != nil {
		s.t.Fatalf("write: %v", err)
	}
}

func (s *rawSession) close() {
	_ = s.conn.Close()
	select {
	case <-s.served:
	case <-time.After(20 * time.Second):
		s.t.Error("ServeConn did not return")
	}
}
