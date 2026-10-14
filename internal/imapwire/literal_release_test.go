package imapwire

import (
	"io"
	"sync"
	"testing"
)

// TestLiteralReleasedReaderLeavesDecoderAlone pins the hand-off a streaming
// FETCH depends on. Once a literal has been read to its end, the decoder
// belongs to the parsing goroutine again and may already be opening the next
// literal. What a consumer does after the payload — the extra Read that
// io.Copy makes to see EOF, or a deferred Close that discards — must not touch
// decoder state. Under -race this fails if it does; a FETCH of several bodies
// raced exactly this way until the reader stopped calling into the decoder
// after release.
func TestLiteralReleasedReaderLeavesDecoderAlone(t *testing.T) {
	d := dec("{3}\r\nabc {3}\r\ndef\r\n")
	first, ok := d.Literal()
	if !ok {
		t.Fatalf("Literal(): %v", d.Err())
	}
	consumed := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 3)
		if _, err := io.ReadFull(first, buf); err != nil {
			t.Errorf("reading first payload: %v", err)
		}
		close(consumed)
		if n, err := first.Read(buf); n != 0 || err != io.EOF {
			t.Errorf("Read after payload = %d, %v; want 0, EOF", n, err)
		}
		if err := first.Discard(); err != nil {
			t.Errorf("Discard after payload: %v", err)
		}
	}()

	<-consumed
	if !d.ExpectSP() {
		t.Fatalf("SP: %v", d.Err())
	}
	second, ok := d.Literal()
	if !ok {
		t.Fatalf("second Literal(): %v", d.Err())
	}
	payload, err := io.ReadAll(second)
	if err != nil || string(payload) != "def" {
		t.Fatalf("second payload = %q, %v", payload, err)
	}
	wg.Wait()
	if !d.ExpectCRLF() {
		t.Fatalf("CRLF: %v", d.Err())
	}
}

// TestZeroLengthLiteralLeavesDecoderAlone covers the literal that never takes
// the interlock: the parser does not wait for an empty payload at all, so the
// consumer's EOF read races with whatever the parser does next.
func TestZeroLengthLiteralLeavesDecoderAlone(t *testing.T) {
	d := dec("{0}\r\n {3}\r\ndef\r\n")
	empty, ok := d.Literal()
	if !ok {
		t.Fatalf("Literal(): %v", d.Err())
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if n, err := empty.Read(make([]byte, 1)); n != 0 || err != io.EOF {
			t.Errorf("Read = %d, %v; want 0, EOF", n, err)
		}
		if err := empty.Discard(); err != nil {
			t.Errorf("Discard: %v", err)
		}
	}()
	if !d.ExpectSP() {
		t.Fatalf("SP: %v", d.Err())
	}
	next, ok := d.Literal()
	if !ok {
		t.Fatalf("next Literal(): %v", d.Err())
	}
	if _, err := io.ReadAll(next); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}
