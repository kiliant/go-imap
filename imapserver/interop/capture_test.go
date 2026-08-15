//go:build interop

package interop

// Fuzz-corpus capture from real third-party client traffic.
//
// T24 and T13 share a standing rule: seeds come from captured traffic, not from
// invention. The reason is that hand-written seeds encode what their author
// thought a client would send, which is the same blind spot the whole
// third-party exercise exists to escape — a fuzzer starting from our guesses
// explores outward from our guesses.
//
// mbsync and imaptest are the only real clients this repository can drive, so
// their sessions are the corpus. The capture is a proxy in front of the server
// rather than a hook inside it: nothing about the server changes to be
// recorded, so what lands in the corpus is exactly what a foreign client put on
// the wire.
//
// Capture is off by default and writes nothing unless GOIMAP_CAPTURE_CORPUS is
// set. A test that rewrote checked-in corpus files on every run would make the
// fuzz corpus a function of who last ran the interop suite.

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"
)

// captureEnv enables corpus capture. See the package comment for why it is not
// on by default.
const captureEnv = "GOIMAP_CAPTURE_CORPUS"

// corpusTarget is the fuzz target captured sessions seed. FuzzServeConnPreAuth
// is the right one rather than the authenticated target: a captured session is
// a whole connection starting from the greeting, including the client's own
// LOGIN, which is precisely the pre-authentication surface.
const corpusTarget = "FuzzServeConnPreAuth"

// recorder is a TCP proxy that forwards to the real server and keeps a copy of
// what each client wrote. Only the client-to-server direction is kept, because
// that is the only direction a fuzz target replays.
type recorder struct {
	listener net.Listener
	target   string

	mu       sync.Mutex
	sessions [][]byte
}

// newRecorder starts a proxy in front of targetPort and returns it. The proxy
// binds every interface for the same reason the server does: the client that
// dials it is inside a container.
func newRecorder(t *testing.T, targetPort string) *recorder {
	t.Helper()
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("recorder listen: %v", err)
	}
	r := &recorder{listener: listener, target: net.JoinHostPort("127.0.0.1", targetPort)}
	go r.accept()
	t.Cleanup(func() { _ = listener.Close() })
	return r
}

func (r *recorder) port() string {
	_, port, _ := net.SplitHostPort(r.listener.Addr().String())
	return port
}

func (r *recorder) accept() {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		go r.proxy(client)
	}
}

// proxy pumps one connection in both directions, teeing the client's half into
// a buffer. A failure to reach the server closes the client rather than failing
// a test: this runs on its own goroutine, where t.Fatal would be a data race
// against the test that is still running.
//
// Both directions half-close on EOF, and that is load-bearing rather than
// tidiness. A proxy that only forwards bytes and never forwards the *close*
// wedges every well-behaved client: mbsync sends LOGOUT, reads the tagged OK,
// and then waits for the connection to end — which never happens if the
// server's close stops at the proxy. The symptom is a client that hangs after a
// visibly complete and correct session, which reads exactly like a server bug
// and is not one.
func (r *recorder) proxy(client net.Conn) {
	defer client.Close()
	server, err := net.DialTimeout("tcp", r.target, 10*time.Second)
	if err != nil {
		return
	}
	defer server.Close()

	var captured capturedWriter
	done := make(chan struct{}, 2)
	var trace *os.File
	if dir := os.Getenv("GOIMAP_TRACE_DIR"); dir != "" {
		trace, _ = os.CreateTemp(dir, "session-*.log")
		if trace != nil {
			defer trace.Close()
		}
	}
	traceMu := &sync.Mutex{}
	go func() {
		defer func() { done <- struct{}{} }()
		var capture io.Writer = &captured
		if trace != nil {
			capture = io.MultiWriter(&captured, &directionWriter{mu: traceMu, file: trace, prefix: "C: "})
		}
		_, _ = io.Copy(server, io.TeeReader(client, capture))
		halfClose(server)
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		var destination io.Writer = client
		if trace != nil {
			destination = io.MultiWriter(client, &directionWriter{mu: traceMu, file: trace, prefix: "S: "})
		}
		_, _ = io.Copy(destination, server)
		halfClose(client)
	}()
	<-done
	<-done

	if buffer := captured.bytes(); len(buffer) > 0 {
		r.mu.Lock()
		r.sessions = append(r.sessions, buffer)
		r.mu.Unlock()
	}
}

type directionWriter struct {
	mu      *sync.Mutex
	file    *os.File
	prefix  string
	partial []byte
}

func (w *directionWriter) Write(p []byte) (int, error) {
	w.partial = append(w.partial, p...)
	for {
		index := bytes.IndexByte(w.partial, '\n')
		if index < 0 {
			break
		}
		line := bytes.TrimRight(w.partial[:index], "\r")
		w.mu.Lock()
		_, _ = fmt.Fprintf(w.file, "%s%s\n", w.prefix, line)
		w.mu.Unlock()
		w.partial = w.partial[index+1:]
	}
	return len(p), nil
}

// halfClose shuts down the write half of a TCP connection, so the peer reads
// EOF while anything still in flight the other way survives. A full Close here
// would truncate the direction that has not finished.
func halfClose(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}

// capturedWriter accumulates what a client wrote, up to a bound. The bound
// exists because a sync of a large mailbox is mostly message payload, and a
// multi-megabyte seed makes every fuzz execution slow without covering any
// parser state the first few kilobytes did not already reach.
type capturedWriter struct {
	mu     sync.Mutex
	buffer []byte
}

const maxCapturedSession = 64 << 10

func (w *capturedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if remaining := maxCapturedSession - len(w.buffer); remaining > 0 {
		w.buffer = append(w.buffer, p[:min(remaining, len(p))]...)
	}
	return len(p), nil
}

func (w *capturedWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer
}

// writeCorpus writes the recorded sessions into the fuzz seed corpus, named for
// the client that produced them. It is a no-op unless capture is enabled.
func (r *recorder) writeCorpus(t *testing.T, client string) {
	t.Helper()
	if os.Getenv(captureEnv) == "" {
		return
	}
	r.mu.Lock()
	sessions := r.sessions
	r.mu.Unlock()
	if len(sessions) == 0 {
		t.Errorf("capture was requested but %s produced no traffic", client)
		return
	}
	// Relative to this package, so it lands beside the target it seeds rather
	// than beside the interop test that captured it.
	dir := filepath.Join("..", "testdata", "fuzz", corpusTarget)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", dir, err)
	}
	for i, session := range pickCorpusSessions(sessions) {
		name := filepath.Join(dir, fmt.Sprintf("captured-%s-%d", client, i))
		body := "go test fuzz v1\n[]byte(" + strconv.Quote(string(session)) + ")\n"
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		t.Logf("captured %d bytes of %s traffic into %s", len(session), client, name)
	}
}

// maxCorpusSessions bounds how many captured sessions are kept per client.
const maxCorpusSessions = 6

// pickCorpusSessions deduplicates and trims a run's sessions down to a corpus
// worth committing.
//
// imaptest opens a connection per simulated client and issues a randomised
// command mix, so an unbounded capture writes dozens of files whose contents
// differ on every run — a corpus that churns in every diff and reviews as
// noise. Longest-first keeps the sessions that reached the most command
// shapes, which is what a seed is for.
func pickCorpusSessions(sessions [][]byte) [][]byte {
	unique := make([][]byte, 0, len(sessions))
	seen := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		if key := string(session); !seen[key] {
			seen[key] = true
			unique = append(unique, session)
		}
	}
	slices.SortStableFunc(unique, func(a, b []byte) int { return len(b) - len(a) })
	return unique[:min(maxCorpusSessions, len(unique))]
}
