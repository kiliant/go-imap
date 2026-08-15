package imapserver_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// Whole-server fuzzing. The decoder targets in internal/imapwire and
// internal/imapcodec cover parsing in isolation; these drive Server.ServeConn,
// which is the surface an unauthenticated remote peer actually reaches — the
// larger and more exposed threat surface of the two, per SERVER-DESIGN.md §8.
//
// The bar is T13's: no panic, no hang, no unbounded allocation. "No hang" is
// the reason ServeConn is watched with an explicit timeout rather than left to
// the go test binary's own -timeout: a wedged connection reported as a package
// timeout names no target and no input, so it is indistinguishable from a slow
// machine and gets triaged as flake.

const (
	// fuzzHangTimeout is how long ServeConn may take to return after the peer
	// has gone away. Orders of magnitude above the microseconds a real
	// execution needs, so it fires on a wedge and not on a loaded machine.
	fuzzHangTimeout = 20 * time.Second
	// fuzzPipeTimeout bounds the harness' own pipe operations, so a test-side
	// stall can never be misreported as a server hang.
	fuzzPipeTimeout = 10 * time.Second
)

// fuzzServerLimits keeps one execution cheap and puts the resource bounds
// themselves under test. Deliberately far below the production defaults: an
// input that allocates past these is a finding, and at the defaults the same
// input would simply be slow. Nothing here raises a limit — SERVER-DESIGN.md
// §8's rule is that a test may not route around the bounds it is validating.
func fuzzServerLimits() imapserver.Limits {
	return imapserver.Limits{
		MaxCommandLineBytes:   4 << 10,
		MaxLiteralBytes:       8 << 10,
		MaxQueuedCommands:     16,
		MaxQueuedCommandBytes: 64 << 10,
		MaxCommands:           64,
		MaxQueuedUpdates:      16,
		MaxQueuedUpdateBytes:  64 << 10,
		MaxSelectedMessages:   256,
		PreAuthTimeout:        5 * time.Second,
		CommandTimeout:        5 * time.Second,
		ReadTimeout:           5 * time.Second,
		WriteTimeout:          5 * time.Second,
	}
}

// driveServer feeds prologue then input to a fresh server over a pipe, closes
// the client end, and requires ServeConn to return.
//
// The prologue is written the same way as the fuzz input rather than through
// imapclient: a fuzz target that needed the client to agree the bytes were
// well-formed could not reach the states a hostile peer reaches by sending
// something the client would never send.
func driveServer(t *testing.T, prologue, input []byte) {
	t.Helper()

	// The second account is not a duplicate. Part of this corpus is captured
	// traffic from real clients driven against the interop server
	// (imapserver/interop/capture_test.go), and those sessions carry that
	// server's credentials. Without the account here every captured seed would
	// stop at a failed LOGIN, and the authenticated command surface they exist
	// to reach would never be entered.
	backend := memory.New(&memory.Options{Users: map[string]string{
		"alice":                "secret",
		"interop@example.test": "interop-pw",
	}})
	server := imapserver.New(backend, &imapserver.Options{
		AllowInsecureAuth: true,
		Limits:            fuzzServerLimits(),
	})
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), fuzzPipeTimeout)
		defer cancel()
		_ = server.Close(closeCtx, nil)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), fuzzPipeTimeout)
	defer cancel()

	clientConn, serverConn := net.Pipe()
	// net.Pipe is synchronous and unbuffered, so every operation on the client
	// end gets a deadline. Without one, a server that stops reading turns the
	// harness' own Write into the hang instead of the server's loop.
	_ = clientConn.SetDeadline(time.Now().Add(fuzzPipeTimeout))

	served := make(chan error, 1)
	go func() { served <- server.ServeConn(ctx, serverConn, nil) }()

	// Drain concurrently with the write. The server emits its greeting before
	// reading anything, so a harness that wrote first would deadlock against
	// an unread greeting on every single execution.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(io.Discard, clientConn)
	}()

	if len(prologue) > 0 {
		_, _ = clientConn.Write(prologue)
	}
	_, _ = clientConn.Write(input)
	// EOF is the signal under test: a peer that sends a partial command and
	// vanishes must not leave the session waiting for the rest of it.
	_ = clientConn.Close()

	select {
	case <-served:
	case <-time.After(fuzzHangTimeout):
		t.Fatalf("ServeConn did not return within %s of the peer closing", fuzzHangTimeout)
	}
	<-drained
}

// TestAppendWithoutLiteralIsRejected pins the protocol answer to the crasher
// FuzzServeConnAuthenticated found. The corpus entry alone would only prove the
// process stopped dying; the connection also owes the client a tagged BAD and
// has to stay usable for the next command, which is what actually distinguishes
// the fix from catching the panic and hanging up.
func TestAppendWithoutLiteralIsRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	backend := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})
	clientConn, serverConn := net.Pipe()
	_ = clientConn.SetDeadline(time.Now().Add(15 * time.Second))
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(ctx, serverConn, nil) }()

	reader := bufio.NewReader(clientConn)
	if _, err := reader.ReadString('\n'); err != nil { // greeting
		t.Fatal(err)
	}
	if _, err := io.WriteString(clientConn, "a LOGIN alice secret\r\n"); err != nil {
		t.Fatal(err)
	}
	if line := readTagged(t, reader, "a"); !strings.HasPrefix(line, "a OK") {
		t.Fatalf("LOGIN = %q", line)
	}

	// The mistyped literal: "23+}" never opens with "{".
	if _, err := io.WriteString(clientConn, "b APPEND INBOX 23+}\r\n"); err != nil {
		t.Fatal(err)
	}
	if line := readTagged(t, reader, "b"); !strings.HasPrefix(line, "b BAD") {
		t.Fatalf("malformed APPEND = %q, want a tagged BAD", line)
	}

	// Still usable: a rejected command must not have desynchronised the wire.
	if _, err := io.WriteString(clientConn, "c NOOP\r\n"); err != nil {
		t.Fatal(err)
	}
	if line := readTagged(t, reader, "c"); !strings.HasPrefix(line, "c OK") {
		t.Fatalf("NOOP after malformed APPEND = %q", line)
	}

	_, _ = io.WriteString(clientConn, "d LOGOUT\r\n")
	_ = clientConn.Close()
	select {
	case <-served:
	case <-time.After(fuzzHangTimeout):
		t.Fatal("ServeConn did not return")
	}
}

// readTagged reads until the line carrying tag, skipping untagged data.
func readTagged(t *testing.T, reader *bufio.Reader, tag string) string {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("waiting for tag %q: %v", tag, err)
		}
		if strings.HasPrefix(line, tag+" ") {
			return strings.TrimRight(line, "\r\n")
		}
	}
}

// FuzzServeConnPreAuth drives bytes at a connection that has authenticated
// nothing — the state a remote attacker starts in.
func FuzzServeConnPreAuth(f *testing.F) {
	for _, seed := range []string{
		"a CAPABILITY\r\n",
		"a NOOP\r\na LOGOUT\r\n",
		"a LOGIN alice secret\r\n",
		"a LOGIN alice wrong\r\n",
		"a AUTHENTICATE PLAIN\r\nAGFsaWNlAHNlY3JldA==\r\n",
		"a AUTHENTICATE SCRAM-SHA-256\r\nbiwsbj1hbGljZSxyPWNub25jZQ==\r\n",
		"a SELECT INBOX\r\n",
		"a STARTTLS\r\na LOGIN alice secret\r\n",
		"a LOGIN alice {6}\r\nsecret\r\n",
		"a LOGIN alice {6+}\r\nsecret\r\n",
		"a ENABLE IMAP4rev2\r\n",
		"a ID NIL\r\n",
		"",
		"\r\n",
		"a",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		driveServer(t, nil, input)
	})
}

// FuzzServeConnAuthenticated prefixes a successful login, so the far larger
// authenticated and selected-state command surface — everything T22 and T23
// added — is reachable by the fuzzer instead of being gated behind a
// credential it would have to guess.
func FuzzServeConnAuthenticated(f *testing.F) {
	for _, seed := range []string{
		"b SELECT INBOX\r\n",
		"b LIST \"\" *\r\n",
		"b STATUS INBOX (MESSAGES UIDNEXT UIDVALIDITY)\r\n",
		"b APPEND INBOX {23+}\r\nSubject: x\r\n\r\nbody\r\n",
		"b SELECT INBOX\r\nc FETCH 1:* (UID FLAGS BODY[])\r\n",
		"b SELECT INBOX\r\nc UID SEARCH ALL\r\n",
		"b SELECT INBOX\r\nc STORE 1 +FLAGS (\\Seen)\r\n",
		"b SELECT INBOX\r\nc UID EXPUNGE 1\r\n",
		"b IDLE\r\nDONE\r\n",
		"b ENABLE CONDSTORE QRESYNC\r\nc SELECT INBOX\r\n",
		"b CREATE Archive\r\nc RENAME Archive Other\r\nd DELETE Other\r\n",
		"b GETQUOTAROOT INBOX\r\n",
		"b SETMETADATA INBOX (/private/comment \"x\")\r\n",
		"b SELECT INBOX\r\nc SORT (SUBJECT) UTF-8 ALL\r\n",
		"b SELECT INBOX\r\nc THREAD REFERENCES UTF-8 ALL\r\n",
		"b NAMESPACE\r\n",
		"",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		driveServer(t, []byte("a LOGIN alice secret\r\n"), input)
	})
}
