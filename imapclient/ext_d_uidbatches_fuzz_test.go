package imapclient

import (
	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
	"testing"
)

func FuzzUIDBatchesCollector(f *testing.F) {
	for _, wire := range []string{" (TAG A1) 9:1\r\n", " (TAG foreign)\r\n", " (TAG {2}\r\nA1) 5\r\n", " 9:1\r\n", " (TAG A1) *:0\r\n"} {
		f.Add(wire)
	}
	f.Fuzz(func(t *testing.T, wire string) {
		var first, second *imap.UIDBatchesData
		resp := &untaggedResponse{name: "UIDBATCHES", dec: imapwire.NewDecoderString(wire, nil)}
		a, err := uidBatchesCollector("A1", &first)(resp)
		if err != nil {
			return
		}
		b, err := uidBatchesCollector("foreign", &second)(resp)
		if err != nil {
			t.Fatal(err)
		}
		if a && b {
			t.Fatal("two collectors claimed one response")
		}
		if first != nil && first.Tag != "A1" || second != nil && second.Tag != "foreign" {
			t.Fatal("foreign response claimed")
		}
	})
}
