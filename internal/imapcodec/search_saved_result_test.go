package imapcodec

import (
	"reflect"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestSearchSavedResultUIDKey(t *testing.T) {
	// RFC 5182 section 2.2 makes UID $ and $ equivalent, including when
	// nested. A following key and CRLF check their token boundaries.
	for _, tc := range []struct {
		wire string
		want imap.SearchCriteria
	}{
		{"UID $", imap.SearchSavedResult{}},
		{"uid $ SMALLER 4096", imap.SearchAnd{imap.SearchSavedResult{}, imap.SearchSize{Key: imap.SearchSizeKeySmaller, Size: 4096}}},
		{"OR UID $ UID 4", imap.SearchOr{Left: imap.SearchSavedResult{}, Right: imap.SearchUID{Set: imap.UIDSetNum(4)}}},
		{"NOT UID $", imap.SearchNot{Criteria: imap.SearchSavedResult{}}},
		{"(UID $)", imap.SearchSavedResult{}},
		{"UID 2:3", imap.SearchUID{Set: imap.UIDSetRange(2, 3)}},
		{"UID 1:*", imap.SearchUID{Set: imap.UIDSetRange(1, 0)}},
	} {
		t.Run(tc.wire, func(t *testing.T) {
			dec := imapwire.NewDecoderString(tc.wire+"\r\n", nil)
			got, err := ReadSearchCriteria(dec)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("criteria = %#v, %v; want %#v", got, err, tc.want)
			}
			if !dec.ExpectCRLF() {
				t.Fatal(dec.Err())
			}
		})
	}
}

func TestSearchSavedResultUIDKeyRejectsInvalidTokens(t *testing.T) {
	for _, wire := range []string{"UID $x", "UID $,1", "UID 1,$", "UID $:2", "UID", "UID "} {
		t.Run(wire, func(t *testing.T) {
			dec := imapwire.NewDecoderString(wire+"\r\n", nil)
			if _, err := ReadSearchCriteria(dec); err == nil && dec.ExpectCRLF() {
				t.Fatal("accepted invalid saved-result token")
			}
		})
	}
}
