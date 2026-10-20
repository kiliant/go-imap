package imapserver

import (
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestSearchQueryUIDNormalization(t *testing.T) {
	query := newSearchQuery(imap.SearchAnd{
		imap.SearchSeqNum{Set: imap.SeqSetNum(1, 3)},
		imap.SearchOr{
			Left:  imap.SearchNot{Criteria: imap.SearchSeqNum{Set: imap.SeqSetRange(2, 0)}},
			Right: imap.SearchFuzzy{Criteria: imap.SearchSeqNum{Set: imap.SeqSetRange(0, 2)}},
		},
	}, []imap.UID{11, 22, 33}, nil)

	assertUIDNormalizedSearch(t, query.Criteria())
	criteria := query.Criteria().(imap.SearchAnd)
	if got := criteria[0].(imap.SearchUID).Set.String(); got != "11,33" {
		t.Fatalf("top-level UID set = %q, want 11,33", got)
	}
	nested := criteria[1].(imap.SearchOr)
	if got := nested.Left.(imap.SearchNot).Criteria.(imap.SearchUID).Set.String(); got != "22,33" {
		t.Fatalf("nested range = %q, want 22,33", got)
	}
	if got := nested.Right.(imap.SearchFuzzy).Criteria.(imap.SearchUID).Set.String(); got != "22,33" {
		t.Fatalf("reversed dynamic range = %q, want 22,33", got)
	}
}

// FuzzSearchQueryUIDNormalization is also the permanent corpus gate: every
// seed is decoded into the public SearchCriteria tree, normalized, and walked
// recursively. Standard go test runs the complete seed corpus.
func FuzzSearchQueryUIDNormalization(f *testing.F) {
	for _, seed := range []string{
		"ALL",
		"OR SEEN NOT DELETED",
		"1:*",
		"HEADER Subject \"x\"",
		"MODSEQ 42",
		"OR 1:2 NOT 3:*",
		"NOT OR 1 *",
		"FUZZY NOT 2",
		"$",
		"OR $ NOT $",
		"FUZZY $",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		criteria, err := imapcodec.ReadSearchCriteria(imapwire.NewDecoderString(input, &imapwire.Options{
			MaxLiteralSize:         1 << 20,
			MaxBufferedLiteralSize: 1 << 20,
			MaxLineLength:          8 << 10,
			MaxListDepth:           64,
		}))
		if err != nil {
			return
		}
		query := newSearchQuery(criteria, []imap.UID{11, 22, 33, 44, 55}, nil)
		assertUIDNormalizedSearch(t, query.Criteria())
	})
}

func assertUIDNormalizedSearch(t *testing.T, criteria imap.SearchCriteria) {
	t.Helper()
	switch criteria := criteria.(type) {
	case imap.SearchAnd:
		for _, child := range criteria {
			assertUIDNormalizedSearch(t, child)
		}
	case imap.SearchOr:
		assertUIDNormalizedSearch(t, criteria.Left)
		assertUIDNormalizedSearch(t, criteria.Right)
	case imap.SearchNot:
		assertUIDNormalizedSearch(t, criteria.Criteria)
	case imap.SearchFuzzy:
		assertUIDNormalizedSearch(t, criteria.Criteria)
	case imap.SearchSavedResult:
		t.Fatal("UID-normalized query contains unresolved saved result")
	case imap.SearchSeqNum:
		t.Fatalf("UID-normalized query contains sequence set %v", criteria.Set)
	case imap.SearchKeyword, imap.SearchFlagKeyword, imap.SearchHeaderField,
		imap.SearchString, imap.SearchDate, imap.SearchSize, imap.SearchUID,
		imap.SearchWithin, imap.SearchObjectID,
		imap.SearchModSeq:
		// Leaf criteria cannot contain another sequence set.
	default:
		t.Fatalf("UID-normalization gate does not know criteria type %T", criteria)
	}
}

func TestSearchQueryUIDWildcardUsesSelectedMaximum(t *testing.T) {
	for _, raw := range []string{"*", "34:*", "*:34", "33:*"} {
		set, err := imap.ParseUIDSet(raw)
		if err != nil {
			t.Fatal(err)
		}
		query := newSearchQuery(imap.SearchNot{Criteria: imap.SearchUID{Set: set}}, []imap.UID{11, 22, 33}, nil)
		got := query.Criteria().(imap.SearchNot).Criteria.(imap.SearchUID).Set
		if got.Dynamic() || got.String() != "33" {
			t.Fatalf("UID %s normalized to %s", raw, got.String())
		}
	}
	set, _ := imap.ParseUIDSet("*")
	got := newSearchQuery(imap.SearchUID{Set: set}, nil, nil).Criteria().(imap.SearchUID).Set
	if !got.IsEmpty() {
		t.Fatalf("empty mailbox wildcard = %s", got.String())
	}
}

func TestSearchSavedResultNormalization(t *testing.T) {
	for _, saved := range []imap.UIDSet{nil, imap.UIDSetNum(22)} {
		query := newSearchQuery(imap.SearchAnd{
			imap.SearchSavedResult{},
			imap.SearchNot{Criteria: imap.SearchSavedResult{}},
			imap.SearchOr{Left: imap.SearchSavedResult{}, Right: imap.SearchFuzzy{Criteria: imap.SearchSavedResult{}}},
		}, []imap.UID{11, 22, 33}, saved)
		assertUIDNormalizedSearch(t, query.Criteria())
		root := query.Criteria().(imap.SearchAnd)
		for _, key := range []imap.SearchCriteria{root[0], root[1].(imap.SearchNot).Criteria, root[2].(imap.SearchOr).Left, root[2].(imap.SearchOr).Right.(imap.SearchFuzzy).Criteria} {
			if saved.IsEmpty() {
				if not, ok := key.(imap.SearchNot); !ok || not.Criteria != imap.SearchAll {
					t.Fatalf("empty saved result = %#v, want NOT ALL", key)
				}
				continue
			}
			if got := key.(imap.SearchUID).Set.String(); got != saved.String() {
				t.Fatalf("saved result = %s, want %s", got, saved.String())
			}
		}
		if len(saved) > 0 {
			saved[0].Start = 999
			if root[0].(imap.SearchUID).Set.Contains(999) {
				t.Fatal("saved result alias reaches query")
			}
		}
	}
}
