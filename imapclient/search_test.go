package imapclient

import (
	"testing"

	"github.com/kiliant/go-imap"
)

// TestSearchNeedsCharsetDescendsIntoContainers pins the CHARSET guard against
// the defect that made it necessary.
//
// Every Client.*Fuzzy entry point wraps the caller's criteria in
// imap.SearchFuzzy before the guard runs. The guard's own container list omitted
// that type, so a fuzzy search over non-ASCII text reported "no charset needed"
// and went onto the wire with the bytes undeclared — a server is then entitled
// to read them in any charset it likes. Nothing failed loudly; the search simply
// matched the wrong messages.
//
// The list now comes from imapcodec.SearchCriteriaChildren, which is gated
// against the type declarations in package imap.
func TestSearchNeedsCharsetDescendsIntoContainers(t *testing.T) {
	nonASCII := imap.SearchString{Key: imap.SearchKeySubject, Value: "Grüße"}
	ascii := imap.SearchString{Key: imap.SearchKeySubject, Value: "plain"}

	for _, testCase := range []struct {
		name     string
		criteria imap.SearchCriteria
		want     bool
	}{
		{"bare", nonASCII, true},
		{"ascii-bare", ascii, false},
		{"and", imap.SearchAnd{ascii, nonASCII}, true},
		{"or", imap.SearchOr{Left: ascii, Right: nonASCII}, true},
		{"not", imap.SearchNot{Criteria: nonASCII}, true},
		{"fuzzy", imap.SearchFuzzy{Criteria: nonASCII}, true},
		{"fuzzy-in-and", imap.SearchAnd{ascii, imap.SearchFuzzy{Criteria: nonASCII}}, true},
		{"fuzzy-in-not", imap.SearchNot{Criteria: imap.SearchFuzzy{Criteria: nonASCII}}, true},
		{"fuzzy-ascii", imap.SearchFuzzy{Criteria: ascii}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := searchNeedsCharset(testCase.criteria); got != testCase.want {
				t.Errorf("searchNeedsCharset = %v, want %v — a false negative sends "+
					"non-ASCII search text with no CHARSET declared", got, testCase.want)
			}
		})
	}
}
