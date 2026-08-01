package imapclient

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

// deafServer answers the greeting and then never reads again. net.Pipe is
// synchronous and unbuffered, so the client's next write blocks in Write until
// its deadline fires — a real timeout, not a simulated one. This is exactly the
// server behaviour the audit named: one that stops reading and would otherwise
// block a command for as long as the connection stayed open.
func deafServer(t *testing.T, writeTimeout time.Duration) *Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY IMAP4rev1] ready\r\n"))
		// Deliberately never read. Hold the conn open until the test ends.
		<-t.Context().Done()
	}()
	c := NewClient(clientConn, &Options{WriteTimeout: writeTimeout})
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	return c
}

// A server that stops reading must not block a command indefinitely.
func TestWriteTimeoutUnblocksStalledCommand(t *testing.T) {
	c := deafServer(t, 150*time.Millisecond)

	start := time.Now()
	cmd := c.Noop(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := cmd.Wait(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a command against a server that stopped reading returned no error")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the command was unblocked by the caller's context, not by the write deadline")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the write deadline took %v to fire", elapsed)
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Logf("error was %v (%T)", err, err)
	}
}

// Every protocol failure reaches the caller as *imap.Error. A write timeout is
// not an exception: see rule 5 in CLAUDE.md.
func TestWriteTimeoutSurfacesAsImapError(t *testing.T) {
	c := deafServer(t, 150*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.Noop(nil).Wait(ctx)
	if err == nil {
		t.Fatal("no error from a stalled write")
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) {
		t.Fatalf("error %v (%T) is not an *imap.Error", err, err)
	}
}

// A write timeout leaves the stream desynchronised: octets may be half written,
// and tls.Conn is explicitly undefined after one. The session must therefore be
// poisoned, never recovered. This is the distinction from a rejected literal,
// which does leave the stream synchronised and does discard-and-continue.
func TestWriteTimeoutPoisonsSession(t *testing.T) {
	c := deafServer(t, 150*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Noop(nil).Wait(ctx); err == nil {
		t.Fatal("no error from the first stalled write")
	}

	// The second command must fail immediately from the poisoned session rather
	// than blocking for another full write timeout.
	start := time.Now()
	err := c.Noop(nil).Wait(ctx)
	if err == nil {
		t.Fatal("a command on a poisoned session succeeded")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("the second command waited %v; the session was not poisoned by the write timeout", elapsed)
	}
}

// The default is applied when the caller leaves WriteTimeout unset, so a
// connection is bounded without the caller having to know the knob exists.
func TestWriteTimeoutDefaultApplied(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want time.Duration
	}{
		{name: "unset", opts: Options{}, want: defaultWriteTimeout},
		{name: "negative", opts: Options{WriteTimeout: -time.Second}, want: defaultWriteTimeout},
		{name: "explicit", opts: Options{WriteTimeout: 42 * time.Second}, want: 42 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.opts.encoderOptions().WriteTimeout; got != tc.want {
				t.Fatalf("WriteTimeout = %v, want %v", got, tc.want)
			}
		})
	}
}
