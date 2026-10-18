package imapclient

import (
	"context"
	"testing"
	"time"
)

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
