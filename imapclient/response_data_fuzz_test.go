package imapclient

import (
	"testing"

	"github.com/kiliant/go-imap/internal/imapwire"
)

// Exercise the shared decode-once boundary, including errors and a second
// collector inspecting the same response. Existing parser targets retain
// their corpora; these seeds cover the newly accepted event forms as well.
func FuzzDecodeResponseData(f *testing.F) {
	for _, name := range []string{"STATUS", "LIST", "LSUB", "XLIST", "ESEARCH", "METADATA"} {
		for _, wire := range hostileSeeds {
			f.Add(name, wire)
		}
	}
	f.Add("STATUS", " INBOX (MESSAGES 3 X-VENDOR 7)\r\n")
	f.Add("LIST", " () \"/\" INBOX (\"OLDNAME\" (\"Old\"))\r\n")
	f.Add("ESEARCH", " (TAG \"old\") UID ADDTO (0 9,3:4) REMOVEFROM (2 8) ADDTO (1 7)\r\n")
	f.Add("METADATA", " \"\" /shared/comment /private/comment\r\n")
	f.Add("METADATA", " INBOX (/shared/comment NIL /private/comment \"\")\r\n")
	f.Fuzz(func(t *testing.T, name, wire string) {
		resp := &untaggedResponse{name: name, dec: imapwire.NewDecoderString(wire, nil)}
		first := resp.decodeData()
		cached := resp.data
		if second := resp.decodeData(); second != first || cached != resp.data {
			t.Fatal("decoding a cached response changed its data or error")
		}
	})
}
