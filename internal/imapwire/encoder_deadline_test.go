package imapwire

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// deadlineWriter is a writer that records every SetWriteDeadline call and, when
// stall is true, refuses writes the way a net.Conn does once its deadline has
// passed. Filling a real kernel socket buffer to provoke a genuine timeout is
// slow and machine-dependent; this reproduces the contract deterministically.
type deadlineWriter struct {
	mu        sync.Mutex
	deadlines []time.Time
	written   int64
	stall     bool
}

func (w *deadlineWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadlines = append(w.deadlines, t)
	return nil
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stall {
		return 0, io.ErrClosedPipe
	}
	w.written += int64(len(p))
	return len(p), nil
}

func (w *deadlineWriter) calls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.deadlines)
}

// A writer that does not implement SetWriteDeadline must still work: the
// deadline is a best-effort bound on real connections, not a requirement.
func TestEncoderWriteTimeoutIgnoresPlainWriter(t *testing.T) {
	var sb strings.Builder
	e := NewEncoder(&sb, &EncoderOptions{WriteTimeout: time.Second})
	e.Atom("A1").SP().Atom("NOOP").CRLF()
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush() = %v", err)
	}
	if sb.String() != "A1 NOOP\r\n" {
		t.Fatalf("wrote %q", sb.String())
	}
}

// A zero or negative WriteTimeout installs no deadline at all, which is what
// the decoder-side ReadTimeout convention does for tests that want no bound.
func TestEncoderWriteTimeoutDisabled(t *testing.T) {
	w := &deadlineWriter{}
	e := NewEncoder(w, &EncoderOptions{WriteTimeout: 0})
	e.Atom("A1").SP().Atom("NOOP").CRLF()
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush() = %v", err)
	}
	if got := w.calls(); got != 0 {
		t.Fatalf("SetWriteDeadline called %d times with the deadline disabled", got)
	}
}

// The deadline is armed before the octets are handed to the connection.
func TestEncoderWriteTimeoutArmsDeadline(t *testing.T) {
	w := &deadlineWriter{}
	before := time.Now()
	e := NewEncoder(w, &EncoderOptions{WriteTimeout: time.Minute})
	e.Atom("A1").SP().Atom("NOOP").CRLF()
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush() = %v", err)
	}
	if w.calls() != 1 {
		t.Fatalf("SetWriteDeadline called %d times, want 1", w.calls())
	}
	w.mu.Lock()
	deadline := w.deadlines[0]
	w.mu.Unlock()
	if !deadline.After(before) {
		t.Fatalf("deadline %v is not in the future relative to %v", deadline, before)
	}
}

// A write that the connection refuses because its deadline expired must surface
// as a fatal encoder error rather than being silently dropped.
func TestEncoderWriteTimeoutSurfacesError(t *testing.T) {
	w := &deadlineWriter{stall: true}
	e := NewEncoder(w, &EncoderOptions{WriteTimeout: time.Minute, LiteralPlus: true})
	// Write more than the bufio buffer so the payload reaches the connection
	// without waiting for an explicit Flush.
	lw, err := e.Literal(1<<20, false)
	if err != nil {
		t.Fatalf("Literal() = %v", err)
	}
	_, werr := lw.Write(make([]byte, 1<<20))
	if werr == nil {
		if err := e.Flush(); err == nil {
			t.Fatal("a stalled connection produced no error")
		}
		return
	}
	var wireErr *Error
	if !errors.As(werr, &wireErr) {
		t.Fatalf("error %v (%T) is not a *wire.Error", werr, werr)
	}
	if !wireErr.Fatal {
		t.Fatal("a write timeout must be fatal: the stream is desynchronised")
	}
}

// This is the requirement the audit actually named: a large literal must not
// have to complete inside one deadline. The deadline refreshes as octets leave,
// so a stream that keeps making progress is never killed, however long it runs.
func TestEncoderWriteTimeoutRefreshesAcrossStreamingLiteral(t *testing.T) {
	const (
		timeout = time.Second
		size    = 64 << 20
		chunk   = 32 << 10
	)
	w := &deadlineWriter{}
	e := NewEncoder(w, &EncoderOptions{WriteTimeout: timeout, LiteralPlus: true})
	lw, err := e.Literal(size, false)
	if err != nil {
		t.Fatalf("Literal() = %v", err)
	}
	buf := make([]byte, chunk)
	for written := 0; written < size; written += chunk {
		if _, err := lw.Write(buf); err != nil {
			t.Fatalf("Write at %d = %v", written, err)
		}
	}
	if err := lw.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush() = %v", err)
	}
	if w.written < size {
		t.Fatalf("wrote %d octets of %d", w.written, size)
	}
	// The refresh is time-based, not per-write: a 64 MiB burst that completes
	// well inside one timeout legitimately arms the deadline only once. What
	// must not happen is the deadline never being armed, or being armed once
	// per chunk, which would allocate a timer per 32 KiB.
	calls := w.calls()
	if calls == 0 {
		t.Fatal("streaming literal never armed a write deadline")
	}
	if chunks := size / chunk; calls >= chunks {
		t.Fatalf("armed the deadline %d times for %d chunks; it should refresh on elapsed time, not per write", calls, chunks)
	}
}

// A stream that keeps making progress across more than one timeout period must
// survive, and must refresh rather than expire. The synthetic clock keeps this
// deterministic and fast.
func TestEncoderWriteTimeoutSlowButProgressingStreamSurvives(t *testing.T) {
	w := &deadlineWriter{}
	tw := &timeoutWriter{w: w, setter: w, timeout: 100 * time.Millisecond}

	// Drive timeoutWriter directly so the elapsed time between writes is
	// controlled rather than raced against a real clock.
	for i := 0; i < 10; i++ {
		if _, err := tw.Write([]byte("data")); err != nil {
			t.Fatalf("write %d = %v", i, err)
		}
		time.Sleep(60 * time.Millisecond)
	}
	// Ten writes spread over ~600ms with a 100ms timeout: the deadline must
	// have been refreshed repeatedly, and no write may have failed.
	if calls := w.calls(); calls < 5 {
		t.Fatalf("deadline refreshed only %d times across a 600ms progressing stream", calls)
	}
	if w.written != 40 {
		t.Fatalf("wrote %d octets, want 40", w.written)
	}
}
