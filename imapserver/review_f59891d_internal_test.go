package imapserver

import (
	"testing"

	"github.com/kiliant/go-imap"
)

func TestReviewGRequestedResyncUIDs(t *testing.T) {
	selected := &selectedState{uids: []imap.UID{1, 3, 4}, savedSearch: imap.UIDSetNum(2, 5)}
	for _, tc := range []struct {
		raw  string
		want imap.UIDSet
	}{
		{"1:10", imap.UIDSetRange(1, 10)},
		{"10:1", imap.UIDSetRange(1, 10)},
		{"2", imap.UIDSetNum(2)},
		{"1:*", imap.UIDSetRange(1, 4)},
		{"*:2", imap.UIDSetRange(2, 4)},
		{"5:*", imap.UIDSetRange(4, 5)},
		{"$", imap.UIDSetNum(2, 5)},
		{"1:4294967295", imap.UIDSetRange(1, ^imap.UID(0))},
	} {
		got, err := requestedResyncUIDs(selected, tc.raw)
		if err != nil || got.Dynamic() || !got.Equal(tc.want) {
			t.Fatalf("%s -> %v, %v; want %v", tc.raw, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		raw  string
		want imap.UIDSet
	}{
		{"2:5", imap.UIDSetRange(2, 5)}, {"*", nil}, {"1:*", nil}, {"2,4:*", imap.UIDSetNum(2)}, {"$", nil},
	} {
		got, err := requestedResyncUIDs(&selectedState{}, tc.raw)
		if err != nil || !got.Equal(tc.want) {
			t.Fatalf("empty selection %s -> %v, %v", tc.raw, got, err)
		}
	}
}
