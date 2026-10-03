package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

func TestBuildAndWriteFolderTree(t *testing.T) {
	mailboxes := []*imapclient.ListData{
		{Mailbox: "INBOX", Delimiter: '/', Attrs: nil},
		{Mailbox: "Archive", Delimiter: '/', Attrs: []imap.MailboxAttr{imap.MailboxAttrArchive}},
		{Mailbox: "Archive/2024", Delimiter: '/'},
		{Mailbox: "Archive/2025", Delimiter: '/'},
		{Mailbox: "Lists", Delimiter: '/', Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}},
		{Mailbox: "Lists/Go", Delimiter: '/'},
	}

	var buf bytes.Buffer
	if err := writeFolderTree(&buf, buildFolderTree(mailboxes)); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	want := strings.Join([]string{
		"├── Archive  [\\Archive]",
		"│   ├── 2024",
		"│   └── 2025",
		"├── INBOX",
		"└── Lists  [noselect]",
		"    └── Go",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("tree mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildFolderTreeNilDelimiter(t *testing.T) {
	mailboxes := []*imapclient.ListData{
		{Mailbox: "INBOX", Delimiter: 0},
		{Mailbox: "Sent Items", Delimiter: 0, Attrs: []imap.MailboxAttr{imap.MailboxAttrSent}},
	}
	var buf bytes.Buffer
	if err := writeFolderTree(&buf, buildFolderTree(mailboxes)); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	want := strings.Join([]string{
		"├── INBOX",
		"└── Sent Items  [\\Sent]",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("tree mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildFolderTreeSkipsEmpty(t *testing.T) {
	root := buildFolderTree([]*imapclient.ListData{
		nil,
		{Mailbox: ""},
		{Mailbox: "INBOX", Delimiter: '/'},
	})
	if len(root.children) != 1 {
		t.Fatalf("children = %d, want 1", len(root.children))
	}
	if root.children["INBOX"] == nil {
		t.Fatal("missing INBOX")
	}
}

func TestMailboxSegments(t *testing.T) {
	tests := []struct {
		name  string
		delim rune
		want  []string
	}{
		{name: "INBOX", delim: '/', want: []string{"INBOX"}},
		{name: "a/b/c", delim: '/', want: []string{"a", "b", "c"}},
		{name: "a.b", delim: '.', want: []string{"a", "b"}},
		{name: "a/b", delim: 0, want: []string{"a/b"}},
	}
	for _, tt := range tests {
		got := mailboxSegments(tt.name, tt.delim)
		if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
			t.Fatalf("mailboxSegments(%q, %q) = %#v, want %#v", tt.name, string(tt.delim), got, tt.want)
		}
	}
}
