package imapserver_test

import (
	"reflect"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func TestNewSearchQuery(t *testing.T) {
	for _, options := range []*imapserver.SearchQueryOptions{nil, {}} {
		query, err := imapserver.NewSearchQuery(nil, options)
		if err != nil || query == nil || query.Criteria() != imap.SearchAll {
			t.Fatalf("nil criteria = %#v, %v; want ALL", query, err)
		}
	}
	for _, criteria := range []imap.SearchCriteria{
		imap.SearchAnd{},
		imap.SearchOr{Left: imap.SearchSeen, Right: imap.SearchNot{Criteria: imap.SearchDeleted}},
		imap.SearchFuzzy{Criteria: imap.SearchString{Key: imap.SearchKeySubject, Value: "hello"}},
		imap.SearchModSeq{ModSeq: 5}, imap.SearchSaveDateSupported,
		imap.SearchWithin{Key: imap.SearchWithinKeyOlder, Seconds: 3600},
	} {
		query, err := imapserver.NewSearchQuery(criteria, nil)
		if err != nil || !reflect.DeepEqual(query.Criteria(), criteria) {
			t.Fatalf("NewSearchQuery(%#v) = %#v, %v", criteria, query, err)
		}
	}
}

func TestNewSearchQueryRejectsUnresolvedAndUnknownKeys(t *testing.T) {
	wraps := []func(imap.SearchCriteria) imap.SearchCriteria{
		func(c imap.SearchCriteria) imap.SearchCriteria { return c },
		func(c imap.SearchCriteria) imap.SearchCriteria { return imap.SearchAnd{imap.SearchAll, c} },
		func(c imap.SearchCriteria) imap.SearchCriteria { return imap.SearchOr{Left: imap.SearchAll, Right: c} },
		func(c imap.SearchCriteria) imap.SearchCriteria { return imap.SearchNot{Criteria: c} },
		func(c imap.SearchCriteria) imap.SearchCriteria { return imap.SearchFuzzy{Criteria: c} },
	}
	for _, invalid := range []imap.SearchCriteria{
		imap.SearchSavedResult{},
		imap.SearchSeqNum{Set: imap.SeqSetNum(1)}, imap.SearchFilter("filter"),
		imap.SearchKeyword("FUTURE"), imap.SearchString{Key: "FUTURE"},
		imap.SearchDate{Key: "FUTURE"}, imap.SearchSize{Key: "FUTURE"},
		imap.SearchWithin{Key: "FUTURE"}, imap.SearchObjectID{Key: "FUTURE"},
		imap.SearchModSeq{EntryType: "FUTURE"},
		imap.SearchNot{}, imap.SearchOr{Left: imap.SearchAll}, imap.SearchFuzzy{},
		imap.SearchModSeq{ModSeq: 1 << 63}, imap.SearchAnd{nil},
	} {
		for _, wrap := range wraps {
			criteria := wrap(invalid)
			if query, err := imapserver.NewSearchQuery(criteria, nil); err == nil || query != nil {
				t.Fatalf("accepted invalid criteria %#v: %#v, %v", criteria, query, err)
			}
		}
	}
}

func TestNewSearchQueryCopiesInput(t *testing.T) {
	uids := imap.UIDSetNum(7)
	children := imap.SearchAnd{imap.SearchUID{Set: uids}}
	criteria := imap.SearchNot{Criteria: imap.SearchFuzzy{Criteria: children}}
	query, err := imapserver.NewSearchQuery(criteria, nil)
	if err != nil {
		t.Fatal(err)
	}
	uids[0].Start = 99
	children[0] = imap.SearchSeqNum{Set: imap.SeqSetNum(1)}
	want := imap.SearchNot{Criteria: imap.SearchFuzzy{Criteria: imap.SearchAnd{imap.SearchUID{Set: imap.UIDSetNum(7)}}}}
	if !reflect.DeepEqual(query.Criteria(), want) {
		t.Fatalf("caller mutation changed query: %#v", query.Criteria())
	}
}

func TestNewSearchQueryBoundsDepthAndCycles(t *testing.T) {
	var criteria imap.SearchCriteria = imap.SearchAll
	for range 64 {
		criteria = imap.SearchNot{Criteria: criteria}
	}
	if _, err := imapserver.NewSearchQuery(criteria, nil); err != nil {
		t.Fatal(err)
	}
	criteria = imap.SearchNot{Criteria: criteria}
	if _, err := imapserver.NewSearchQuery(criteria, nil); err == nil {
		t.Fatal("accepted an excessively deep query")
	}
	cyclic := imap.SearchAnd{nil}
	cyclic[0] = cyclic
	if _, err := imapserver.NewSearchQuery(cyclic, nil); err == nil {
		t.Fatal("accepted a cyclic query")
	}
}
