package imapserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type urlFetchFixture struct {
	URLAuthSession
	result *URLFetchResult
	err    error
}

func (s urlFetchFixture) FetchURLAuth(context.Context, string, *URLFetchOptions) (*URLFetchResult, error) {
	return s.result, s.err
}

type countedURLBody struct {
	reader        io.Reader
	reads, closes int
}

func (b *countedURLBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.reads += n
	return n, err
}
func (b *countedURLBody) Close() error { b.closes++; return nil }

func TestURLFetchSizedAndUnknownBodies(t *testing.T) {
	for _, test := range []struct {
		name, raw, wire string
		known           bool
		size, limit     int64
		tooBig, short   bool
	}{
		{name: "known", raw: "abc", wire: "{3}\r\nabc", known: true, size: 3, limit: 8},
		{name: "unknown", raw: "abc", wire: "{3}\r\nabc", limit: 8},
		{name: "empty", wire: "{0}\r\n", known: true, limit: 8},
		{name: "unknown-empty", wire: "{0}\r\n", limit: 8},
		{name: "known-too-big", raw: "123456789", known: true, size: 9, limit: 8, tooBig: true},
		{name: "unknown-too-big", raw: "123456789abcdef", limit: 8, tooBig: true},
		{name: "short-known", raw: "abc", known: true, size: 4, limit: 8, short: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &countedURLBody{reader: strings.NewReader(test.raw)}
			fixture := urlFetchFixture{result: &URLFetchResult{Body: body, HasSize: test.known, Size: test.size}}
			var wire bytes.Buffer
			c := &conn{encoder: imapwire.NewEncoder(&wire, &imapwire.EncoderOptions{ServerResponse: true})}
			remaining := test.limit
			framed, err := writeURLFetchContent(t.Context(), c, fixture, "url", &remaining)
			if body.closes != 1 {
				t.Fatalf("body closed %d times", body.closes)
			}
			if test.tooBig {
				var protocolErr *imap.Error
				if framed || !errors.As(err, &protocolErr) || protocolErr.Code != imap.CodeTooBig || body.reads > int(test.limit)+1 {
					t.Fatalf("oversize: framed=%t, err=%v, read=%d", framed, err, body.reads)
				}
				if test.known && body.reads != 0 {
					t.Fatal("read a known oversized body")
				}
				return
			}
			if test.short {
				if !framed || !errors.Is(err, io.EOF) {
					t.Fatalf("short body did not invalidate framing: %t %v", framed, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := c.encoder.Flush(); err != nil {
				t.Fatal(err)
			}
			if wire.String() != test.wire || remaining != test.limit-int64(len(test.raw)) {
				t.Fatalf("wire=%q, remaining=%d", wire.String(), remaining)
			}
		})
	}
}

func TestURLFetchClosesBodyReturnedWithError(t *testing.T) {
	body := &countedURLBody{reader: strings.NewReader("private")}
	fixture := urlFetchFixture{result: &URLFetchResult{Body: body}, err: errors.New("not authorized")}
	var wire bytes.Buffer
	c := &conn{encoder: imapwire.NewEncoder(&wire, &imapwire.EncoderOptions{ServerResponse: true})}
	remaining := int64(8)
	if framed, err := writeURLFetchContent(t.Context(), c, fixture, "url", &remaining); framed || !errors.Is(err, fixture.err) {
		t.Fatalf("backend failure was swallowed: framed=%v err=%v", framed, err)
	}
	if err := c.encoder.Flush(); err != nil {
		t.Fatal(err)
	}
	if wire.Len() != 0 || body.reads != 0 || body.closes != 1 {
		t.Fatalf("errored result leaked: %q reads=%d closes=%d", wire.String(), body.reads, body.closes)
	}
}
