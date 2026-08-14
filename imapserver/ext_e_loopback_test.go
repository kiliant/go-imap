package imapserver_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// LANGUAGE reports what is available, then adopts one. RFC 5255 section 3.2.
func TestLoopbackLanguage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "E1 LANGUAGE\r\n")
	untagged, tagged := collectUntilTag(t, reader, "E1 ")
	if !strings.HasPrefix(tagged, "E1 OK") {
		t.Fatalf("LANGUAGE failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* LANGUAGE"); !strings.Contains(line, "en") {
		t.Errorf("LANGUAGE = %q, want the available tags", line)
	}

	// RFC 4647 matches by prefix, so en-GB selects en — and the response says
	// which tag was actually adopted, not which was asked for.
	writeRawCommand(t, clientSide, "E2 LANGUAGE \"en-GB\"\r\n")
	untagged, tagged = collectUntilTag(t, reader, "E2 ")
	if !strings.HasPrefix(tagged, "E2 OK") {
		t.Fatalf("LANGUAGE en-GB failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* LANGUAGE"); !strings.Contains(line, `"en"`) {
		t.Errorf("adopted language = %q, want en", line)
	}

	writeRawCommand(t, clientSide, "E3 LANGUAGE \"xx\"\r\n")
	if _, tagged := collectUntilTag(t, reader, "E3 "); !strings.HasPrefix(tagged, "E3 NO") {
		t.Errorf("an unavailable language was accepted: %q", tagged)
	}
}

// The URLAUTH round trip, and the property that actually matters: a forged
// token must not grant access.
func TestLoopbackURLAuth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	url := "imap://alice@example.com/INBOX/;UID=1"
	writeRawCommand(t, clientSide, "E1 GENURLAUTH \""+url+"\" INTERNAL\r\n")
	untagged, tagged := collectUntilTag(t, reader, "E1 ")
	if !strings.HasPrefix(tagged, "E1 OK") {
		t.Fatalf("GENURLAUTH failed: %q", tagged)
	}
	line := findResponse(t, untagged, "* GENURLAUTH")
	authorized := strings.Trim(strings.TrimPrefix(line, "* GENURLAUTH "), `"`)
	if !strings.Contains(authorized, ":internal:") {
		t.Fatalf("GENURLAUTH did not return an authorized URL: %q", line)
	}

	writeRawCommand(t, clientSide, "E2 URLFETCH \""+authorized+"\"\r\n")
	untagged, tagged = collectUntilTag(t, reader, "E2 ")
	if !strings.HasPrefix(tagged, "E2 OK") {
		t.Fatalf("URLFETCH failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* URLFETCH"); strings.Contains(line, "NIL") {
		t.Errorf("a valid URL was not resolved: %q", line)
	}

	// A tampered token resolves to NIL. This is the security property, not a
	// formatting detail: if it passed, anyone could mint access to any message.
	forged := authorized[:len(authorized)-4] + "AAAA"
	writeRawCommand(t, clientSide, "E3 URLFETCH \""+forged+"\"\r\n")
	untagged, tagged = collectUntilTag(t, reader, "E3 ")
	if !strings.HasPrefix(tagged, "E3 OK") {
		t.Fatalf("URLFETCH of a forged URL should still succeed as a command: %q", tagged)
	}
	if line := findResponse(t, untagged, "* URLFETCH"); !strings.Contains(line, "NIL") {
		t.Errorf("a forged token was honoured: %q", line)
	}

	// RESETKEY revokes every token minted so far.
	writeRawCommand(t, clientSide, "E4 RESETKEY\r\n")
	if _, tagged := collectUntilTag(t, reader, "E4 "); !strings.HasPrefix(tagged, "E4 OK") {
		t.Fatalf("RESETKEY failed: %q", tagged)
	}
	writeRawCommand(t, clientSide, "E5 URLFETCH \""+authorized+"\"\r\n")
	untagged, _ = collectUntilTag(t, reader, "E5 ")
	if line := findResponse(t, untagged, "* URLFETCH"); !strings.Contains(line, "NIL") {
		t.Errorf("RESETKEY did not revoke the existing URL: %q", line)
	}
}

// ESORT returns the SORT result in ESEARCH's shape. MIN and MAX are the first
// and last of the *sorted* order, which is what makes them different from
// SEARCH's. RFC 5267 section 4.
func TestLoopbackESort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "E1 SORT RETURN (MIN MAX COUNT) (REVERSE SUBJECT) UTF-8 ALL\r\n")
	untagged, tagged := collectUntilTag(t, reader, "E1 ")
	if !strings.HasPrefix(tagged, "E1 OK") {
		t.Fatalf("ESORT failed: %q", tagged)
	}
	line := findResponse(t, untagged, "* ESEARCH")
	// Reversed by subject, so the first of the order is message 3.
	if !strings.Contains(line, "MIN 3") || !strings.Contains(line, "MAX 1") {
		t.Errorf("ESORT MIN/MAX = %q, want the ends of the sorted order", line)
	}
	if !strings.Contains(line, "COUNT 3") {
		t.Errorf("ESORT COUNT = %q", line)
	}

	// ALL preserves the sorted order rather than collapsing it into ranges,
	// which would re-sort it.
	writeRawCommand(t, clientSide, "E2 SORT RETURN (ALL) (REVERSE SUBJECT) UTF-8 ALL\r\n")
	untagged, _ = collectUntilTag(t, reader, "E2 ")
	if line := findResponse(t, untagged, "* ESEARCH"); !strings.Contains(line, "ALL 3,2,1") {
		t.Errorf("ESORT ALL = %q, want the order preserved", line)
	}
}

// What remains unadvertised, and why: UTF8=ALL and UTF8=USER are deprecated by
// RFC 9755, and UTF8=ONLY asserts the server refuses ASCII-only clients, which
// this framework does not do. Advertising any of them would be a claim the
// server cannot honour.
func TestGroupEUnadvertisedCapabilities(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "E1 CAPABILITY\r\n")
	untagged, _ := collectUntilTag(t, reader, "E1 ")
	line := findResponse(t, untagged, "* CAPABILITY")
	for _, absent := range []string{"UTF8=ALL", "UTF8=USER", "UTF8=ONLY"} {
		if strings.Contains(line, absent) {
			t.Errorf("%s is advertised but not implemented: %q", absent, line)
		}
	}
	for _, present := range []string{"LANGUAGE", "URLAUTH", "ESORT", "I18NLEVEL=1", "I18NLEVEL=2", "FILTERS", "CONTEXT=SEARCH", "CONTEXT=SORT"} {
		if !strings.Contains(line, present) {
			t.Errorf("%s is implemented but not advertised: %q", present, line)
		}
	}
}

// A full SCRAM-SHA-256 exchange against the server, driven by the real client.
// The client mechanism verifies the server's signature, so a passing exchange
// proves mutual authentication rather than just that the server said OK.
func TestLoopbackSCRAMAuthentication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	backend := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})
	client, _ := openLoopbackClient(t, ctx, server, &imapclient.Options{AllowInsecureAuth: true})

	if err := client.Capability(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !client.Capabilities()["AUTH=SCRAM-SHA-256"] {
		t.Fatalf("AUTH=SCRAM-SHA-256 is not advertised: %v", client.Capabilities())
	}
	if err := client.Authenticate(ctx, "alice", "secret", &imapclient.AuthenticateOptions{Mechanism: "SCRAM-SHA-256"}); err != nil {
		t.Fatalf("SCRAM-SHA-256 authentication failed: %v", err)
	}
	if _, err := client.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatalf("SELECT after SCRAM failed: %v", err)
	}
}

// A wrong password must fail, and must fail the same way an unknown user does.
func TestLoopbackSCRAMRejectsBadPassword(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	backend := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})

	client, _ := openLoopbackClient(t, ctx, server, &imapclient.Options{AllowInsecureAuth: true})
	if err := client.Authenticate(ctx, "alice", "wrong", &imapclient.AuthenticateOptions{Mechanism: "SCRAM-SHA-256"}); err == nil {
		t.Error("a wrong password was accepted")
	}

	other, _ := openLoopbackClient(t, ctx, server, &imapclient.Options{AllowInsecureAuth: true})
	if err := other.Authenticate(ctx, "nobody", "secret", &imapclient.AuthenticateOptions{Mechanism: "SCRAM-SHA-256"}); err == nil {
		t.Error("an unknown user was accepted")
	}
}

// Two backends configured with the same username and different passwords must
// not share SCRAM state. A cache keyed only by username would let the second
// backend authenticate against the first one's password.
func TestLoopbackSCRAMDerivationsAreNotShared(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	first := imapserver.New(memory.New(&memory.Options{
		Users: map[string]string{"alice": "secret"},
	}), &imapserver.Options{AllowInsecureAuth: true})
	second := imapserver.New(memory.New(&memory.Options{
		Users: map[string]string{"alice": "different"},
	}), &imapserver.Options{AllowInsecureAuth: true})

	// Prime the first backend's derivation.
	client, _ := openLoopbackClient(t, ctx, first, &imapclient.Options{AllowInsecureAuth: true})
	if err := client.Authenticate(ctx, "alice", "secret", &imapclient.AuthenticateOptions{Mechanism: "SCRAM-SHA-256"}); err != nil {
		t.Fatalf("first backend: %v", err)
	}

	// The second backend must reject the first's password and accept its own.
	wrong, _ := openLoopbackClient(t, ctx, second, &imapclient.Options{AllowInsecureAuth: true})
	if err := wrong.Authenticate(ctx, "alice", "secret", &imapclient.AuthenticateOptions{Mechanism: "SCRAM-SHA-256"}); err == nil {
		t.Error("the second backend accepted the first backend's password")
	}
	right, _ := openLoopbackClient(t, ctx, second, &imapclient.Options{AllowInsecureAuth: true})
	if err := right.Authenticate(ctx, "alice", "different", &imapclient.AuthenticateOptions{Mechanism: "SCRAM-SHA-256"}); err != nil {
		t.Errorf("the second backend rejected its own password: %v", err)
	}
}

// COMPARATOR negotiates how string SEARCH keys are compared. RFC 5255 section 4.
func TestLoopbackComparator(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	// With no arguments it reports state rather than changing it.
	writeRawCommand(t, clientSide, "E1 COMPARATOR\r\n")
	untagged, tagged := collectUntilTag(t, reader, "E1 ")
	if !strings.HasPrefix(tagged, "E1 OK") {
		t.Fatalf("COMPARATOR failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* COMPARATOR"); !strings.Contains(line, "i;unicode-casemap") {
		t.Errorf("COMPARATOR = %q, want the default active collation", line)
	}

	writeRawCommand(t, clientSide, "E2 COMPARATOR \"i;octet\"\r\n")
	untagged, tagged = collectUntilTag(t, reader, "E2 ")
	if !strings.HasPrefix(tagged, "E2 OK") {
		t.Fatalf("COMPARATOR selection failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* COMPARATOR"); !strings.Contains(line, "i;octet") {
		t.Errorf("adopted comparator = %q, want i;octet", line)
	}

	// A request naming nothing servable gets BADCOMPARATOR, so a client can
	// tell it apart from an ordinary failure and fall back.
	writeRawCommand(t, clientSide, "E3 COMPARATOR \"i;nonexistent\"\r\n")
	if _, tagged := collectUntilTag(t, reader, "E3 "); !strings.Contains(tagged, "BADCOMPARATOR") {
		t.Errorf("unservable comparator = %q, want BADCOMPARATOR", tagged)
	}
}

// CONTEXT=SEARCH keeps reporting changes to a search result after the command
// finished — the notification lifetime that made this the last capability to
// land. RFC 5267.
func TestLoopbackSearchContextUpdate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "E1 SEARCH RETURN (ALL UPDATE) ALL\r\n")
	untagged, tagged := collectUntilTag(t, reader, "E1 ")
	if !strings.HasPrefix(tagged, "E1 OK") {
		t.Fatalf("SEARCH RETURN (UPDATE) failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* ESEARCH"); !strings.Contains(line, "ALL") {
		t.Fatalf("ESEARCH = %q", line)
	}

	// Removing a matching message must produce a REMOVEFROM tagged with the
	// registering command's tag, so the client knows which result changed.
	writeRawCommand(t, clientSide, "E2 STORE 2 +FLAGS (\\Deleted)\r\n")
	collectUntilTag(t, reader, "E2 ")
	writeRawCommand(t, clientSide, "E3 EXPUNGE\r\n")
	untagged, tagged = collectUntilTag(t, reader, "E3 ")
	if !strings.HasPrefix(tagged, "E3 OK") {
		t.Fatalf("EXPUNGE failed: %q", tagged)
	}
	var removeFrom string
	for _, line := range untagged {
		if strings.Contains(line, "REMOVEFROM") {
			removeFrom = line
		}
	}
	if removeFrom == "" {
		t.Fatalf("no REMOVEFROM for the registered context: %v", untagged)
	}
	if !strings.Contains(removeFrom, `"E1"`) {
		t.Errorf("REMOVEFROM is not tagged with the registering command: %q", removeFrom)
	}

	// CANCELUPDATE stops it.
	writeRawCommand(t, clientSide, "E4 CANCELUPDATE \"E1\"\r\n")
	if _, tagged := collectUntilTag(t, reader, "E4 "); !strings.HasPrefix(tagged, "E4 OK") {
		t.Fatalf("CANCELUPDATE failed: %q", tagged)
	}
	writeRawCommand(t, clientSide, "E5 STORE 1 +FLAGS (\\Deleted)\r\n")
	collectUntilTag(t, reader, "E5 ")
	writeRawCommand(t, clientSide, "E6 EXPUNGE\r\n")
	untagged, _ = collectUntilTag(t, reader, "E6 ")
	for _, line := range untagged {
		if strings.Contains(line, "REMOVEFROM") {
			t.Errorf("CANCELUPDATE did not stop context updates: %q", line)
		}
	}
}

// FILTERS lets a client name a saved search instead of restating it. RFC 5466.
func TestLoopbackSearchFilters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	// All three seeded messages are unseen, so the saved "unseen" filter
	// matches them all.
	writeRawCommand(t, clientSide, "E1 SEARCH FILTER \"unseen\"\r\n")
	untagged, tagged := collectUntilTag(t, reader, "E1 ")
	if !strings.HasPrefix(tagged, "E1 OK") {
		t.Fatalf("SEARCH FILTER failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* SEARCH"); strings.TrimSpace(line) != "* SEARCH 1 2 3" {
		t.Errorf("SEARCH FILTER unseen = %q, want all three", line)
	}

	// The filter composes with ordinary criteria rather than replacing them.
	writeRawCommand(t, clientSide, "E2 STORE 1 +FLAGS (\\Flagged)\r\n")
	collectUntilTag(t, reader, "E2 ")
	writeRawCommand(t, clientSide, "E3 SEARCH FILTER \"flagged\" FILTER \"unseen\"\r\n")
	untagged, tagged = collectUntilTag(t, reader, "E3 ")
	if !strings.HasPrefix(tagged, "E3 OK") {
		t.Fatalf("composed SEARCH FILTER failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* SEARCH"); strings.TrimSpace(line) != "* SEARCH 1" {
		t.Errorf("composed filters = %q, want only message 1", line)
	}

	// An undefined name is UNDEFINED-FILTER, not an empty result — the client
	// must be able to tell "no such filter" from "nothing matched".
	writeRawCommand(t, clientSide, "E4 SEARCH FILTER \"nosuchfilter\"\r\n")
	if _, tagged := collectUntilTag(t, reader, "E4 "); !strings.Contains(tagged, "UNDEFINED-FILTER") {
		t.Errorf("unknown filter = %q, want UNDEFINED-FILTER", tagged)
	}
}

// TestSearchQueryNormalisationGuarantee enforces the promise [SearchQuery.Criteria]
// makes to backends: no imap.SearchFilter reaches them, on any command that
// accepts a search key.
//
// This is the test docs/API-STABILITY.md section 10 requires before a new
// imap.SearchCriteria implementation may be added to the frozen root package.
// The rule is only sound while the substitution is exhaustive, and it was not:
// FILTER is a search key, so RFC 5256 and RFC 7377 make it legal in SORT, THREAD
// and ESEARCH, but substitution was wired into SEARCH alone. A backend compiled
// before RFC 5466 then received a criteria type it could not know, and its type
// switch fell to default — a silently empty result, indistinguishable from a
// correct search that matched nothing.
//
// Each command below therefore asserts the *substituted* answer rather than mere
// success. The memory backend's evaluator rejects an unsubstituted FILTER, so a
// regression fails the command outright; asserting the answer also catches a
// backend that swallows it.
func TestSearchQueryNormalisationGuarantee(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	// Flag message 1 so the saved "flagged" filter has exactly one answer. An
	// all-three answer would coincide with "matched everything", which is what a
	// broken substitution can also look like.
	writeRawCommand(t, clientSide, "N1 STORE 1 +FLAGS (\\Flagged)\r\n")
	collectUntilTag(t, reader, "N1 ")

	for _, testCase := range []struct {
		name     string
		command  string
		response string
		want     string
	}{
		{"SEARCH", "SEARCH FILTER \"flagged\"", "* SEARCH", "* SEARCH 1"},
		{"SORT", "SORT (REVERSE DATE) UTF-8 FILTER \"flagged\"", "* SORT", "* SORT 1"},
		{"THREAD", "THREAD ORDEREDSUBJECT UTF-8 FILTER \"flagged\"", "* THREAD", "* THREAD (1)"},
		{"MULTISEARCH", "ESEARCH IN (\"INBOX\") FILTER \"flagged\"", "* ESEARCH", "ALL 1"},

		// Nested, not just top-level. A FILTER under a container key is the
		// case the first version of this test missed entirely: it only ever
		// placed the FILTER at the root, so a substitution walk that failed to
		// descend into FUZZY passed it. RFC 6203's FUZZY takes a search key, so
		// this is ordinary syntax rather than a contrived tree.
		{"SEARCH-fuzzy", "SEARCH FUZZY FILTER \"flagged\"", "* SEARCH", "* SEARCH 1"},
		{"SEARCH-not", "SEARCH NOT FILTER \"flagged\"", "* SEARCH", "* SEARCH 2"},
		{"SEARCH-or", "SEARCH OR FILTER \"flagged\" FILTER \"flagged\"", "* SEARCH", "* SEARCH 1"},
		{"SORT-fuzzy", "SORT (REVERSE DATE) UTF-8 FUZZY FILTER \"flagged\"", "* SORT", "* SORT 1"},
		{"MULTISEARCH-fuzzy", "ESEARCH IN (\"INBOX\") FUZZY FILTER \"flagged\"", "* ESEARCH", "ALL 1"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tag := "N" + testCase.name
			writeRawCommand(t, clientSide, tag+" "+testCase.command+"\r\n")
			untagged, tagged := collectUntilTag(t, reader, tag+" ")
			if !strings.HasPrefix(tagged, tag+" OK") {
				t.Fatalf("%s with FILTER failed: %q\nan unsubstituted imap.SearchFilter reached the backend", testCase.command, tagged)
			}
			line := findResponse(t, untagged, testCase.response)
			if !strings.Contains(line, testCase.want) {
				t.Errorf("%s = %q, want it to contain %q", testCase.command, strings.TrimSpace(line), testCase.want)
			}
		})
	}

	// The other half of the same guarantee: no imap.SearchSeqNum reaches a
	// backend either.
	//
	// SEARCH, SORT and THREAD resolve sequence numbers against the selection.
	// MULTISEARCH has no single selection when an IN clause names other
	// mailboxes, so there the number indexes into nothing and the command is
	// refused — the only two answers that do not hand a backend a criterion it
	// cannot evaluate.
	t.Run("seqnum-resolved-against-the-selection", func(t *testing.T) {
		writeRawCommand(t, clientSide, "NS1 SEARCH 1\r\n")
		untagged, tagged := collectUntilTag(t, reader, "NS1 ")
		if !strings.HasPrefix(tagged, "NS1 OK") {
			t.Fatalf("SEARCH by sequence number failed: %q", tagged)
		}
		if line := findResponse(t, untagged, "* SEARCH"); strings.TrimSpace(line) != "* SEARCH 1" {
			t.Errorf("SEARCH 1 = %q, want message 1", line)
		}
		// Without an IN clause the source is the selection, so numbers still
		// resolve.
		writeRawCommand(t, clientSide, "NS2 ESEARCH 1\r\n")
		untagged, tagged = collectUntilTag(t, reader, "NS2 ")
		if !strings.HasPrefix(tagged, "NS2 OK") {
			t.Fatalf("ESEARCH without IN by sequence number failed: %q", tagged)
		}
		if line := findResponse(t, untagged, "* ESEARCH"); !strings.Contains(line, "ALL 1") {
			t.Errorf("ESEARCH 1 = %q, want UID 1", line)
		}
	})

	t.Run("seqnum-refused-across-an-in-clause", func(t *testing.T) {
		for _, command := range []string{
			"ESEARCH IN (\"INBOX\") 1",
			// Nested, so the refusal cannot depend on the number being at the
			// top of the tree.
			"ESEARCH IN (\"INBOX\") NOT 1",
			"ESEARCH IN (\"INBOX\") FUZZY 1",
		} {
			writeRawCommand(t, clientSide, "NS3 "+command+"\r\n")
			_, tagged := collectUntilTag(t, reader, "NS3 ")
			if !strings.HasPrefix(tagged, "NS3 BAD") {
				t.Errorf("%q = %q, want BAD — a sequence number means nothing in a mailbox the client has not selected", command, tagged)
			}
		}
	})
}
