package message_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver/message"
)

func TestBinarySectionsDecodeAndSlice(t *testing.T) {
	for _, tc := range []struct{ name, media, cte, body, want string }{
		{"base64", "application/octet-stream", "base64", "YWJjAGRlZg==", "abc\x00def"},
		{"quoted-printable", "application/octet-stream", "quoted-printable", "a=00b=0Ac", "a\x00b\nc"},
		{"identity", "application/octet-stream", "binary", "a\x00b\nc", "a\x00b\nc"},
		{"text", "text/plain", "base64", base64.StdEncoding.EncodeToString([]byte("a\nb\rc\r\nd\r")), "a\r\nb\r\nc\r\nd\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "Content-Type: " + tc.media + "\r\nContent-Transfer-Encoding: " + tc.cte + "\r\n\r\n" + tc.body
			m, err := message.Analyze(t.Context(), strings.NewReader(raw), int64(len(raw)), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, partial := range []*imap.SectionPartial{nil, {Offset: 1, Size: 3}, {Offset: 100, Size: 2}} {
				item := &imap.FetchItemBinarySection{Part: []int{1}, Peek: true, Partial: partial}
				r, size, err := m.OpenBinarySection(t.Context(), item, nil)
				if err != nil {
					t.Fatal(err)
				}
				want := tc.want
				if partial != nil {
					start := min(int(partial.Offset), len(want))
					want = want[start:min(start+int(partial.Size), len(want))]
				}
				var got strings.Builder
				// Force split CRLFs and decoder buffering boundaries.
				if _, err := io.CopyBuffer(&got, &oneByteReader{r}, make([]byte, 1)); err != nil {
					t.Fatal(err)
				}
				if got.String() != want || size != int64(len(want)) {
					t.Fatalf("binary section = %q (%d), want %q", got.String(), size, want)
				}
			}
		})
	}
}

type oneByteReader struct{ io.Reader }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

func TestBinaryNestedPartsAndRefusals(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: message/rfc822\r\n\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nYQBi\r\n--x--\r\n"
	m, err := message.Analyze(t.Context(), strings.NewReader(raw), int64(len(raw)), nil)
	if err != nil {
		t.Fatal(err)
	}
	r, size, err := m.OpenBinarySection(t.Context(), &imap.FetchItemBinarySection{Part: []int{1, 1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "a\x00b" || size != 3 {
		t.Fatalf("nested binary = %q (%d), %v", got, size, err)
	}
	for _, item := range []*imap.FetchItemBinarySection{nil, {}, {Part: []int{1}}, {Part: []int{0}}, {Part: []int{2}}, {Part: []int{1, 1}, Partial: &imap.SectionPartial{Size: 0}}} {
		if _, _, err := m.OpenBinarySection(t.Context(), item, nil); err == nil {
			t.Fatalf("invalid BINARY section accepted: %+v", item)
		}
	}
	for _, cte := range []string{"x-future", "base64"} {
		raw := "Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: " + cte + "\r\n\r\n!invalid"
		m, err := message.Analyze(t.Context(), strings.NewReader(raw), int64(len(raw)), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = m.OpenBinarySection(t.Context(), &imap.FetchItemBinarySection{Part: []int{1}}, nil)
		var failure *imap.Error
		if !errors.As(err, &failure) || failure.Code != imap.CodeUnknownCTE {
			t.Fatalf("%s refusal = %v", cte, err)
		}
	}
}

func TestBinarySourceLifetimeAndCancellation(t *testing.T) {
	source := &syntheticSource{header: "Content-Type: application/octet-stream\r\n\r\n", size: 200 << 10}
	m, err := message.Analyze(t.Context(), source, source.size, nil)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("binary source failed")
	source.hook = func(int64) error { return failure }
	if _, _, err := m.OpenBinarySection(t.Context(), &imap.FetchItemBinarySection{Part: []int{1}}, nil); !errors.Is(err, failure) {
		t.Fatalf("source error identity lost: %v", err)
	}
	source.hook = nil
	ctx, cancel := context.WithCancel(t.Context())
	r, _, err := m.OpenBinarySection(ctx, &imap.FetchItemBinarySection{Part: []int{1}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reader = %v", err)
	}
	if _, _, err := m.OpenBinarySection(ctx, &imap.FetchItemBinarySection{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled open = %v", err)
	}
	if _, _, err := m.OpenBinarySection(t.Context(), &imap.FetchItemBinarySection{Part: []int{1}}, nil); err != nil {
		t.Fatalf("cancelling one operation poisoned Message: %v", err)
	}
}

func TestBinaryEmptySectionPreservesHeaders(t *testing.T) {
	header := "Subject: =?utf-8?b?YWJj?=\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\n"
	raw := header + "YQBi"
	m, err := message.Analyze(t.Context(), strings.NewReader(raw), int64(len(raw)), nil)
	if err != nil {
		t.Fatal(err)
	}
	r, size, err := m.OpenBinarySection(t.Context(), &imap.FetchItemBinarySection{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	want := header + "a\x00b"
	if err != nil || string(got) != want || size != int64(len(want)) {
		t.Fatalf("empty BINARY section = %q (%d), %v", got, size, err)
	}
}
