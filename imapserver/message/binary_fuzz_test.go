package message_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver/message"
)

func FuzzOpenBinarySection(f *testing.F) {
	for _, raw := range []string{"\r\na\nb\r\nc\r", "Content-Transfer-Encoding: base64\r\n\r\nYQBi", "Content-Transfer-Encoding: quoted-printable\r\n\r\na=00b=0Ac"} {
		f.Add([]byte(raw), uint8(1), uint16(0), uint16(16))
	}
	f.Fuzz(func(t *testing.T, raw []byte, part uint8, offset, size uint16) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		m, err := message.Analyze(t.Context(), bytes.NewReader(raw), int64(len(raw)), nil)
		if err != nil {
			t.Fatal(err)
		}
		item := &imap.FetchItemBinarySection{Partial: &imap.SectionPartial{Offset: int64(offset), Size: int64(size)}}
		if part != 0 {
			item.Part = []int{int(part)}
		}
		r, n, err := m.OpenBinarySection(t.Context(), item, nil)
		if err != nil {
			return
		}
		got, err := io.Copy(io.Discard, r)
		if err != nil || got != n || n > int64(size) {
			t.Fatalf("binary section size %d: read %d, %v", n, got, err)
		}
	})
}
