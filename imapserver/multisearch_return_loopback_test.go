package imapserver_test

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

// esearchLines maps each ESEARCH response's MAILBOX correlator to its line.
func esearchLines(lines []string) map[string]string {
	byMailbox := make(map[string]string)
	for _, line := range lines {
		if !strings.HasPrefix(line, "* ESEARCH ") {
			continue
		}
		if name, ok := esearchMailbox(line); ok {
			byMailbox[name] = strings.TrimRight(line, "\r\n")
		}
	}
	return byMailbox
}

// TestMultiSearchReturnOptions checks that ESEARCH accepts the result options
// of extended SEARCH and evaluates them per mailbox (RFC 7377 sections 2.1 and
// 2.2), including the rules for SAVE and UPDATE.
func TestMultiSearchReturnOptions(t *testing.T) {
	client, reader := reviewRawSession(t, reviewBackend(t, "memory"), nil)
	for name, count := range map[string]int{"A": 3, "B": 1, "Empty": 0} {
		reviewCommand(t, client, reader, "C", "CREATE "+name, "OK")
		for i := range count {
			raw := fmt.Sprintf("From: alice@example.test\r\nSubject: %s %d\r\n\r\nbody\r\n", name, i)
			reviewCommand(t, client, reader, "P", fmt.Sprintf("APPEND %s {%d+}\r\n%s", name, len(raw), raw), "OK")
		}
	}
	search := func(command string) map[string]string {
		t.Helper()
		return esearchLines(reviewCommand(t, client, reader, "Q", command, "OK"))
	}
	for _, tc := range []struct {
		command string
		want    map[string]string // MAILBOX -> items after "UID "
	}{
		{"ESEARCH IN (mailboxes (A B Empty)) RETURN (MIN MAX COUNT) ALL",
			map[string]string{"A": "MIN 1 MAX 3 COUNT 3", "B": "MIN 1 MAX 1 COUNT 1"}},
		{"ESEARCH IN (mailboxes (A B)) RETURN (COUNT) ALL", map[string]string{"A": "COUNT 3", "B": "COUNT 1"}},
		{"ESEARCH IN (mailboxes (A B)) RETURN () ALL", map[string]string{"A": "ALL 1:3", "B": "ALL 1"}},
		{"ESEARCH IN (mailboxes A) ALL", map[string]string{"A": "ALL 1:3"}},
		{"ESEARCH IN (mailboxes A) RETURN (PARTIAL 1:2) ALL", map[string]string{"A": "PARTIAL (1:2 1:2)"}},
		{"ESEARCH IN (mailboxes A) RETURN (CONTEXT COUNT) ALL", map[string]string{"A": "COUNT 3"}},
		{"ESEARCH IN (mailboxes (A Empty)) RETURN (COUNT) SUBJECT nothing", map[string]string{}},
	} {
		got := search(tc.command)
		if len(got) != len(tc.want) {
			t.Errorf("%s = %q, want mailboxes %q", tc.command, got, tc.want)
			continue
		}
		for mailbox, items := range tc.want {
			if line := got[mailbox]; !strings.HasSuffix(line, " UID "+items) {
				t.Errorf("%s: %s = %q, want UID %s", tc.command, mailbox, line, items)
			}
		}
	}

	// SAVE needs the selected mailbox as the sole source; UPDATE a selection
	// that the command searches.
	reviewCommand(t, client, reader, "Q", "ESEARCH RETURN (SAVE) ALL", "BAD")
	reviewCommand(t, client, reader, "Q", "ESEARCH IN (mailboxes A) RETURN (UPDATE) ALL", "BAD")
	reviewCommand(t, client, reader, "S", "SELECT A", "OK")
	reviewCommand(t, client, reader, "Q", "ESEARCH IN (mailboxes A) RETURN (SAVE) ALL", "BAD")
	reviewCommand(t, client, reader, "Q", "ESEARCH IN (selected mailboxes B) RETURN (SAVE) ALL", "BAD")
	reviewCommand(t, client, reader, "Q", "ESEARCH IN (mailboxes B) RETURN (UPDATE) ALL", "BAD")

	// SAVE alone returns no data and replaces "$"; with MIN it saves that one.
	if got := search(`ESEARCH RETURN (SAVE) SUBJECT "A 1"`); len(got) != 0 {
		t.Errorf("SAVE alone returned data: %q", got)
	}
	if lines := reviewCommand(t, client, reader, "Q", "UID SEARCH $", "OK"); !slices.Contains(lines, "* SEARCH 2") {
		t.Errorf("saved result = %q, want UID 2", lines)
	}
	if got := search("ESEARCH IN (selected) RETURN (SAVE MIN) ALL"); !strings.HasSuffix(got["A"], " UID MIN 1") {
		t.Errorf("SAVE MIN = %q", got)
	}
	if lines := reviewCommand(t, client, reader, "Q", "UID SEARCH $", "OK"); !slices.Contains(lines, "* SEARCH 1") {
		t.Errorf("saved MIN = %q, want UID 1", lines)
	}
	search("ESEARCH RETURN (SAVE) SUBJECT nothing")
	if lines := reviewCommand(t, client, reader, "Q", "UID SEARCH $", "OK"); !slices.Contains(lines, "* SEARCH") {
		t.Errorf("saved empty result = %q", lines)
	}

	// UPDATE registers a context for the selected mailbox; its updates carry
	// the MULTISEARCH correlators.
	got := search("ESEARCH IN (mailboxes (A B)) RETURN (COUNT UPDATE) ALL")
	if !strings.HasSuffix(got["A"], " UID COUNT 3") || !strings.HasSuffix(got["B"], " UID COUNT 1") {
		t.Errorf("UPDATE initial result = %q", got)
	}
	validity := regexp.MustCompile(`MAILBOX A UIDVALIDITY (\d+)\)`).FindStringSubmatch(got["A"])
	if validity == nil {
		t.Fatalf("no UIDVALIDITY correlator: %q", got["A"])
	}
	raw := "Subject: A new\r\n\r\nbody\r\n"
	lines := reviewCommand(t, client, reader, "P", fmt.Sprintf("APPEND A {%d+}\r\n%s", len(raw), raw), "OK")
	lines = append(lines, reviewCommand(t, client, reader, "N", "NOOP", "OK")...)
	want := `* ESEARCH (TAG "Q" MAILBOX A UIDVALIDITY ` + validity[1] + `) UID ADDTO (0 4)`
	if !strings.Contains(strings.Join(lines, ""), want) {
		t.Errorf("context update = %q, want %s", lines, want)
	}
	reviewCommand(t, client, reader, "X", `CANCELUPDATE "Q"`, "OK")
}

// TestMultiSearchModSeq checks RFC 7162 section 3.1.5 and RFC 4731 section 3.2
// for ESEARCH: MIN and MAX report their own messages' clock, ALL the highest.
func TestMultiSearchModSeq(t *testing.T) {
	client, reader := reviewRawSession(t, reviewBackend(t, "memory"), nil)
	reviewCommand(t, client, reader, "E", "ENABLE CONDSTORE", "OK")
	reviewCommand(t, client, reader, "C", "CREATE A", "OK")
	for i := range 3 {
		raw := fmt.Sprintf("Subject: %d\r\n\r\nbody\r\n", i)
		reviewCommand(t, client, reader, "P", fmt.Sprintf("APPEND A {%d+}\r\n%s", len(raw), raw), "OK")
	}
	reviewCommand(t, client, reader, "S", "SELECT A", "OK")
	reviewCommand(t, client, reader, "F", `STORE 2 +FLAGS (\Flagged)`, "OK")
	modSeqs := make(map[string]uint64)
	for _, line := range reviewCommand(t, client, reader, "F", "UID FETCH 1:* (MODSEQ)", "OK") {
		if match := regexp.MustCompile(`UID (\d+) MODSEQ \((\d+)\)|MODSEQ \((\d+)\) UID (\d+)`).FindStringSubmatch(line); match != nil {
			uid, value := match[1], match[2]
			if uid == "" {
				uid, value = match[4], match[3]
			}
			modSeqs[uid], _ = strconv.ParseUint(value, 10, 64)
		}
	}
	if len(modSeqs) != 3 {
		t.Fatalf("mod-sequences = %v", modSeqs)
	}
	highest := max(modSeqs["1"], modSeqs["2"], modSeqs["3"])
	reviewCommand(t, client, reader, "U", "UNSELECT", "OK")
	for _, tc := range []struct {
		options string
		want    uint64
	}{
		{"MIN", modSeqs["1"]},
		{"MAX", modSeqs["3"]},
		{"MIN MAX", max(modSeqs["1"], modSeqs["3"])},
		{"ALL", highest},
		{"MIN COUNT", highest},
		{"PARTIAL 2:2", modSeqs["2"]},
	} {
		lines := reviewCommand(t, client, reader, "Q", "ESEARCH IN (mailboxes A) RETURN ("+tc.options+") MODSEQ 1", "OK")
		if line := esearchLines(lines)["A"]; !strings.HasSuffix(line, fmt.Sprintf(" MODSEQ %d", tc.want)) {
			t.Errorf("RETURN (%s) = %q, want MODSEQ %d", tc.options, line, tc.want)
		}
	}
}

// TestMultiSearchMailboxCorrelatorEncoding checks that MAILBOX uses the
// negotiated mailbox encoding: modified UTF-7 unless UTF8=ACCEPT or IMAP4rev2
// is enabled, and that LIST and STATUS switch with it.
func TestMultiSearchMailboxCorrelatorEncoding(t *testing.T) {
	for _, enable := range []string{"UTF8=ACCEPT", "IMAP4rev2"} {
		t.Run(enable, func(t *testing.T) { testMailboxEncoding(t, enable) })
	}
}

func testMailboxEncoding(t *testing.T, enable string) {
	client, reader := reviewRawSession(t, reviewBackend(t, "memory"), nil)
	const utf7 = "Gr&APwA3w-e"
	reviewCommand(t, client, reader, "C", "CREATE "+utf7, "OK")
	raw := "Subject: x\r\n\r\nbody\r\n"
	reviewCommand(t, client, reader, "P", fmt.Sprintf("APPEND %s {%d+}\r\n%s", utf7, len(raw), raw), "OK")
	lines := reviewCommand(t, client, reader, "Q", "ESEARCH IN (mailboxes "+utf7+") ALL", "OK")
	if _, ok := esearchLines(lines)[utf7]; !ok {
		t.Errorf("rev1 MAILBOX correlator = %q, want %s", lines, utf7)
	}
	reviewCommand(t, client, reader, "E", "ENABLE "+enable, "OK")
	// UTF-8 names may arrive quoted or as literals; either way not as UTF-7.
	for _, command := range []string{`ESEARCH IN (mailboxes "Grüße") ALL`, `LIST "" "Gr*"`, `STATUS "Grüße" (MESSAGES)`} {
		wire := strings.Join(reviewCommand(t, client, reader, "L", command, "OK"), "")
		if !strings.Contains(wire, "Grüße") || strings.Contains(wire, utf7) {
			t.Errorf("%s after ENABLE %s = %q, want the UTF-8 name", command, enable, wire)
		}
	}
}

// TestMemorySortThreadSearchAgree checks that memory's SORT, THREAD and
// MULTISEARCH see the save dates and EMAILIDs SEARCH sees.
func TestMemorySortThreadSearchAgree(t *testing.T) {
	client, reader := reviewRawSession(t, reviewBackend(t, "memory"), nil)
	for i := range 2 {
		raw := fmt.Sprintf("Subject: m%d\r\n\r\nbody %d\r\n", i, i)
		reviewCommand(t, client, reader, "P", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
	}
	reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
	var emailID string
	for _, line := range reviewCommand(t, client, reader, "F", "FETCH 2 (EMAILID)", "OK") {
		if match := regexp.MustCompile(`EMAILID \(([^)]+)\)`).FindStringSubmatch(line); match != nil {
			emailID = match[1]
		}
	}
	if emailID == "" {
		t.Fatal("no EMAILID")
	}
	for command, want := range map[string]string{
		"UID SEARCH SAVEDSINCE 1-Jan-2000":                      "* SEARCH 1 2",
		"UID SORT (SUBJECT) UTF-8 SAVEDSINCE 1-Jan-2000":        "* SORT 1 2",
		"UID THREAD ORDEREDSUBJECT UTF-8 SAVEDSINCE 1-Jan-2000": "* THREAD (1)(2)",
		"UID SORT (SUBJECT) UTF-8 EMAILID " + emailID:           "* SORT 2",
		"ESEARCH SAVEDSINCE 1-Jan-2000":                         "UID ALL 1:2",
		"ESEARCH EMAILID " + emailID:                            "UID ALL 2",
	} {
		lines := reviewCommand(t, client, reader, "Q", command, "OK")
		if !strings.Contains(strings.ReplaceAll(strings.Join(lines, ""), ") (", ")("), want) {
			t.Errorf("%s = %q, want %s", command, lines, want)
		}
	}
}

// TestMultiSearchClientReturnOptions round-trips MIN, MAX and COUNT through
// imapclient, which sends every option in one RETURN list.
func TestMultiSearchClientReturnOptions(t *testing.T) {
	client, _ := newGroupAClient(t, t.Context())
	t.Cleanup(func() { _ = client.Close() })
	seedMessages(t, t.Context(), client, 3)
	data, err := client.MultiSearch(imap.SearchAll, &imapclient.MultiSearchOptions{
		Sources:       []imapclient.MultiSearchSource{imapclient.MultiSearchMailboxes{Names: []string{"INBOX"}}},
		ReturnOptions: []imapclient.SearchReturnOption{imapclient.SearchReturnMin, imapclient.SearchReturnMax, imapclient.SearchReturnCount},
	}).Wait(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Results) != 1 {
		t.Fatalf("results = %+v", data.Results)
	}
	got := data.Results[0].Data
	if !got.HasMin || got.Min != 1 || !got.HasMax || got.Max != 3 || !got.HasCount || got.Count != 3 {
		t.Fatalf("MULTISEARCH MIN/MAX/COUNT = %+v", got)
	}
}

// TestUTF8AcceptCharsetConflicts checks RFC 9755 section 3: after ENABLE
// UTF8=ACCEPT an optional CHARSET is refused, and a mandatory one must be UTF-8.
func TestUTF8AcceptCharsetConflicts(t *testing.T) {
	client, reader := reviewRawSession(t, reviewBackend(t, "memory"), nil)
	reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
	reviewCommand(t, client, reader, "Q", "SEARCH CHARSET US-ASCII ALL", "OK")
	reviewCommand(t, client, reader, "U", "UNSELECT", "OK")
	reviewCommand(t, client, reader, "E", "ENABLE UTF8=ACCEPT", "OK")
	reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
	for command, status := range map[string]string{
		"SEARCH CHARSET UTF-8 ALL":                       "BAD",
		"UID SEARCH RETURN (COUNT) CHARSET US-ASCII ALL": "BAD",
		"ESEARCH CHARSET UTF-8 ALL":                      "BAD",
		"SORT (SUBJECT) US-ASCII ALL":                    "BAD",
		"THREAD ORDEREDSUBJECT US-ASCII ALL":             "BAD",
		"SEARCH ALL":                                     "OK",
		"SORT (SUBJECT) utf-8 ALL":                       "OK",
		"THREAD ORDEREDSUBJECT UTF-8 ALL":                "OK",
	} {
		reviewCommand(t, client, reader, "Q", command, status)
	}
}
