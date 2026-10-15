package memory

import (
	"context"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// TestListSelectSpecialUse checks the backend itself, beneath the framework's
// own filter: with SelectSpecialUse, List returns only mailboxes that have a
// use attribute (RFC 6154 section 2).
func TestListSelectSpecialUse(t *testing.T) {
	ctx := context.Background()
	backend := New(&Options{Users: map[string]string{"alice": "secret"}})
	session, err := backend.Authenticate(ctx, &imapserver.ConnInfo{}, &imapserver.Credentials{
		Mechanism: "PLAIN",
		Username:  "alice",
		Password:  "secret",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Create(ctx, "Plain", nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Create(ctx, "Arch", &imapserver.CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrArchive}}); err != nil {
		t.Fatal(err)
	}
	var names []string
	writer := &imapserver.ListWriter{WriteFunc: func(_ context.Context, data *imap.ListData) error {
		names = append(names, data.Mailbox)
		return nil
	}}
	if err := session.List(ctx, writer, "", []string{"*"}, &imapserver.ListOptions{SelectSpecialUse: true}); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "Arch" {
		t.Fatalf("List with SelectSpecialUse = %v, want only Arch", names)
	}
}
