package memory

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/backendtest"
)

func TestBackendConformance(t *testing.T) {
	backendtest.Run(t, &backendtest.Harness{New: func() *backendtest.Instance {
		backend := New(&Options{Users: map[string]string{"alice": "secret"}})
		return &backendtest.Instance{
			Backend: backend,
			Credentials: imapserver.Credentials{
				Mechanism: "PLAIN",
				Username:  "alice",
				Password:  "secret",
			},
			Controls: backendtest.Controls{
				ForceUIDValidityChange: func(ctx context.Context, mailbox string) error {
					return backend.forceUIDValidityChange(ctx, "alice", mailbox)
				},
				ForceSelectFailure: func(ctx context.Context, mailbox string, enabled bool) error {
					return backend.forceSelectFailure(ctx, "alice", mailbox, enabled)
				},
			},
		}
	}})
}

func TestMailboxPatternWildcards(t *testing.T) {
	for _, test := range []struct {
		name, pattern string
		want          bool
	}{
		{name: "Archive/2026/August", pattern: "*", want: true},
		{name: "Archive/2026/August", pattern: "Archive/%", want: false},
		{name: "Archive/2026", pattern: "Archive/%", want: true},
		{name: "Archive/2026/August", pattern: "Archive/%/August", want: true},
		{name: "Archive/2026/August", pattern: "Archive/*/August", want: true},
		{name: "Börse/2026", pattern: "B%/2026", want: true},
	} {
		if got := matchesMailboxPattern(test.name, test.pattern); got != test.want {
			t.Errorf("matchesMailboxPattern(%q, %q) = %v, want %v", test.name, test.pattern, got, test.want)
		}
	}
}

func TestAuthenticateRejectsNonPasswordMechanism(t *testing.T) {
	backend := New(&Options{Users: map[string]string{"alice": ""}})
	_, err := backend.Authenticate(context.Background(), &imapserver.ConnInfo{}, &imapserver.Credentials{
		Mechanism: "OAUTHBEARER",
		Username:  "alice",
		Token:     "token",
	}, nil)
	if err == nil {
		t.Fatal("token mechanism authenticated against an empty plaintext password")
	}
}

func TestFetchSeenAndLegacySections(t *testing.T) {
	backend := New(&Options{Users: map[string]string{"alice": "secret"}})
	session, err := backend.Authenticate(context.Background(), &imapserver.ConnInfo{}, &imapserver.Credentials{
		Mechanism: "PLAIN",
		Username:  "alice",
		Password:  "secret",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("From: sender@example.com\r\nSubject: fetch\r\n\r\nbody\r\n")
	appended, err := session.Append(context.Background(), "INBOX", bytes.NewReader(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	status, err := session.Status(context.Background(), "INBOX", &imapserver.StatusOptions{Items: []imap.StatusItem{imap.StatusItemMessages}})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Values) != 1 || status.Values[imap.StatusItemMessages] != uint64(1) {
		t.Fatalf("requested STATUS projection = %#v", status.Values)
	}
	var batches []*imapserver.UpdateBatch
	selected, err := session.Select(context.Background(), "INBOX", &imapserver.Updater{PushFunc: func(batch *imapserver.UpdateBatch) error {
		batches = append(batches, batch)
		return nil
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var fetched *imap.FetchMessageData
	err = selected.Mailbox.Fetch(context.Background(), &imapserver.FetchWriter{WriteFunc: func(_ context.Context, data *imap.FetchMessageData) error {
		fetched = data
		return nil
	}}, imap.UIDSetNum(appended.UID), &imapserver.FetchOptions{Items: []imap.FetchItem{
		imap.FetchItemFlags,
		&imap.FetchItemBodySection{},
		imap.FetchItemRFC822Header,
		imap.FetchItemRFC822Text,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if fetched == nil {
		t.Fatal("FETCH returned no message data")
	}
	flagValues := fetched.Items[imap.FetchDataKey(imap.FetchItemFlags)]
	if len(flagValues) != 1 || !imap.ContainsFlag([]imap.Flag(flagValues[0].(imap.FetchDataFlags)), imap.FlagSeen) {
		t.Fatalf("FETCH FLAGS after non-PEEK body read = %#v", flagValues)
	}
	if len(batches) != 1 {
		t.Fatalf("seen transition update batches = %d, want 1", len(batches))
	}
	assertLiteral(t, fetched.Items["BODY[]"], raw)
	assertLiteral(t, fetched.Items[imap.FetchDataKey(imap.FetchItemRFC822Header)], []byte("From: sender@example.com\r\nSubject: fetch\r\n\r\n"))
	assertLiteral(t, fetched.Items[imap.FetchDataKey(imap.FetchItemRFC822Text)], []byte("body\r\n"))
	if err := selected.Mailbox.Unselect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBodySectionKeyHeaderFields(t *testing.T) {
	item := &imap.FetchItemBodySection{
		Part:         []int{2},
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: []string{"Subject", "From"},
		Partial:      &imap.SectionPartial{Offset: 12, Size: 20},
		Peek:         true,
	}
	if got, want := bodySectionKey(item), "BODY[2.HEADER.FIELDS (Subject From)]<12>"; got != want {
		t.Fatalf("bodySectionKey = %q, want %q", got, want)
	}
}

func assertLiteral(t *testing.T, values []imap.FetchData, want []byte) {
	t.Helper()
	if len(values) != 1 {
		t.Fatalf("literal values = %#v", values)
	}
	var reader io.Reader
	switch value := values[0].(type) {
	case *imap.FetchDataLiteral:
		reader = value.Literal
	case *imap.FetchDataBodySection:
		reader = value.Literal
	default:
		t.Fatalf("literal value has type %T", values[0])
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("literal = %q, want %q", got, want)
	}
}

func (b *Backend) forceUIDValidityChange(ctx context.Context, username, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.RLock()
	account := b.accounts[username]
	b.mu.RUnlock()
	if account == nil {
		return authenticationError()
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	mailbox := account.mailboxes[mailboxKey(name)]
	if mailbox == nil {
		return nonexistentMailbox(name)
	}
	mailbox.uidValidity = account.nextUIDValidity
	account.nextUIDValidity++
	mailbox.revision++
	return nil
}

func (b *Backend) forceSelectFailure(ctx context.Context, username, name string, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.RLock()
	account := b.accounts[username]
	b.mu.RUnlock()
	if account == nil {
		return authenticationError()
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	if account.mailboxes[mailboxKey(name)] == nil {
		return nonexistentMailbox(name)
	}
	account.selectFailure[mailboxKey(name)] = enabled
	return nil
}
