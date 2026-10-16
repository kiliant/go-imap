package message_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver/message"
)

func FuzzAnalyze(f *testing.F) {
	for _, seed := range []string{raw, "", "Content-Type: message/rfc822", "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n\r\nbody\r\n--x--\r\n"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		ctx := context.Background()
		m, err := message.Analyze(ctx, bytes.NewReader(raw), int64(len(raw)), nil)
		if err != nil {
			t.Fatal(err)
		}
		r, size, err := m.OpenBodySection(ctx, &imap.FetchItemBodySection{})
		if err != nil || size != int64(len(raw)) {
			t.Fatalf("full section = %d, %v", size, err)
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("message bytes changed: %v", err)
		}
		_, _ = message.Match(ctx, m, message.Metadata{RFC822Size: m.Size()}, imap.SearchString{Key: imap.SearchKeyBody, Value: "fuzz"}, nil)
		_ = m.Envelope()
		_ = m.BodyStructure()
	})
}
