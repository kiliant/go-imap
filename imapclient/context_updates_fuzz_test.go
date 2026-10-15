package imapclient

import (
	"reflect"
	"testing"

	"github.com/kiliant/go-imap/internal/imapwire"
)

func FuzzDecodeContextUpdate(f *testing.F) {
	for _, wire := range append(append([]string(nil), hostileSeeds...),
		` (TAG "old") UID ADDTO (1 9,4:2 1 7) REMOVEFROM (0 1:4294967295) ADDTO (0 5)`+"\r\n",
		` (TAG "old") X-FUTURE ("a" NIL)`+"\r\n") {
		f.Add(wire)
	}
	f.Fuzz(func(t *testing.T, wire string) {
		parsed, err := readESearchResponse(imapwire.NewDecoderString(wire, nil))
		if err != nil {
			return
		}
		first, err := decodeContextUpdate(parsed)
		if err != nil {
			return
		}
		second, err := decodeContextUpdate(parsed)
		if err != nil || !reflect.DeepEqual(first, second) || len(first.Items) != len(parsed.items) {
			t.Fatal("decoding lost items or mutated cached data")
		}
	})
}
