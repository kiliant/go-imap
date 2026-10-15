package imapclient

import (
	"strings"
	"testing"

	"github.com/kiliant/go-imap/internal/imapwire"
)

// FuzzReadListExtendedData feeds the remainder of a LIST response, after the
// mailbox name, to the extended data reader. Whatever a hostile server sends
// there must not panic, and what is returned must keep ListData's documented
// shape.
func FuzzReadListExtendedData(f *testing.F) {
	for _, seed := range []string{
		"\r\n",
		" (\"CHILDINFO\" (\"SUBSCRIBED\"))\r\n",
		" (CHILDINFO (subscribed \"X-OPT\"))\r\n",
		" (\"OLDNAME\" (\"Old\") \"CHILDINFO\" (\"SUBSCRIBED\") \"X-FUTURE\" (1 (2 \"3\") NIL))\r\n",
		" (\"X-LIT\" {5}\r\nab\r\nc \"X-NEXT\" ({3}\r\nxyz))\r\n",
		" (\"CHILDINFO\" ((\"SUBSCRIBED\")) \"CHILDINFO\" () \"CHILDINFO\" (\"\") \"CHILDINFO\" \"SUBSCRIBED\")\r\n",
		" (\"CHILDINFO\" (\"SUBSCRIBED\") \"CHILDINFO\" (\"REMOTE\"))\r\n",
		" (\"OLDNAME\" (\"Old\") \"OLDNAME\" (\"A\" \"B\") \"OLDNAME\" {3}\r\nabc)\r\n",
		" (\"CHILDINFO\" (\"x-opt Value\"))\r\n",
		" (\"CHILDINFO\" (\" SUBSCRIBED\"))\r\n",
		" (\"X-BROKEN\")\r\n",
		" (\"CHILDINFO\" (\"SUBSCRIBED\"\r\n",
		" (\"X-DEEP\" ((((((((((((((((x)))))))))))))))))\r\n",
		" \"CHILDINFO\" (\"SUBSCRIBED\")\r\n",
		" ({3}\r\nTAG 1)\r\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, wire string) {
		got, err := readListExtendedData(imapwire.NewDecoderString(wire, nil))
		if err != nil {
			return
		}
		if got.ChildInfo != nil && len(got.ChildInfo) == 0 {
			t.Fatalf("present but empty ChildInfo from %q", wire)
		}
		for _, option := range got.ChildInfo {
			name, _, _ := strings.Cut(option, " ")
			if name == "" || name != strings.ToUpper(name) {
				t.Fatalf("CHILDINFO option %q from %q", option, wire)
			}
		}
		tags := make(map[string]bool)
		for _, item := range got.Extended {
			if item.Tag == "" || item.Tag != strings.ToUpper(item.Tag) {
				t.Fatalf("extended item tag %q from %q", item.Tag, wire)
			}
			tags[item.Tag] = true
		}
		// A decoded field is a view of an item that is kept, never a
		// replacement for it.
		if got.ChildInfo != nil && !tags["CHILDINFO"] || got.OldName != "" && !tags["OLDNAME"] {
			t.Fatalf("decoded item missing from Extended: %#v from %q", got, wire)
		}
	})
}
