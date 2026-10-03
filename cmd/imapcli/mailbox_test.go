package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

func TestWriteMailboxStatus(t *testing.T) {
	var buf bytes.Buffer
	err := writeMailboxStatus(&buf, &imapclient.MailboxStatus{MailboxStatus: imap.MailboxStatus{
		Mailbox:     "INBOX",
		ReadOnly:    true,
		NumMessages: 3,
		NumRecent:   1,
		Unseen:      2,
		UIDNext:     10,
		UIDValidity: 99,
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"INBOX", "read-only", "Messages: 3", "UIDNEXT: 10"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status missing %q:\n%s", want, got)
		}
	}
}

func TestWriteMessageSummary(t *testing.T) {
	var buf bytes.Buffer
	data := &imap.FetchMessageData{
		SeqNum: 1,
		Items: map[imap.FetchDataKey][]imap.FetchData{
			"UID":   {imap.FetchDataUID(42)},
			"FLAGS": {imap.FetchDataFlags{imap.FlagSeen}},
			"ENVELOPE": {&imap.FetchDataEnvelope{Envelope: &imap.Envelope{
				Subject: "Hello",
				Date:    time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
				From:    []imap.Address{{Name: "Alice", Mailbox: "alice", Host: "example.com"}},
			}}},
		},
	}
	if err := writeMessageSummary(&buf, data); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	for _, want := range []string{"42", "Alice", "Hello", "\\Seen"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
}

func TestWriteMessageBody(t *testing.T) {
	var buf bytes.Buffer
	data := &imap.FetchMessageData{
		Items: map[imap.FetchDataKey][]imap.FetchData{
			"BODY[]": {&imap.FetchDataBodySection{
				Literal: strings.NewReader("From: a\r\n\r\nhello\r\n"),
			}},
		},
	}
	if err := writeMessageBody(&buf, data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "hello") {
		t.Fatalf("body = %q", buf.String())
	}
}

func TestMessageWindow(t *testing.T) {
	tests := []struct {
		n               uint32
		limitU, offsetU uint
		start, stop     imap.SeqNum
		ok              bool
	}{
		{n: 0, limitU: 20, ok: false},
		{n: 10, limitU: 20, start: 1, stop: 10, ok: true},
		{n: 100, limitU: 20, start: 81, stop: 100, ok: true},
		{n: 100, limitU: 0, start: 1, stop: 100, ok: true},
		{n: 100, limitU: 20, offsetU: 20, start: 61, stop: 80, ok: true},
		{n: 100, limitU: 20, offsetU: 100, ok: false},
		{n: 5, limitU: 0, offsetU: 2, start: 1, stop: 3, ok: true},
	}
	for _, tt := range tests {
		start, stop, ok := messageWindow(tt.n, tt.limitU, tt.offsetU)
		if ok != tt.ok || start != tt.start || stop != tt.stop {
			t.Fatalf("messageWindow(%d,%d,%d) = %d,%d,%v want %d,%d,%v",
				tt.n, tt.limitU, tt.offsetU, start, stop, ok, tt.start, tt.stop, tt.ok)
		}
	}
}

func TestFormatAddressesAndTruncate(t *testing.T) {
	if got := formatAddresses(nil); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := formatAddresses([]imap.Address{{Mailbox: "a", Host: "b.com"}}); got != "a@b.com" {
		t.Fatalf("addr = %q", got)
	}
	if got := truncate("abcdef", 5); got != "ab..." {
		t.Fatalf("truncate = %q", got)
	}
	// A byte-based cut would split the two-byte "ü" and emit invalid UTF-8.
	if got := truncate("Jürgen Müller", 5); got != "Jü..." || !utf8.ValidString(got) {
		t.Fatalf("truncate multi-byte = %q", got)
	}
}
