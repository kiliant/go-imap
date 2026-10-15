package imapserver_test

import (
	"context"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// TestLoopbackListSelectSpecialUse pins RFC 6154 section 2: LIST with the
// SPECIAL-USE selection option "MUST return only those mailboxes that have a
// special-use attribute set". The memory backend restricts the result itself;
// the framework drops the rest too, so a backend that ignores the option is
// still correct on the wire.
func TestLoopbackListSelectSpecialUse(t *testing.T) {
	for _, test := range []struct {
		name    string
		backend func(*memory.Backend) imapserver.Backend
	}{
		{"memory", func(b *memory.Backend) imapserver.Backend { return b }},
		{"backend ignoring the option", func(b *memory.Backend) imapserver.Backend { return &specialUseIgnoringBackend{inner: b} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			backend := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
			server := imapserver.New(test.backend(backend), &imapserver.Options{AllowInsecureAuth: true})
			client, _ := openLoopbackClient(t, ctx, server, &imapclient.Options{AllowInsecureAuth: true})
			if err := client.Login(ctx, "alice", "secret", nil); err != nil {
				t.Fatal(err)
			}
			if err := client.Create("Plain", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if err := client.Create("Arch", &imapclient.CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrArchive}}).Wait(ctx); err != nil {
				t.Fatal(err)
			}

			selected, err := client.List("", "*", &imapclient.ListOptions{
				SelectionOptions: []imapclient.ListSelectOption{imapclient.ListSelectSpecialUse},
			}).Wait(ctx)
			if err != nil {
				t.Fatalf("LIST (SPECIAL-USE): %v", err)
			}
			if len(selected) != 1 || selected[0].Mailbox != "Arch" || !imap.ContainsAttr(selected[0].Attrs, imap.MailboxAttrArchive) {
				t.Fatalf("LIST (SPECIAL-USE) = %v, want only Arch with \\Archive", listNames(selected))
			}

			all, err := client.List("", "*", nil).Wait(ctx)
			if err != nil {
				t.Fatalf("LIST: %v", err)
			}
			if len(all) != 3 {
				t.Fatalf("LIST = %v, want INBOX, Plain and Arch", listNames(all))
			}
		})
	}
}

func listNames(data []*imapclient.ListData) []string {
	names := make([]string, 0, len(data))
	for _, item := range data {
		names = append(names, item.Mailbox)
	}
	return names
}

// specialUseIgnoringBackend wraps the memory backend in a session that clears
// SelectSpecialUse before listing, as a backend that does not implement the
// option would. It keeps the backend's capability witnesses.
type specialUseIgnoringBackend struct{ inner *memory.Backend }

func (b *specialUseIgnoringBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.inner.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &specialUseIgnoringSession{Session: session}, nil
}

func (b *specialUseIgnoringBackend) SupportsCapability(ctx context.Context, name string, options *imapserver.CapabilitySupportOptions) bool {
	return b.inner.SupportsCapability(ctx, name, options)
}

type specialUseIgnoringSession struct{ imapserver.Session }

func (s *specialUseIgnoringSession) List(ctx context.Context, writer *imapserver.ListWriter, reference string, patterns []string, options *imapserver.ListOptions) error {
	if options != nil {
		ignored := *options
		ignored.SelectSpecialUse = false
		options = &ignored
	}
	return s.Session.List(ctx, writer, reference, patterns, options)
}
