package memory

import (
	"crypto/sha256"
	"reflect"
	"strings"
	"testing"
	"time"
)

func FuzzURLAuthURL(f *testing.F) {
	for _, raw := range []string{
		"imap://alice@example.com/INBOX/;UID=1;URLAUTH=anonymous",
		"imap://alice@example.com/INBOX;UIDVALIDITY=1/;UID=2/;SECTION=HEADER.FIELDS%20(SUBJECT)/;PARTIAL=0.5;URLAUTH=user+bob",
		"imap://alice@example.com/INBOX/;UID=1;EXPIRE=2999-01-01T00:00:00Z;URLAUTH=authuser",
	} {
		f.Add(raw, false)
		f.Add(raw+":internal:"+strings.Repeat("00", sha256.Size), true)
	}
	f.Fuzz(func(t *testing.T, raw string, full bool) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		parsed, ok := parseURLAuthURL(raw, full, now)
		if !ok {
			return
		}
		if parsed.owner == "" || parsed.mailbox == "" || parsed.uid == 0 || !strings.HasPrefix(raw, parsed.rump) {
			t.Fatalf("invalid accepted URL: %#v", parsed)
		}
		if full {
			if len(parsed.token) != sha256.Size {
				t.Fatal("invalid verifier length")
			}
			rump, ok := parseURLAuthURL(parsed.rump, false, now)
			if !ok {
				t.Fatal("stripping the verifier did not yield a valid rump URL")
			}
			parsed.token = nil
			if !reflect.DeepEqual(parsed, rump) {
				t.Fatal("full and rump URL changed resource or access meaning")
			}
		} else if parsed.rump != raw {
			t.Fatal("rump bytes were normalized")
		}
	})
}
