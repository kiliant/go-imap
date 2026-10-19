package imapserver

import (
	"testing"

	"github.com/kiliant/go-imap"
)

func TestAppendLimitProjectionPreservesBackendData(t *testing.T) {
	c := &conn{server: New(nil, &Options{Limits: Limits{MaxLiteralBytes: 16}})}
	for _, value := range []any{uint64(0), uint64(8), uint64(16), uint64(32), "NIL"} {
		data := &imap.StatusData{Mailbox: "INBOX", Values: map[imap.StatusItemKeyword]any{
			imap.StatusItemAppendLimit: value, imap.StatusItemMessages: uint64(3),
		}}
		reply := c.statusReply(data, []imap.StatusItem{imap.StatusItemAppendLimit})
		want := value
		if value == uint64(32) || value == "NIL" {
			want = uint64(16)
		}
		if got := reply.Values[imap.StatusItemAppendLimit]; got != want || len(reply.Values) != 1 {
			t.Errorf("backend limit %v: reply = %v; want %v", value, reply.Values, want)
		}
		if data.Values[imap.StatusItemAppendLimit] != value || len(data.Values) != 2 {
			t.Errorf("backend-owned data was changed: %v", data.Values)
		}
	}
}
