package imapserver_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// relevancyBackend wraps memory so that SEARCH and MULTISEARCH return chosen
// scores. Each result's UIDs are reversed first, so a score that pairs by
// sorted position instead of by UID shows up as a wrong number.
type relevancyBackend struct {
	*memory.Backend
	// mode selects the scores: "uid" scores 10 per UID, "none" supplies nil,
	// "empty" an empty slice, "short" one too few, "zero" and "high" an
	// out-of-range score, and "memory" leaves memory's own result untouched.
	mode     string
	noFuzzy  bool
	searches []bool // ReturnRelevancy of each Search call
}

type relevancySession struct {
	imapserver.Session
	backend *relevancyBackend
}

type relevancyMailbox struct {
	imapserver.SelectedMailbox
	backend *relevancyBackend
}

func (b *relevancyBackend) SupportsCapability(ctx context.Context, name string, options *imapserver.CapabilitySupportOptions) bool {
	if b.noFuzzy && name == "SEARCH=FUZZY" {
		return false
	}
	return b.Backend.SupportsCapability(ctx, name, options)
}

func (b *relevancyBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &relevancySession{session, b}, nil
}

func (s *relevancySession) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, name, updater, options)
	if err == nil {
		result.Mailbox = &relevancyMailbox{result.Mailbox, s.backend}
	}
	return result, err
}

func (s *relevancySession) MultiSearch(ctx context.Context, mailboxes []string, criteria imap.SearchCriteria, options *imapserver.MultiSearchOptions) ([]imapserver.MultiSearchMailboxResult, error) {
	results, err := s.Session.(imapserver.MultiSearchSession).MultiSearch(ctx, mailboxes, criteria, options)
	for i := range results {
		slices.Reverse(results[i].UIDs)
		slices.Reverse(results[i].ModSeqs)
		results[i].Relevancy = s.backend.scores(results[i].UIDs, results[i].Relevancy)
	}
	return results, err
}

func (m *relevancyMailbox) Search(ctx context.Context, query *imapserver.SearchQuery, options *imapserver.SearchOptions) (*imapserver.SearchResult, error) {
	m.backend.searches = append(m.backend.searches, options != nil && options.ReturnRelevancy)
	result, err := m.SelectedMailbox.Search(ctx, query, options)
	if err != nil {
		return nil, err
	}
	slices.Reverse(result.UIDs)
	result.Relevancy = m.backend.scores(result.UIDs, result.Relevancy)
	return result, nil
}

func (m *relevancyMailbox) Sort(ctx context.Context, query *imapserver.SearchQuery, keys []imap.SortKeySpec, options *imapserver.SortOptions) ([]imap.UID, error) {
	return m.SelectedMailbox.(imapserver.SortMailbox).Sort(ctx, query, keys, options)
}

func (b *relevancyBackend) scores(uids []imap.UID, own []uint8) []uint8 {
	if b.mode == "memory" {
		return own
	}
	if b.mode == "none" {
		return nil
	}
	if b.mode == "empty" {
		return []uint8{}
	}
	scores := make([]uint8, len(uids))
	for i, uid := range uids {
		scores[i] = uint8(10 * uid)
	}
	switch {
	case len(scores) == 0:
	case b.mode == "short":
		scores = scores[1:]
	case b.mode == "zero":
		scores[0] = 0
	case b.mode == "high":
		scores[0] = 101
	}
	return scores
}

// relevancyOpen selects INBOX holding five messages. Three subjects contain
// "apple", at UIDs 1, 3 and 5; UID 2 is expunged, so those three are at
// sequence numbers 1, 2 and 4.
func relevancyOpen(t *testing.T, b *relevancyBackend) *r4Wire {
	t.Helper()
	w := r4Open(t, b, nil)
	for _, subject := range []string{"apple one", "banana", "apple two", "cherry", "apple three"} {
		r4Append(w, "INBOX", "", subject)
	}
	for _, command := range []string{"SELECT INBOX", "UID STORE 2 +FLAGS (\\Deleted)", "EXPUNGE"} {
		if got := r4Tagged(w.cmd("S", command)); !strings.HasPrefix(got, "S OK") {
			t.Fatalf("%s = %q", command, got)
		}
	}
	return w
}

// relevancyResult runs one command and returns its single ESEARCH response and
// its tagged status.
func relevancyResult(t *testing.T, w *r4Wire, command string) (string, string) {
	t.Helper()
	lines := w.cmd("T", command)
	var esearch []string
	for _, line := range lines {
		if strings.HasPrefix(line, "* ESEARCH ") {
			esearch = append(esearch, line)
		}
	}
	if len(esearch) > 1 {
		t.Fatalf("%s sent %d ESEARCH responses: %q", command, len(esearch), lines)
	}
	if len(esearch) == 0 {
		return "", r4Tagged(lines)
	}
	return esearch[0], r4Tagged(lines)
}

// RFC 6203 section 4: SEARCH RETURN (RELEVANCY) returns the backend's scores,
// in result order, for the messages the response returns.
func TestSearchReturnRelevancy(t *testing.T) {
	cases := []struct{ command, want string }{
		{"SEARCH RETURN (RELEVANCY ALL) FUZZY SUBJECT apple", `* ESEARCH (TAG "T") ALL 1:2,4 RELEVANCY (10 30 50)`},
		{"UID SEARCH RETURN (RELEVANCY ALL) FUZZY SUBJECT apple", `* ESEARCH (TAG "T") UID ALL 1,3,5 RELEVANCY (10 30 50)`},
		// RELEVANCY is data of its own and implies no ALL.
		{"SEARCH RETURN (RELEVANCY) FUZZY SUBJECT apple", `* ESEARCH (TAG "T") RELEVANCY (10 30 50)`},
		// MIN, MAX and PARTIAL without ALL or COUNT return only their
		// messages, so the scores cover only those (RFC 4731 section 3.2).
		{"SEARCH RETURN (MIN RELEVANCY) FUZZY SUBJECT apple", `* ESEARCH (TAG "T") MIN 1 RELEVANCY (10)`},
		{"SEARCH RETURN (MIN MAX RELEVANCY) FUZZY SUBJECT apple", `* ESEARCH (TAG "T") MIN 1 MAX 4 RELEVANCY (10 50)`},
		{"SEARCH RETURN (MAX RELEVANCY) FUZZY SUBJECT \"apple one\"", `* ESEARCH (TAG "T") MAX 1 RELEVANCY (10)`},
		{"SEARCH RETURN (PARTIAL 2:3 RELEVANCY) FUZZY SUBJECT apple", `* ESEARCH (TAG "T") PARTIAL (2:3 2,4) RELEVANCY (30 50)`},
		{"SEARCH RETURN (MIN PARTIAL -1:-1 RELEVANCY) FUZZY SUBJECT apple", `* ESEARCH (TAG "T") MIN 1 PARTIAL (-1:-1 4) RELEVANCY (10 50)`},
		{"SEARCH RETURN (COUNT MIN RELEVANCY) FUZZY SUBJECT apple", `* ESEARCH (TAG "T") MIN 1 COUNT 3 RELEVANCY (10 30 50)`},
		// SAVE alone sends nothing, but RELEVANCY is data.
		{"SEARCH RETURN (SAVE RELEVANCY) FUZZY SUBJECT apple", `* ESEARCH (TAG "T") RELEVANCY (10 30 50)`},
		// An empty result still answers the option, with an empty score-list.
		{"SEARCH RETURN (RELEVANCY ALL) FUZZY SUBJECT zebra", `* ESEARCH (TAG "T") RELEVANCY ()`},
		// FUZZY may sit anywhere in the criteria.
		{"SEARCH RETURN (RELEVANCY ALL) OR FUZZY SUBJECT apple SUBJECT cherry", `* ESEARCH (TAG "T") ALL 1:4 RELEVANCY (10 30 40 50)`},
	}
	b := &relevancyBackend{Backend: r4Memory(), mode: "uid"}
	w := relevancyOpen(t, b)
	for _, test := range cases {
		got, tagged := relevancyResult(t, w, test.command)
		if !strings.HasPrefix(tagged, "T OK") || got != test.want {
			t.Errorf("%s\n got %q, %q\nwant %q", test.command, got, tagged, test.want)
		}
	}
}

// The backend is asked for scores only when the client asked for them.
func TestSearchReturnRelevancyRequestsScores(t *testing.T) {
	b := &relevancyBackend{Backend: r4Memory(), mode: "uid"}
	w := relevancyOpen(t, b)
	b.searches = nil
	relevancyResult(t, w, "SEARCH RETURN (ALL) FUZZY SUBJECT apple")
	relevancyResult(t, w, "SEARCH RETURN (RELEVANCY) FUZZY SUBJECT apple")
	if !slices.Equal(b.searches, []bool{false, true}) {
		t.Fatalf("ReturnRelevancy per Search call = %v, want [false true]", b.searches)
	}
}

// Memory scores every exact FUZZY match 100, and a backend that supplies no
// scores, nil or empty, gets the same answer from the framework.
func TestSearchReturnRelevancyWithoutBackendScores(t *testing.T) {
	for _, mode := range []string{"memory", "none", "empty"} {
		t.Run(mode, func(t *testing.T) {
			w := relevancyOpen(t, &relevancyBackend{Backend: r4Memory(), mode: mode})
			got, tagged := relevancyResult(t, w, "UID SEARCH RETURN (RELEVANCY ALL) FUZZY SUBJECT apple")
			if want := `* ESEARCH (TAG "T") UID ALL 1,3,5 RELEVANCY (100 100 100)`; !strings.HasPrefix(tagged, "T OK") || got != want {
				t.Fatalf("got %q, %q; want %q", got, tagged, want)
			}
		})
	}
}

// Scores that do not pair with the UIDs, or that leave 1 to 100, are a
// backend fault: the command fails before any ESEARCH response is written.
func TestSearchReturnRelevancyRejectsInvalidScores(t *testing.T) {
	for _, mode := range []string{"short", "zero", "high"} {
		t.Run(mode, func(t *testing.T) {
			w := relevancyOpen(t, &relevancyBackend{Backend: r4Memory(), mode: mode})
			got, tagged := relevancyResult(t, w, "SEARCH RETURN (RELEVANCY ALL) FUZZY SUBJECT apple")
			if got != "" || !strings.HasPrefix(tagged, "T NO [SERVERBUG]") {
				t.Fatalf("got %q, %q; want NO [SERVERBUG] and no ESEARCH", got, tagged)
			}
			// Scores are not read unless the client asked for them.
			if _, tagged := relevancyResult(t, w, "SEARCH RETURN (ALL) FUZZY SUBJECT apple"); !strings.HasPrefix(tagged, "T OK") {
				t.Fatalf("SEARCH without RELEVANCY = %q", tagged)
			}
		})
	}
}

// RFC 6203 section 4: RELEVANCY MUST NOT be used without a FUZZY key, and the
// option exists only where SEARCH=FUZZY is advertised. Both are refused with
// BAD before the backend searches.
func TestSearchReturnRelevancyRefusals(t *testing.T) {
	b := &relevancyBackend{Backend: r4Memory(), mode: "uid"}
	w := relevancyOpen(t, b)
	b.searches = nil
	for _, command := range []string{
		"SEARCH RETURN (RELEVANCY) SUBJECT apple",
		"UID SORT RETURN (RELEVANCY) (ARRIVAL) UTF-8 SUBJECT apple",
		`ESEARCH IN (selected) RETURN (RELEVANCY) SUBJECT apple`,
	} {
		if _, tagged := relevancyResult(t, w, command); !strings.HasPrefix(tagged, "T BAD") {
			t.Errorf("%s = %q, want BAD", command, tagged)
		}
	}
	if len(b.searches) != 0 {
		t.Errorf("refused commands reached Search %d times", len(b.searches))
	}

	b = &relevancyBackend{Backend: r4Memory(), mode: "uid", noFuzzy: true}
	w = relevancyOpen(t, b)
	if _, tagged := relevancyResult(t, w, "SEARCH RETURN (RELEVANCY) SUBJECT apple"); !strings.HasPrefix(tagged, "T BAD") {
		t.Errorf("RELEVANCY without SEARCH=FUZZY = %q, want BAD", tagged)
	}
}

// RELEVANCY alone returns every match's score, so a MODSEQ search reports the
// highest clock among them (RFC 4731 section 3.2) after the scores.
func TestSearchReturnRelevancyReportsModSeq(t *testing.T) {
	w := relevancyOpen(t, &relevancyBackend{Backend: r4Memory(), mode: "uid"})
	got, tagged := relevancyResult(t, w, "SEARCH RETURN (RELEVANCY) FUZZY SUBJECT apple MODSEQ 1")
	if !strings.HasPrefix(tagged, "T OK") || !strings.HasPrefix(got, `* ESEARCH (TAG "T") RELEVANCY (10 30 50) MODSEQ `) {
		t.Fatalf("got %q, %q", got, tagged)
	}
}

// RFC 6203 section 6: SORT RETURN (RELEVANCY) lists the scores in the sorted
// order. SortMailbox returns no scores, so they come from Search.
func TestSortReturnRelevancy(t *testing.T) {
	cases := []struct{ command, want string }{
		{"SORT RETURN (ALL RELEVANCY) (REVERSE ARRIVAL) UTF-8 FUZZY SUBJECT apple", `* ESEARCH (TAG "T") ALL 4,2,1 RELEVANCY (50 30 10)`},
		{"UID SORT RETURN (RELEVANCY) (REVERSE ARRIVAL) UTF-8 FUZZY SUBJECT apple", `* ESEARCH (TAG "T") UID RELEVANCY (50 30 10)`},
		{"UID SORT RETURN (MIN RELEVANCY) (REVERSE ARRIVAL) UTF-8 FUZZY SUBJECT apple", `* ESEARCH (TAG "T") UID MIN 5 RELEVANCY (50)`},
		{"UID SORT RETURN (PARTIAL 2:3 RELEVANCY) (REVERSE ARRIVAL) UTF-8 FUZZY SUBJECT apple", `* ESEARCH (TAG "T") UID PARTIAL (2:3 3,1) RELEVANCY (30 10)`},
		{"SORT RETURN (RELEVANCY ALL) (RELEVANCY) UTF-8 FUZZY SUBJECT zebra", `* ESEARCH (TAG "T") RELEVANCY ()`},
	}
	b := &relevancyBackend{Backend: r4Memory(), mode: "uid"}
	w := relevancyOpen(t, b)
	for _, test := range cases {
		got, tagged := relevancyResult(t, w, test.command)
		if !strings.HasPrefix(tagged, "T OK") || got != test.want {
			t.Errorf("%s\n got %q, %q\nwant %q", test.command, got, tagged, test.want)
		}
	}
	b.mode = "high"
	if got, tagged := relevancyResult(t, w, "SORT RETURN (RELEVANCY) (ARRIVAL) UTF-8 FUZZY SUBJECT apple"); got != "" || !strings.HasPrefix(tagged, "T NO [SERVERBUG]") {
		t.Errorf("SORT with an invalid score = %q, %q", got, tagged)
	}
}

// MULTISEARCH shares SEARCH's return options, RELEVANCY included.
func TestMultiSearchReturnRelevancy(t *testing.T) {
	for _, test := range []struct{ mode, want string }{
		{"uid", ` UID ALL 1,3,5 RELEVANCY (10 30 50)`},
		{"none", ` UID ALL 1,3,5 RELEVANCY (100 100 100)`},
		{"memory", ` UID ALL 1,3,5 RELEVANCY (100 100 100)`},
	} {
		t.Run(test.mode, func(t *testing.T) {
			w := relevancyOpen(t, &relevancyBackend{Backend: r4Memory(), mode: test.mode})
			got, tagged := relevancyResult(t, w, `ESEARCH IN (mailboxes INBOX) RETURN (ALL RELEVANCY) FUZZY SUBJECT apple`)
			if !strings.HasPrefix(tagged, "T OK") || !strings.HasPrefix(got, `* ESEARCH (TAG "T" MAILBOX INBOX UIDVALIDITY `) || !strings.HasSuffix(got, test.want) {
				t.Fatalf("got %q, %q; want suffix %q", got, tagged, test.want)
			}
		})
	}
}
