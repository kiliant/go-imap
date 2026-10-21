package message_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver/message"
)

func TestMatchComparator(t *testing.T) {
	raw := "Subject: =?utf-8?q?=C3=85?=\r\nX-Test: =?utf-8?q?=C3=85?=\r\n" +
		"From: =?utf-8?q?=C3=85?= <sender@example.test>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nÅ body\r\n"
	m, err := message.Analyze(t.Context(), strings.NewReader(raw), int64(len(raw)), nil)
	if err != nil {
		t.Fatal(err)
	}
	criteria := []imap.SearchCriteria{
		imap.SearchString{Key: imap.SearchKeySubject, Value: "a\u030a"},
		imap.SearchString{Key: imap.SearchKeyFrom, Value: "a\u030a"},
		imap.SearchString{Key: imap.SearchKeyBody, Value: "a\u030a"},
		imap.SearchString{Key: imap.SearchKeyText, Value: "a\u030a"},
		imap.SearchHeaderField{Field: "X-Test", Value: "a\u030a"},
	}
	for _, criterion := range criteria {
		for _, comparator := range []string{"", "i;unicode-casemap", "i;octet"} {
			matched, err := message.Match(t.Context(), m, message.Metadata{}, imap.SearchAnd{criterion}, &message.MatchOptions{Comparator: comparator})
			if err != nil || matched != (comparator != "i;octet") {
				t.Errorf("%T comparator=%q: %v, %v", criterion, comparator, matched, err)
			}
		}
	}
	_, err = message.Match(t.Context(), m, message.Metadata{}, imap.SearchAll, &message.MatchOptions{Comparator: "i;future"})
	var protocol *imap.Error
	if !errors.As(err, &protocol) || protocol.Code != imap.CodeBadComparator {
		t.Fatalf("unsupported comparator = %v, want BADCOMPARATOR", err)
	}
}
