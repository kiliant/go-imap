package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/kiliant/go-imap/imapserver"
)

func TestCatenateURLContent(t *testing.T) {
	s, _ := urlAuthSessions(t)
	selected, err := s.Select(t.Context(), "INBOX", &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { return nil }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer selected.Mailbox.Unselect(context.Background(), nil)
	validity := selected.Snapshot.Status.UIDValidity
	for _, tc := range []struct{ url, want string }{
		{"/INBOX/;UID=1", "Subject: alice\r\n\r\nalice body"},
		{fmt.Sprintf("/INBOX;UIDVALIDITY=%d/;UID=1/;SECTION=TEXT", validity), "alice body"},
		{"imap://alice@localhost/INBOX/;UID=1/;SECTION=TEXT/;PARTIAL=2.4", "ice "},
		{"imap://localhost/INBOX/;UID=1/;SECTION=HEADER", "Subject: alice\r\n\r\n"},
		{"imap://alice@elsewhere.example:1143/INBOX/;UID=1/;SECTION=TEXT", "alice body"},
		{"imap://elsewhere.example:993/INBOX/;UID=1/;SECTION=TEXT", "alice body"},
		{"/INBOX/;UID=1/;SECTION=HEADER.FIELDS%20(SUBJECT)", "Subject: alice\r\n\r\n"},
		{"/INBOX/;UID=1/;SECTION=TEXT/;PARTIAL=6", "body"},
		{"/INBOX/;UID=1/;SECTION=TEXT/;PARTIAL=99.4", ""},
		{";UID=1/;SECTION=TEXT", "alice body"},
		{fmt.Sprintf(";UIDVALIDITY=%d/;UID=1/;SECTION=TEXT", validity), "alice body"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			r, err := s.ResolveCatenateURL(t.Context(), tc.url, nil)
			if err != nil || r == nil {
				t.Fatalf("resolve: %v", err)
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err != nil || string(got) != tc.want {
				t.Fatalf("content=%q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, raw := range []string{
		"/INBOX;UIDVALIDITY=4294967295/;UID=1", "/INBOX/;UID=999",
		"/INBOX/;UID=1/;SECTION=2", "/INBOX/;UID=1/;SECTION=BAD",
		"/INBOX/;UID=1/;SECTION=TEXT/;PARTIAL=2.0", "/INBOX/;UID=1/;PARTIAL=-1.4",
		"/INBOX/;UID=1/;PARTIAL=0.4294967296", "/INBOX/;UID=1/;PARTIAL=0.4/junk",
		"/INBOX/;UID=1/;SECTION=TEXT<0.4>", "imap://bob@localhost/INBOX/;UID=1",
		"imap://alice:secret@localhost/INBOX/;UID=1", "https://alice@localhost/INBOX/;UID=1",
		"imap://bob@elsewhere.example:1143/INBOX/;UID=1",
		"/INBOX/;UID=1;URLAUTH=anonymous", "/INBOX/;UID=1?x", "/INBOX/;UID=1#x",
	} {
		if r, err := s.ResolveCatenateURL(t.Context(), raw, nil); r != nil {
			r.Close()
			t.Errorf("invalid URL resolved: %s (%v)", raw, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	r, err := s.ResolveCatenateURL(ctx, "/INBOX/;UID=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	defer r.Close()
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancellation: %v", err)
	}
	if _, err := s.ResolveCatenateURL(ctx, "/INBOX/;UID=1", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func FuzzCatenateURL(f *testing.F) {
	for _, raw := range []string{"/INBOX/;UID=1", ";UID=1/;SECTION=TEXT", "imap://alice@localhost/INBOX/;UID=1/;PARTIAL=0.4"} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 1<<16 {
			t.Skip()
		}
		u, ok := parseCatenateURL(raw, "alice", "INBOX")
		if ok && (u.uid == 0 || u.owner != "alice" || u.mailbox == "" || u.section == nil || !u.section.Peek) {
			t.Fatalf("invalid parsed URL: %#v", u)
		}
	})
}

func TestCatenateURLMIMESection(t *testing.T) {
	s, _ := urlAuthSessions(t)
	raw := "Subject: MIME\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nfirst\r\n--x\r\nContent-Type: text/plain\r\n\r\nsecond\r\n--x--\r\n"
	data, err := s.Append(t.Context(), "INBOX", strings.NewReader(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ResolveCatenateURL(t.Context(), fmt.Sprintf("/INBOX/;UID=%d/;SECTION=2/;PARTIAL=1.3", data.UID), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "eco" {
		t.Fatalf("part = %q, %v", got, err)
	}
}
