package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/maildir"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// Observe both sides of the backend boundary while the real framework and
// storage implementations handle the command and deliver its flag updates.
type fetchOriginBackend struct {
	imapserver.Backend
	legacy   bool
	command  chan imapserver.ChangeToken
	mutation chan imapserver.ChangeToken
}

func (b *fetchOriginBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &fetchOriginSession{Session: session, backend: b}, nil
}

type fetchOriginSession struct {
	imapserver.Session
	backend *fetchOriginBackend
}

func (s *fetchOriginSession) Select(ctx context.Context, mailbox string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	observer := &imapserver.Updater{PushFunc: func(batch *imapserver.UpdateBatch) error {
		s.backend.mutation <- batch.Origin
		return updater.Push(batch)
	}}
	result, err := s.Session.Select(ctx, mailbox, observer, options)
	if err != nil {
		return nil, err
	}
	result.Mailbox = &fetchOriginMailbox{SelectedMailbox: result.Mailbox, backend: s.backend}
	return result, nil
}

type fetchOriginMailbox struct {
	imapserver.SelectedMailbox
	backend *fetchOriginBackend
}

func (m *fetchOriginMailbox) Fetch(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, options *imapserver.FetchOptions) error {
	m.backend.command <- options.Origin
	if m.backend.legacy {
		// Simulate an existing backend that has not adopted the origin field.
		copy := *options
		copy.Origin = 0
		options = &copy
	}
	return m.SelectedMailbox.Fetch(ctx, writer, uids, options)
}

func TestFetchImplicitSeenOrigin(t *testing.T) {
	for _, storage := range []string{"memory", "maildir"} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy=%t", storage, legacy), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				var backend imapserver.Backend
				if storage == "memory" {
					backend = memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
				} else {
					b, err := maildir.Open(ctx, t.TempDir(), &maildir.Options{
						Accounts: []maildir.AccountOptions{{Username: "alice", Password: "secret"}},
					})
					if err != nil {
						t.Fatal(err)
					}
					defer b.Close(context.Background(), nil)
					backend = b
				}
				observed := &fetchOriginBackend{
					Backend: backend, legacy: legacy,
					command: make(chan imapserver.ChangeToken, 8), mutation: make(chan imapserver.ChangeToken, 8),
				}
				server := imapserver.New(observed, &imapserver.Options{AllowInsecureAuth: true})
				defer server.Close(context.Background(), nil)
				_, connection, reader := newRawSessionOn(t, ctx, server)
				defer connection.Close()
				if err := connection.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
					t.Fatal(err)
				}
				request := func(tag, command string) string {
					t.Helper()
					writeRawCommand(t, connection, tag+" "+command+"\r\n")
					lines, tagged := collectUntilTag(t, reader, tag+" ")
					if !strings.HasPrefix(tagged, tag+" OK ") {
						t.Fatalf("%s: %q", command, tagged)
					}
					return strings.Join(lines, "\n")
				}
				body := "Subject: origin\r\n\r\nbody\r\n"
				for i := range 3 {
					request(fmt.Sprintf("A%d", i), fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(body), body))
				}
				request("S", "SELECT INBOX")
				origins := make(map[imapserver.ChangeToken]bool)
				for i, tc := range []struct {
					command, selection string
					mutates, seen      bool
				}{
					{command: "FETCH 1 (BODY.PEEK[] FLAGS)"},
					{command: "FETCH 1 (BODY[] FLAGS)", mutates: true, seen: true},
					// With no requested FLAGS, the implicit change must still be
					// delivered as an update to the session that issued FETCH.
					{command: "UID FETCH 2 (BODY[])", mutates: true, seen: true},
					{command: "FETCH 1 (BODY[] FLAGS)", seen: true},
					{command: "FETCH 3 (BODY[] FLAGS)", selection: "EXAMINE INBOX"},
				} {
					if tc.selection != "" {
						request("E", tc.selection)
					}
					wire := request(fmt.Sprintf("F%d", i), tc.command)
					var origin imapserver.ChangeToken
					select {
					case origin = <-observed.command:
					case <-ctx.Done():
						t.Fatal("FETCH did not reach backend")
					}
					if origin == 0 || origins[origin] {
						t.Fatalf("%s: origin %d is zero or reused", tc.command, origin)
					}
					origins[origin] = true
					if tc.mutates {
						want := origin
						if legacy {
							want = 0
						}
						select {
						case got := <-observed.mutation:
							if got != want {
								t.Errorf("%s: update origin = %d, want %d", tc.command, got, want)
							}
						case <-ctx.Done():
							t.Fatal("FETCH did not publish its implicit Seen change")
						}
					}
					select {
					case got := <-observed.mutation:
						t.Fatalf("%s: unexpected update with origin %d", tc.command, got)
					default:
					}
					if got := strings.Contains(wire, "\\Seen"); got != tc.seen {
						t.Errorf("%s: Seen = %t, want %t: %s", tc.command, got, tc.seen, wire)
					}
				}
			})
		}
	}
}
