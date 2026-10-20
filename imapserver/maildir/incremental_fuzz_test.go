package maildir

import (
	"encoding/json"
	"testing"
)

func FuzzMaildirIncrementalState(f *testing.F) {
	f.Add(`{"header":{"version":4,"sequence":1},"mailboxes":{}}`)
	f.Add(`{"header":{"version":4,"sequence":1},"mailboxes":{"INBOX":{"header":{"uidvalidity":1},"upsert":[null]}}}`)
	f.Add(`{"header":{"version":999,"sequence":1},"mailboxes":{}}`)
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 256<<10 {
			return
		}
		var change stateChange
		if json.Unmarshal([]byte(raw), &change) != nil {
			return
		}
		before, err := decodeState([]byte(`{"version":4,"mailboxes":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		next, err := applyChange(before, &change)
		if err != nil {
			return
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			t.Fatal(err)
		}
		if state, err := decodeState(encoded); err == nil {
			again, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeState(again); err != nil {
				t.Fatalf("incremental roundtrip: %v", err)
			}
		}
	})
}
