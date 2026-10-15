package imapcodec

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/kiliant/go-imap/internal/imapwire"
)

func FuzzContextUpdateChanges(f *testing.F) {
	for _, wire := range []string{"", "()", "(0 9,4:2 1 7)", "(4294967295 1:4294967295)", "(0 *)", "(0 NIL)", "(0 0)", "(0 1 1)"} {
		f.Add(wire, false)
		f.Add(wire, true)
	}
	f.Fuzz(func(t *testing.T, wire string, uid bool) {
		dec := imapwire.NewDecoderString(wire+"\r\n", nil)
		want, err := ReadContextUpdateChanges(dec, uid)
		if err != nil || !dec.ExpectCRLF() {
			return
		}
		var out bytes.Buffer
		enc := imapwire.NewEncoder(&out, nil)
		if err := WriteContextUpdateChanges(enc, uid, want); err != nil {
			t.Fatal(err)
		}
		if err := enc.Flush(); err != nil {
			t.Fatal(err)
		}
		got, err := ReadContextUpdateChanges(imapwire.NewDecoderString(out.String(), nil), uid)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("round trip changed parsed update: %v", err)
		}
	})
}
