package imapmessage

import (
	"bytes"
	"io"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func FuzzAnalyzeGenerateAndExtract(f *testing.F) {
	for _, seed := range []string{
		fixture,
		"Subject: plain\r\n\r\nbody\r\n",
		"Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n\r\na\r\n--x--\r\n",
		"broken header\n\xff\x00body",
	} {
		f.Add([]byte(seed), uint32(0), uint32(32))
	}
	f.Fuzz(func(t *testing.T, input []byte, offset, count uint32) {
		if len(input) > 1<<20 {
			t.Skip()
		}
		message, err := Analyze(bytes.NewReader(input), int64(len(input)))
		if err != nil {
			return
		}

		var wire bytes.Buffer
		enc := imapwire.NewEncoder(&wire, &imapwire.EncoderOptions{ServerResponse: true})
		imapcodec.WriteBodyStructure(enc, message.BodyStructure)
		if err := enc.Flush(); err != nil {
			t.Fatalf("encode generated BODYSTRUCTURE: %v", err)
		}
		got, err := imapcodec.ReadBodyStructure(imapwire.NewDecoder(bytes.NewReader(wire.Bytes()), nil))
		if err != nil || got == nil {
			t.Fatalf("parse generated BODYSTRUCTURE %q: %v", wire.Bytes(), err)
		}

		item := &imap.FetchItemBodySection{Partial: &imap.SectionPartial{Offset: int64(offset), Size: int64(count) + 1}}
		section, size, err := message.OpenBodySection(item)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(section)
		if err != nil || int64(len(b)) != size {
			t.Fatalf("section size = %d/%d, err = %v", len(b), size, err)
		}
		start := min(int64(offset), int64(len(input)))
		end := min(start+int64(count)+1, int64(len(input)))
		if !bytes.Equal(b, input[start:end]) {
			t.Fatalf("partial section differs")
		}
	})
}
