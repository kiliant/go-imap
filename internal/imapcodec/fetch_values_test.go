package imapcodec

import (
	"testing"

	"github.com/kiliant/go-imap"
)

func TestFetchValueCaseAndAmbiguity(t *testing.T) {
	for _, test := range []struct {
		name  string
		items map[imap.FetchDataKey][]imap.FetchData
		valid bool
	}{
		{"lower", map[imap.FetchDataKey][]imap.FetchData{"uid": {imap.FetchDataUID(3)}}, true},
		{"mixed", map[imap.FetchDataKey][]imap.FetchData{"UiD": {imap.FetchDataUID(3)}}, true},
		{"conflicting", map[imap.FetchDataKey][]imap.FetchData{"uid": {imap.FetchDataUID(3)}, "UID": {imap.FetchDataUID(4)}}, false},
		{"repeated", map[imap.FetchDataKey][]imap.FetchData{"uid": {imap.FetchDataUID(3)}, "UID": {imap.FetchDataUID(3)}}, false},
		{"wrong-type", map[imap.FetchDataKey][]imap.FetchData{"uid": {imap.FetchDataFlags{imap.FlagSeen}}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, ok := FetchValue[imap.FetchDataUID](&imap.FetchMessageData{Items: test.items}, "UID")
			if ok != test.valid {
				t.Fatalf("valid=%v, want %v", ok, test.valid)
			}
		})
	}
}
