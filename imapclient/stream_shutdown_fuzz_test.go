package imapclient

import (
	"context"
	"io"
	"net"
	"runtime"
	"testing"
	"time"
)

// net.Pipe does not stop deadline timers on Close. The client's default
// 30-minute read deadline would therefore retain each pipe for the entire fuzz
// campaign, eventually exhausting the worker. These stream targets already
// bound the reader with a context and close the peer after writing, so their
// transport does not need a second read timer. Real read deadlines are covered
// separately by TestStalledLiteralObservesReadDeadline in internal/imapwire.
type fuzzStreamConn struct{ net.Conn }

func (fuzzStreamConn) SetReadDeadline(time.Time) error { return nil }

func fuzzStreamPipe() (net.Conn, net.Conn) {
	client, server := net.Pipe()
	return fuzzStreamConn{client}, server
}

func TestFuzzStreamConnectionsAreReclaimed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 10_000; i++ {
		clientConn, serverConn := fuzzStreamPipe()
		go func() {
			_, _ = io.WriteString(serverConn, "* OK ready\r\n")
			_ = serverConn.Close()
		}()
		c := NewClient(clientConn, nil)
		finished := fuzzReaderFinished(ctx, c.readerDone)
		_ = c.Close()
		if !finished {
			t.Fatal("reader did not finish on a closed connection")
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	// A deadline timer that survives Close retains roughly 1 KiB per pipe.
	// Allow runtime bookkeeping, but not one retained connection per input.
	if after.HeapAlloc > before.HeapAlloc+4<<20 {
		t.Fatalf("closed fuzz connections retained %d bytes", after.HeapAlloc-before.HeapAlloc)
	}
}

// fuzzReaderFinished bounds a live reader, then confirms whether shutdown has
// already finished before reporting a timeout. Under fuzzing load the worker
// may resume with both channels ready; select has no completion priority.
func fuzzReaderFinished(ctx context.Context, done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	case <-ctx.Done():
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}

func TestFuzzReaderCompletionBeatsExpiredDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	close(done)
	// Both become ready while a fuzz worker is descheduled. A completed reader
	// must not be reported as hung merely because select chooses the deadline.
	for i := 0; i < 1_000; i++ {
		if !fuzzReaderFinished(ctx, done) {
			t.Fatal("completed reader reported as hung after the deadline")
		}
	}
}

func TestFuzzReaderStillRunningTimesOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if fuzzReaderFinished(ctx, make(chan struct{})) {
		t.Fatal("reader that never finishes escaped the shutdown check")
	}
}

func TestFuzzReaderWaitsForShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { close(done) }()
	if !fuzzReaderFinished(ctx, done) {
		t.Fatal("reader shutdown was not observed")
	}
}
