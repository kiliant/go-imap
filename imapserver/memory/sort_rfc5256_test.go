package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func TestSubjectBase(t *testing.T) {
	for _, tc := range []struct{ subject, want string }{
		{"", ""},
		{" ", ""},
		{"a", "a"},
		{"Re: a", "a"},
		{"RE:a", "a"},
		{"re  :  a", "a"},
		{"Fw: a", "a"},
		{"FWD: a", "a"},
		{"Re[2]: a", "a"},
		{"Re [list]: a", "a"},
		{"a (fwd)", "a"},
		{"a (FWD)  (fwd)\t", "a"},
		{"[foo] Fwd: [bar] Re: fw: b (fWd)  (fwd)", "b"},
		{"b (a)", "b (a)"},
		{"Re: [FWD: c]", "c"},
		// A blob is removed only when something remains after it.
		{"[xyz]", "[xyz]"},
		{"[list] topic", "topic"},
		{"[a] [b] topic", "topic"},
		{"Ad: Re: Ad: Re: Ad: x", "Ad: Re: Ad: Re: Ad: x"},
		{"re: [fwd: [fwd: re: [fwd: babylon]]]", "babylon"},
		{"a\r\n\tb   c", "a b c"},
		{"Reply: a", "Reply: a"},
	} {
		if got := subjectBase(tc.subject); got != tc.want {
			t.Errorf("subjectBase(%q) = %q, want %q", tc.subject, got, tc.want)
		}
	}
}

// sortTestMailbox appends one message per header block and selects INBOX.
func sortTestMailbox(t *testing.T, headers []string) *selected {
	t.Helper()
	backend := New(&Options{Users: map[string]string{"alice": "secret"}})
	authenticated, err := backend.Authenticate(t.Context(), nil, &imapserver.Credentials{Mechanism: "LOGIN", Username: "alice", Password: "secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authenticated.Close(context.Background(), nil) })
	s := authenticated.(*session)
	for _, header := range headers {
		raw := header + "\r\n\r\nbody\r\n"
		if _, err := s.Append(t.Context(), "INBOX", strings.NewReader(raw), nil); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.Select(t.Context(), "INBOX", &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { return nil }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result.Mailbox.(*selected)
}

func sortedUIDs(t *testing.T, m *selected, keys ...imap.SortKeySpec) string {
	t.Helper()
	query, err := imapserver.NewSearchQuery(imap.SearchAll, nil)
	if err != nil {
		t.Fatal(err)
	}
	uids, err := m.Sort(t.Context(), query, keys, nil)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Trim(fmt.Sprint(uids), "[]")
}

// The subjects and expected orders are those of imaptest's sort-subject
// script, including RFC 2047 encoded words that step (1) decodes.
func TestSortSubjectRFC5256(t *testing.T) {
	m := sortTestMailbox(t, []string{
		"Subject: a",
		"Subject: C",
		"Subject: b",
		"Subject: _",
		"Subject: [foo] Fwd: [bar] Re: fw: b (fWd)  (fwd)",
		"Subject: b (a)",
		"Subject: Re: [FWD: c]",
		"Subject: [xyz]",
		"X-No-Subject: yes",
		"Subject: =?iso-8859-1?q?_?=",
		"Subject: Re: =?utf-8?q?b?=",
		"Subject: =?iso-8859-1?q?RE:_C?=",
		"Subject: =?us-ascii?b?UmU6IGM=?=",
		"Subject: Ad: Re: Ad: Re: Ad: x",
		"Subject: re: [fwd: [fwd: re: [fwd: babylon]]]",
	})
	for _, tc := range []struct {
		keys []imap.SortKeySpec
		want string
	}{
		{[]imap.SortKeySpec{{Key: imap.SortKeySubject}}, "9 10 1 14 3 5 11 6 15 2 7 12 13 8 4"},
		{[]imap.SortKeySpec{{Key: imap.SortKeySubject, Reverse: true}}, "4 8 2 7 12 13 15 6 3 5 11 14 1 9 10"},
	} {
		if got := sortedUIDs(t, m, tc.keys...); got != tc.want {
			t.Errorf("SORT %v = %s, want %s", tc.keys, got, tc.want)
		}
	}
}

// FROM, TO and CC compare addr-mailbox alone, so the first two messages tie
// and keep their order (RFC 5256 section 3). The third message's Cc is a
// group, whose first address carries the group name as its mailbox.
func TestSortAddressRFC5256(t *testing.T) {
	m := sortTestMailbox(t, []string{
		"From: user2@domain2.org\r\nTo: foo bar <user2@domain2.org>\r\nCc: user2@domain2.org (foo bar)",
		"From: user2@domain1.org\r\nTo: user2@domain1.org\r\nCc: user2: blah@domain1.org;",
		"From: user1@domain1.org\r\nTo: user1@domain1.org\r\nCc: user1@domain1.org",
	})
	for _, key := range []imap.SortKey{imap.SortKeyFrom, imap.SortKeyTo, imap.SortKeyCc} {
		if got := sortedUIDs(t, m, imap.SortKeySpec{Key: key}); got != "3 1 2" {
			t.Errorf("SORT (%s) = %s, want 3 1 2", key, got)
		}
		if got := sortedUIDs(t, m, imap.SortKeySpec{Key: key, Reverse: true}); got != "1 2 3" {
			t.Errorf("SORT (REVERSE %s) = %s, want 1 2 3", key, got)
		}
	}
}

// RFC 5957 section 3: the decoded display name, else mailbox@host.
func TestSortDisplayRFC5957(t *testing.T) {
	m := sortTestMailbox(t, []string{
		"From: foo bar <a@b.c>",
		"From: b@c.d",
		"From: =?iso-8859-1?q?foo_aar?= <e@f.g>",
		"From: =?iso-8859-1?q?foo_car?= <f@g.h>",
	})
	if got := sortedUIDs(t, m, imap.SortKeySpec{Key: imap.SortKeyDisplayFrom}); got != "2 3 1 4" {
		t.Errorf("SORT (DISPLAYFROM) = %s, want 2 3 1 4", got)
	}
}

func TestThreadOrderedSubjectGroupsBaseSubjects(t *testing.T) {
	m := sortTestMailbox(t, []string{
		"Subject: one\r\nDate: Mon, 1 Jan 2024 10:00:00 +0000",
		"Subject: [list] Re: one\r\nDate: Mon, 1 Jan 2024 11:00:00 +0000",
		"Subject: Re: [FWD: one]\r\nDate: Mon, 1 Jan 2024 12:00:00 +0000",
		"Subject: two\r\nDate: Mon, 1 Jan 2024 09:00:00 +0000",
	})
	query, err := imapserver.NewSearchQuery(imap.SearchAll, nil)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := m.Thread(t.Context(), query, imap.ThreadOrderedSubject, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(roots)
	want := fmt.Sprint([]imap.ThreadNode{
		{Num: 4},
		{Num: 1, Children: []imap.ThreadNode{{Num: 2}, {Num: 3}}},
	})
	if got != want {
		t.Errorf("THREAD ORDEREDSUBJECT = %s, want %s", got, want)
	}
}
