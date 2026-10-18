package maildir

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func FuzzMaildirState(f *testing.F) {
	f.Add(`{"version":2,"mailboxes":{},"future":{"x":1}}`)
	f.Add(`{"version":3,"mailboxes":{"INBOX":{"uidvalidity":1,"uidnext":6,"revision":2,"highestmodseq":9,"tombstonefloor":4,"messages":[{"uid":5,"file":"msg:2,","size":0,"modseq":9}],"tombstones":[{"uid":1,"modseq":7}]}}}`)
	f.Add(`{"version":3,"mailboxes":{"INBOX":{"uidvalidity":1,"uidnext":1,"revision":1,"highestmodseq":9,"tombstonefloor":10}}}`)
	f.Add(`{"version":999,"mailboxes":{}}`)
	f.Add(`{"version":1,"mailboxes":{"INBOX":{"uidvalidity":1,"uidnext":1}}}`)
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 256<<10 {
			return
		}
		state, err := decodeState([]byte(raw))
		if err != nil {
			return
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeState(encoded)
		if err != nil {
			t.Fatalf("roundtrip: %v", err)
		}
		again, err := json.Marshal(got)
		if err != nil || !bytes.Equal(encoded, again) {
			t.Fatalf("unstable state roundtrip: %v", err)
		}
	})
}
func FuzzMaildirPathsAndDelivery(f *testing.F) {
	for _, seed := range []string{"", "cur", "../new/%", "a\r\nb\nc\r", "用户", "x\x00y"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		escaped := escapeSegment(raw)
		got, err := unescapeSegment(escaped)
		if err != nil || got != raw {
			t.Fatalf("path roundtrip: %v", err)
		}
		if !localPath(escaped) || strings.Contains(escaped, "/") {
			t.Fatal("unsafe segment")
		}
		if _, err := unescapeSegment(raw); err == nil && escapeSegment(raw) == "" {
			t.Fatal("empty encoded segment")
		}
		gotBytes, err := io.ReadAll(&normalizer{source: bytes.NewBufferString(raw)})
		if err != nil {
			t.Fatal(err)
		}
		want := strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\n", "\r\n")
		if string(gotBytes) != want {
			t.Fatalf("normalization %q != %q", gotBytes, want)
		}
	})
}
