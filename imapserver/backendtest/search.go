package backendtest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func searchQuery(t *testing.T, criteria imap.SearchCriteria) *imapserver.SearchQuery {
	t.Helper()
	query, err := imapserver.NewSearchQuery(criteria, nil)
	if err != nil {
		t.Fatalf("construct SEARCH query: %v", err)
	}
	return query
}

func checkSearch(t *testing.T, mailbox imapserver.SelectedMailbox, criteria imap.SearchCriteria, want []imap.UID) {
	t.Helper()
	checkSearchWith(t, mailbox, criteria, nil, want)
}

func checkSearchWith(t *testing.T, mailbox imapserver.SelectedMailbox, criteria imap.SearchCriteria, options *imapserver.SearchOptions, want []imap.UID) *imapserver.SearchResult {
	t.Helper()
	result, err := mailbox.Search(t.Context(), searchQuery(t, criteria), options)
	if err != nil || result == nil {
		t.Fatalf("Search(%#v, %#v) = %#v, %v", criteria, options, result, err)
	}
	got := slices.Clone(result.UIDs)
	want = slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Search(%#v, %#v) UIDs = %v, want %v", criteria, options, got, want)
	}
	return result
}

// checkRelevancy asks for RFC 6203 relevancy scores, which are optional: a nil
// or empty Relevancy passes. Scores a backend does supply must pair with the
// UIDs by position and lie from 1 to 100, the rule SearchResult.Relevancy
// states and the framework enforces with a NO.
func checkRelevancy(t *testing.T, mailbox imapserver.SelectedMailbox, criteria imap.SearchCriteria, want []imap.UID) {
	t.Helper()
	result := checkSearchWith(t, mailbox, criteria, &imapserver.SearchOptions{ReturnRelevancy: true}, want)
	if len(result.Relevancy) == 0 {
		return
	}
	if len(result.Relevancy) != len(result.UIDs) {
		t.Fatalf("Search(%#v) returned %d relevancy scores for %d UIDs", criteria, len(result.Relevancy), len(result.UIDs))
	}
	for i, score := range result.Relevancy {
		if score < 1 || score > 100 {
			t.Fatalf("Search(%#v) gave UID %d the relevancy score %d, outside 1 to 100", criteria, result.UIDs[i], score)
		}
	}
}

func runSearch(t *testing.T, harness *Harness) {
	t.Run("search", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		const name = "backendtest-search"
		if err := session.Create(t.Context(), name, nil); err != nil {
			t.Fatal(err)
		}
		empty := selectMailbox(t, session, name, discardUpdater())
		checkSearch(t, empty.Mailbox, imap.SearchAll, nil)
		if err := empty.Mailbox.Unselect(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		// reported[i] is the UID APPEND reported for message i, or zero when it
		// reported none. Without UIDPLUS each result is optional on its own.
		var reported []imap.UID
		var sizes []int
		flags := [][]imap.Flag{{imap.FlagSeen, "$search-fixture"}, {imap.FlagFlagged}, {imap.FlagAnswered, imap.FlagDeleted, imap.FlagDraft}}
		for i, subject := range []string{"Red apple", "Blue berry", "Red berry"} {
			date := time.Date(2020, 1, i+1, 12, 0, 0, 0, time.UTC)
			raw := fmt.Sprintf("From: sender%d@example.test\r\nTo: receiver@example.test\r\nCc: team@example.test\r\nBcc: hidden@example.test\r\nSubject: %s\r\nDate: %s\r\n\r\nbody%d\r\n", i, subject, date.Format(time.RFC1123Z), i)
			data, err := session.Append(t.Context(), name, strings.NewReader(raw), &imapserver.AppendOptions{
				Flags: flags[i], InternalDate: date.AddDate(0, 0, 1),
			})
			if err != nil {
				t.Fatalf("seed SEARCH message: %#v, %v", data, err)
			}
			uid := imap.UID(0)
			if data != nil && data.HasUID {
				uid = data.UID
			}
			reported = append(reported, uid)
			sizes = append(sizes, len(raw))
		}
		selected := selectMailbox(t, session, name, discardUpdater())
		// The mailbox held nothing before, so the snapshot lists the seeded
		// messages in append order and is the fixture's source of UIDs. A UID
		// APPEND did report must name its own message; reporting is optional
		// per call without UIDPLUS, so absent ones are not compared.
		ids := selected.Snapshot.UIDs
		if len(ids) != len(reported) {
			t.Fatalf("seeded SEARCH mailbox lists %v, want %d messages", ids, len(reported))
		}
		for i, uid := range reported {
			if uid != 0 && uid != ids[i] {
				t.Fatalf("APPEND %d reported UID %d, but the snapshot lists %v", i, uid, ids)
			}
		}
		defer func() {
			if err := selected.Mailbox.Unselect(context.Background(), nil); err != nil {
				t.Error(err)
			}
		}()
		uids := func(indices ...int) []imap.UID {
			var result []imap.UID
			for _, i := range indices {
				result = append(result, ids[i])
			}
			return result
		}
		type searchCase struct {
			name, capability string
			criteria         imap.SearchCriteria
			want             []imap.UID
		}
		cases := []searchCase{
			{name: "all", criteria: imap.SearchAll, want: ids},
			{name: "seen", criteria: imap.SearchSeen, want: uids(0)},
			{name: "unseen", criteria: imap.SearchUnseen, want: uids(1, 2)},
			{name: "answered", criteria: imap.SearchAnswered, want: uids(2)},
			{name: "unanswered", criteria: imap.SearchUnanswered, want: uids(0, 1)},
			{name: "deleted", criteria: imap.SearchDeleted, want: uids(2)},
			{name: "undeleted", criteria: imap.SearchUndeleted, want: uids(0, 1)},
			{name: "draft", criteria: imap.SearchDraft, want: uids(2)},
			{name: "undraft", criteria: imap.SearchUndraft, want: uids(0, 1)},
			{name: "flagged", criteria: imap.SearchFlagged, want: uids(1)},
			{name: "unflagged", criteria: imap.SearchUnflagged, want: uids(0, 2)},
			{name: "keyword", criteria: imap.SearchFlagKeyword{Flag: "$search-fixture"}, want: uids(0)},
			{name: "unkeyword", criteria: imap.SearchFlagKeyword{Flag: "$search-fixture", Not: true}, want: uids(1, 2)},
			{name: "uid", criteria: imap.SearchUID{Set: imap.UIDSetNum(ids[1])}, want: uids(1)},
			// These are the forms produced when the framework substitutes SEARCHRES,
			// including an unset or empty saved result under NOT and OR.
			{name: "saved-empty", criteria: imap.SearchNot{Criteria: imap.SearchAll}},
			{name: "saved-empty-not", criteria: imap.SearchNot{Criteria: imap.SearchNot{Criteria: imap.SearchAll}}, want: ids},
			{name: "saved-or", criteria: imap.SearchOr{Left: imap.SearchUID{Set: imap.UIDSetNum(ids[0])}, Right: imap.SearchUID{Set: imap.UIDSetNum(ids[1])}}, want: uids(0, 1)},
			{name: "missing-uid", criteria: imap.SearchUID{Set: imap.UIDSetNum(ids[2] + 1)}},
			{name: "header", criteria: imap.SearchHeaderField{Field: "Subject", Value: "rED"}, want: uids(0, 2)},
			{name: "and", criteria: imap.SearchAnd{imap.SearchUnseen, imap.SearchDeleted}, want: uids(2)},
			{name: "or", criteria: imap.SearchOr{Left: imap.SearchSeen, Right: imap.SearchFlagged}, want: uids(0, 1)},
			{name: "not", criteria: imap.SearchNot{Criteria: imap.SearchSeen}, want: uids(1, 2)},
			{name: "no-matches", criteria: imap.SearchNot{Criteria: imap.SearchAll}},
			{name: "modseq-all", capability: "CONDSTORE", criteria: imap.SearchModSeq{}, want: ids},
			{name: "modseq-none", capability: "CONDSTORE", criteria: imap.SearchModSeq{ModSeq: 1<<63 - 1}},
			{name: "within-older", capability: "WITHIN", criteria: imap.SearchWithin{Key: imap.SearchWithinKeyOlder, Seconds: 3600}, want: ids},
			{name: "within-younger", capability: "WITHIN", criteria: imap.SearchWithin{Key: imap.SearchWithinKeyYounger, Seconds: 3600}},
			{name: "fuzzy-all", capability: "SEARCH=FUZZY", criteria: imap.SearchFuzzy{Criteria: imap.SearchAll}, want: ids},
			{name: "threadid-absent", capability: "OBJECTID", criteria: imap.SearchObjectID{Key: imap.SearchObjectIDKeyThread, Value: "backendtest-no-such-thread"}},
		}
		for _, test := range []struct {
			key   imap.SearchStringKey
			value string
			want  []imap.UID
		}{
			{imap.SearchKeyFrom, "sender1", uids(1)}, {imap.SearchKeyTo, "receiver", ids},
			{imap.SearchKeyCc, "team", ids}, {imap.SearchKeyBcc, "hidden", ids},
			{imap.SearchKeySubject, "rED", uids(0, 2)}, {imap.SearchKeyBody, "body2", uids(2)},
			{imap.SearchKeyText, "Blue berry", uids(1)},
		} {
			cases = append(cases, searchCase{name: string(test.key), criteria: imap.SearchString{Key: test.key, Value: test.value}, want: test.want})
		}
		for _, test := range []struct {
			key  imap.SearchDateKey
			day  int
			want []imap.UID
		}{
			{imap.SearchDateKeyBefore, 3, uids(0)}, {imap.SearchDateKeyOn, 3, uids(1)}, {imap.SearchDateKeySince, 3, uids(1, 2)},
			{imap.SearchDateKeySentBefore, 2, uids(0)}, {imap.SearchDateKeySentOn, 2, uids(1)}, {imap.SearchDateKeySentSince, 2, uids(1, 2)},
		} {
			cases = append(cases, searchCase{name: string(test.key), criteria: imap.SearchDate{Key: test.key, Date: time.Date(2020, 1, test.day, 0, 0, 0, 0, time.UTC)}, want: test.want})
		}
		for _, key := range []imap.SearchSizeKey{imap.SearchSizeKeySmaller, imap.SearchSizeKeyLarger} {
			var want []imap.UID
			for i, size := range sizes {
				if key == imap.SearchSizeKeySmaller && size < sizes[1] || key == imap.SearchSizeKeyLarger && size > sizes[1] {
					want = append(want, ids[i])
				}
			}
			cases = append(cases, searchCase{name: string(key), criteria: imap.SearchSize{Key: key, Size: int64(sizes[1])}, want: want})
		}

		// Recent is assigned by the backend. Check its SEARCH view against its
		// FETCH flags instead of requiring a particular delivery policy.
		items := []imap.FetchItem{imap.FetchItemUID, imap.FetchItemFlags}
		if supportsCapability(instance, session, "SAVEDATE") {
			items = append(items, imap.FetchItemSaveDate)
		}
		if supportsCapability(instance, session, "OBJECTID") {
			items = append(items, imap.FetchItemEmailID, imap.FetchItemThreadID)
		}
		var recent, old, newMessages, saved []imap.UID
		writer := &imapserver.FetchWriter{WriteFunc: func(_ context.Context, data *imap.FetchMessageData) error {
			uidValue, ok := fetchValue[imap.FetchDataUID](data, "UID")
			if !ok || uidValue == 0 {
				return fmt.Errorf("FETCH omitted a valid UID")
			}
			flagValue, ok := fetchValue[imap.FetchDataFlags](data, "FLAGS")
			if !ok {
				return fmt.Errorf("FETCH omitted valid FLAGS for UID %d", uidValue)
			}
			uid, flags := imap.UID(uidValue), []imap.Flag(flagValue)
			if imap.ContainsFlag(flags, imap.FlagRecent) {
				recent = append(recent, uid)
				if !imap.ContainsFlag(flags, imap.FlagSeen) {
					newMessages = append(newMessages, uid)
				}
			} else {
				old = append(old, uid)
			}
			if value, ok := fetchValue[*imap.FetchDataSaveDate](data, "SAVEDATE"); ok && value != nil {
				if date := value.Date; date != nil {
					saved = append(saved, uid)
					// Restrict each comparison to this UID, since other messages
					// may have been appended on the same calendar day.
					for _, key := range []imap.SearchDateKey{imap.SearchDateKeySavedBefore, imap.SearchDateKeySavedOn, imap.SearchDateKeySavedSince} {
						var want []imap.UID
						if key != imap.SearchDateKeySavedBefore {
							want = []imap.UID{uid}
						}
						cases = append(cases, searchCase{name: fmt.Sprintf("%s-%d", key, uid), capability: "SAVEDATE", criteria: imap.SearchAnd{
							imap.SearchUID{Set: imap.UIDSetNum(uid)}, imap.SearchDate{Key: key, Date: *date},
						}, want: want})
					}
				}
			}
			if value, ok := fetchValue[imap.FetchDataObjectID](data, "EMAILID"); ok {
				id := string(value)
				if id == "" {
					t.Error("OBJECTID witnessed but FETCH EMAILID is empty")
				} else {
					cases = append(cases, searchCase{name: fmt.Sprintf("emailid-%d", uid), capability: "OBJECTID", criteria: imap.SearchObjectID{Key: imap.SearchObjectIDKeyEmail, Value: id}, want: []imap.UID{uid}})
					cases = append(cases, searchCase{name: fmt.Sprintf("nested-emailid-%d", uid), capability: "OBJECTID", criteria: imap.SearchAnd{
						imap.SearchNot{Criteria: imap.SearchNot{Criteria: imap.SearchOr{Left: imap.SearchDeleted, Right: imap.SearchObjectID{Key: imap.SearchObjectIDKeyEmail, Value: id}}}},
						imap.SearchUID{Set: imap.UIDSetNum(uid)},
					}, want: []imap.UID{uid}})
				}
			}
			return nil
		}}
		if err := selected.Mailbox.Fetch(t.Context(), writer, imap.UIDSetRange(1, 0), &imapserver.FetchOptions{Items: items}); err != nil {
			t.Fatal(err)
		}
		cases = append(cases,
			searchCase{name: "recent", criteria: imap.SearchRecent, want: recent},
			searchCase{name: "old", criteria: imap.SearchOld, want: old},
			searchCase{name: "new", criteria: imap.SearchNew, want: newMessages},
			searchCase{name: "savedatesupported", capability: "SAVEDATE", criteria: imap.SearchSaveDateSupported, want: saved},
		)
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				if test.capability != "" && !supportsCapability(instance, session, test.capability) {
					t.Skipf("backend does not witness %s", test.capability)
				}
				checkSearch(t, selected.Mailbox, test.criteria, test.want)
			})
		}
		t.Run("fuzzy-relevancy", func(t *testing.T) {
			if !supportsCapability(instance, session, "SEARCH=FUZZY") {
				t.Skip("backend does not witness SEARCH=FUZZY")
			}
			checkRelevancy(t, selected.Mailbox, imap.SearchFuzzy{Criteria: imap.SearchAll}, ids)
		})
	})
}
