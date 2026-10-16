package imapcodec

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestUIDBatchesResponseRoundTrip(t *testing.T) {
	if _, err := ReadUIDBatchesResponse(imapwire.NewDecoderString(" (TAG ~{1}\r\n\x00)\r\n", nil)); err == nil {
		t.Fatal("accepted NUL tag")
	}
	var invalid bytes.Buffer
	encInvalid := imapwire.NewEncoder(&invalid, nil)
	if err := WriteUIDBatchesResponse(encInvalid, &imap.UIDBatchesData{Tag: "\x00"}); err == nil {
		t.Fatal("wrote NUL tag")
	}
	if err := encInvalid.Flush(); err != nil || invalid.Len() != 0 {
		t.Fatal("invalid tag emitted a prefix")
	}
	for _, wire := range []string{" (TAG A1) " + strings.Repeat("1,", 3000) + "1\r\n", " (TAG A1)\r\n", " (TAG A1) 9:5,4:4,3:1\r\n", " (TAG A1) 1:4294967295,9:3\r\n", " (TAG \"A1\") 5\r\n", " (TAG {2}\r\nA1) 5:5\r\n"} {
		data, err := ReadUIDBatchesResponse(imapwire.NewDecoderString(wire, nil))
		if err != nil {
			t.Fatalf("%q: %v", wire, err)
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
			t.Fatalf("roundtrip %q: %#v, %v", wire, got, err)
		}
		if len(data.Batches) == 1 && !bytes.Contains(out.Bytes(), []byte("5:5")) {
			t.Fatalf("singleton shortened: %q", out.String())
		}
	}
	for _, wire := range []string{"\r\n", " 5:1\r\n", " (TAG A1) *:1\r\n", " (TAG A1) 0:1\r\n", " (TAG A1) 1:0\r\n", " (TAG A1) 4294967296:1\r\n", " (TAG A1) 1,\r\n", " (X A1) 1:1\r\n", " (TAG A1 X A2)\r\n", " (TAG \"\")\r\n", " (TAG A1) 1:1 junk\r\n"} {
		if _, err := ReadUIDBatchesResponse(imapwire.NewDecoderString(wire, nil)); err == nil {
			t.Errorf("accepted %q", wire)
		}
	}
	var out bytes.Buffer
	if err := WriteUIDBatchesResponse(imapwire.NewEncoder(&out, nil), &imap.UIDBatchesData{Tag: "A1", Batches: []imap.UIDBatch{{Range: imap.UIDRange{Start: 1}}}}); err == nil || out.Len() != 0 {
		t.Fatal("zero endpoint written")
	}
}

func TestUIDBatchesCommandRoundTrip(t *testing.T) {
	for _, wire := range []string{" 500\r\n", " 500 1:200\r\n", " 4294967295 4294967295:4294967295\r\n", " 500 2:1\r\n"} {
		size, indices, err := ReadUIDBatchesCommand(imapwire.NewDecoderString(wire, nil))
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		enc := imapwire.NewEncoder(&out, nil)
		if err := WriteUIDBatchesCommand(enc, size, indices); err != nil {
			t.Fatal(err)
		}
		enc.CRLF()
		if err := enc.Flush(); err != nil {
			t.Fatal(err)
		}
		if out.String() != wire {
			t.Fatalf("roundtrip %q != %q", out.String(), wire)
		}
	}
	for _, wire := range []string{" 0\r\n", " 500 0:1\r\n", " 500 1:0\r\n", " 500 1\r\n", " 500 1:*\r\n", " 500 1:4294967296\r\n", " 500 1:1 extra\r\n"} {
		if _, _, err := ReadUIDBatchesCommand(imapwire.NewDecoderString(wire, nil)); err == nil {
			t.Errorf("accepted %q", wire)
		}
	}
}
