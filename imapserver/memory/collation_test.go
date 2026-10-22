package memory

import (
	"fmt"
	"mime"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func TestSortActiveComparator(t *testing.T) {
	for _, key := range []imap.SortKey{imap.SortKeySubject, imap.SortKeyFrom, imap.SortKeyTo, imap.SortKeyCc, imap.SortKeyDisplayFrom, imap.SortKeyDisplayTo} {
		t.Run(string(key), func(t *testing.T) {
			var headers []string
			for _, text := range []string{"å", "Å", "A\u030a"} {
				encoded := mime.QEncoding.Encode("utf-8", text)
				header := "Subject: " + encoded
				switch key {
				case imap.SortKeyFrom:
					header = "From: " + text + "@example.test"
				case imap.SortKeyTo:
					header = "To: " + text + "@example.test"
				case imap.SortKeyCc:
					header = "Cc: " + text + "@example.test"
				case imap.SortKeyDisplayFrom:
					header = "From: " + encoded + " <same@example.test>"
				case imap.SortKeyDisplayTo:
					header = "To: " + encoded + " <same@example.test>"
				}
				headers = append(headers, header)
			}
			m := sortTestMailbox(t, headers)
			for _, reverse := range []bool{false, true} {
				if got := sortedUIDs(t, m, imap.SortKeySpec{Key: key, Reverse: reverse}); got != "1 2 3" {
					t.Fatalf("Unicode %s reverse=%v = %s; equivalent keys must retain mailbox order", key, reverse, got)
				}
			}
			if _, err := m.session.SetComparator(t.Context(), []string{"i;octet"}, nil); err != nil {
				t.Fatal(err)
			}
			if got := sortedUIDs(t, m, imap.SortKeySpec{Key: key}); got != "3 2 1" {
				t.Fatalf("octet %s = %s, want 3 2 1", key, got)
			}
		})
	}
}

func TestThreadActiveComparator(t *testing.T) {
	m := sortTestMailbox(t, []string{
		"Subject: =?utf-8?q?=C3=A5?=\r\nDate: Mon, 1 Jan 2024 10:00:00 +0000",
		"Subject: =?utf-8?q?=C3=85?=\r\nDate: Mon, 1 Jan 2024 11:00:00 +0000",
	})
	query, err := imapserver.NewSearchQuery(imap.SearchAll, nil)
	if err != nil {
		t.Fatal(err)
	}
	roots, err := m.Thread(t.Context(), query, imap.ThreadOrderedSubject, nil)
	if err != nil || fmt.Sprint(roots) != fmt.Sprint([]imap.ThreadNode{{Num: 1, Children: []imap.ThreadNode{{Num: 2}}}}) {
		t.Fatalf("Unicode-equivalent subjects did not group: %v, %v", roots, err)
	}
	if _, err := m.session.SetComparator(t.Context(), []string{"i;octet"}, nil); err != nil {
		t.Fatal(err)
	}
	roots, err = m.Thread(t.Context(), query, imap.ThreadOrderedSubject, nil)
	if err != nil || len(roots) != 2 {
		t.Fatalf("octet comparator did not distinguish subjects: %v, %v", roots, err)
	}
}

// RFC 5255 section 4.6: a key that fails conversion sorts after every key the
// comparator accepts, compared with i;octet among its peers.
func TestSortFailedConversionLast(t *testing.T) {
	m := sortTestMailbox(t, []string{"Subject: A\xff", "Subject: B", "Subject: A", "Subject: A\xfe"})
	for spec, want := range map[imap.SortKeySpec]string{
		{Key: imap.SortKeySubject}:                "3 2 4 1",
		{Key: imap.SortKeySubject, Reverse: true}: "1 4 2 3",
	} {
		if got := sortedUIDs(t, m, spec); got != want {
			t.Errorf("reverse=%v = %s, want %s", spec.Reverse, got, want)
		}
	}
	if _, err := m.session.SetComparator(t.Context(), []string{"i;octet"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := sortedUIDs(t, m, imap.SortKeySpec{Key: imap.SortKeySubject}); got != "3 4 1 2" {
		t.Errorf("i;octet = %s, want 3 4 1 2", got)
	}
}
