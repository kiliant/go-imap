package imapcodec

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/kiliant/go-imap/internal/imapwire"
)

func FuzzUIDBatchesCodec(f *testing.F) {
	f.Add(" (TAG ~{1}\r\n\x00)\r\n")
	f.Add(" (TAG A1) " + strings.Repeat("1,", 3000) + "1\r\n")
	for _, wire := range []string{" (TAG A1) 9:5,4:4,3:1\r\n", " (TAG A1)\r\n", " (TAG {2}\r\nA1) 5\r\n", " (TAG A1) *:0\r\n", " 500 1:200\r\n", " 4294967295 2:1\r\n"} {
		f.Add(wire)
	}
	f.Fuzz(func(t *testing.T, wire string) {
		_, _, _ = ReadUIDBatchesCommand(imapwire.NewDecoderString(wire, nil))
		data, err := ReadUIDBatchesResponse(imapwire.NewDecoderString(wire, nil))
		if err != nil {
			return
		}
		var out bytes.Buffer
		enc := imapwire.NewEncoder(&out, &imapwire.EncoderOptions{ServerResponse: true})
		if err := WriteUIDBatchesResponse(enc, data); err != nil {
			t.Fatal(err)
		}
		if err := enc.Flush(); err != nil {
			t.Fatal(err)
		}
		got, err := ReadUIDBatchesResponse(imapwire.NewDecoderString(out.String(), &imapwire.Options{MaxLineLength: out.Len() + 1}))
		if err != nil || !reflect.DeepEqual(got, data) {
			t.Fatalf("roundtrip: %#v, %v", got, err)
		}
	})
}
