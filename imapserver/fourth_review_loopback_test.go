package imapserver_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// TestCondStoreEnablingCommands checks RFC 7162 section 3.1: STATUS
// (HIGHESTMODSEQ), and SEARCH-family commands using MODSEQ, enable CONDSTORE.
// The first one with a mailbox selected reports its HIGHESTMODSEQ, and later
// FETCH responses carry MODSEQ.
func TestCondStoreEnablingCommands(t *testing.T) {
	for _, command := range []string{
		"STATUS other (HIGHESTMODSEQ)",
		`LIST "" "other" RETURN (STATUS (HIGHESTMODSEQ))`,
		"ESEARCH IN (mailboxes other) MODSEQ 1",
	} {
		t.Run(strings.Fields(command)[0], func(t *testing.T) {
			client, reader := reviewRawSession(t, reviewBackend(t, "memory"), nil)
			reviewCommand(t, client, reader, "C", "CREATE other", "OK")
			raw := "Subject: x\r\n\r\nbody\r\n"
			for _, mailbox := range []string{"INBOX", "other"} {
				reviewCommand(t, client, reader, "P", fmt.Sprintf("APPEND %s {%d+}\r\n%s", mailbox, len(raw), raw), "OK")
			}
			reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
			if lines := reviewCommand(t, client, reader, "F", `STORE 1 +FLAGS (\Flagged)`, "OK"); strings.Contains(strings.Join(lines, ""), "MODSEQ") {
				t.Fatalf("MODSEQ before CONDSTORE was enabled: %q", lines)
			}
			lines := reviewCommand(t, client, reader, "Q", command, "OK")
			if !strings.Contains(strings.Join(lines, ""), "* OK [HIGHESTMODSEQ ") {
				t.Errorf("%s did not report the selected mailbox's HIGHESTMODSEQ: %q", command, lines)
			}
			if lines := reviewCommand(t, client, reader, "F", `STORE 1 -FLAGS (\Flagged)`, "OK"); !strings.Contains(strings.Join(lines, ""), "MODSEQ (") {
				t.Errorf("FETCH after %s lacks MODSEQ: %q", command, lines)
			}
		})
	}
}

type countingListBackend struct {
	*memory.Backend
	lists *int
}

// countingListSession keeps memory's MULTISEARCH, counts LIST calls and
// presents a shared namespace beside an empty personal prefix.
type countingListSession struct {
	imapserver.Session
	imapserver.MultiSearchSession
	lists *int
}

func (b *countingListBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &countingListSession{Session: s, MultiSearchSession: s.(imapserver.MultiSearchSession), lists: b.lists}, nil
}

func (s *countingListSession) List(ctx context.Context, writer *imapserver.ListWriter, reference string, patterns []string, options *imapserver.ListOptions) error {
	if slices.Equal(patterns, []string{"*"}) {
		*s.lists++
	}
	return s.Session.List(ctx, writer, reference, patterns, options)
}

func (s *countingListSession) Namespace(context.Context, *imapserver.NamespaceOptions) (*imap.NamespaceData, error) {
	return &imap.NamespaceData{
		Personal: []imap.NamespaceDescriptor{{Prefix: "", Delimiter: '/'}},
		Shared:   []imap.NamespaceDescriptor{{Prefix: "#shared/", Delimiter: '/'}},
	}, nil
}

// TestMultiSearchSourceExpansion checks that a shared namespace is not part of
// "personal", and that expansion lists the hierarchy once per command.
func TestMultiSearchSourceExpansion(t *testing.T) {
	lists := 0
	b := &countingListBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), lists: &lists}
	client, reader := reviewRawSession(t, b, nil)
	for _, name := range []string{"INBOX", "a", "a/b", "#shared/team"} {
		if name != "INBOX" {
			reviewCommand(t, client, reader, "C", fmt.Sprintf("CREATE %q", name), "OK")
		}
		raw := "Subject: x\r\n\r\nbody\r\n"
		reviewCommand(t, client, reader, "P", fmt.Sprintf("APPEND %q {%d+}\r\n%s", name, len(raw), raw), "OK")
	}
	lists = 0
	lines := reviewCommand(t, client, reader, "Q", "ESEARCH IN (personal inboxes subtree a subtree-one a) ALL", "OK")
	got := make([]string, 0, len(lines))
	for name := range esearchLines(lines) {
		got = append(got, name)
	}
	slices.Sort(got)
	if want := []string{"INBOX", "a", "a/b"}; !slices.Equal(got, want) {
		t.Errorf("personal sources = %q, want %q; wire %q", got, want, lines)
	}
	if lists != 1 {
		t.Errorf("source expansion listed the hierarchy %d times, want once", lists)
	}
}

// TestMultiSearchExplicitSelectedSource checks that IN (selected) behaves like
// an absent source list (RFC 7377 section 2.2): "$" and sequence numbers
// resolve against the selection, while a "mailboxes" source is still refused.
func TestMultiSearchExplicitSelectedSource(t *testing.T) {
	client, reader := reviewRawSession(t, reviewBackend(t, "memory"), nil)
	for i := range 3 {
		raw := fmt.Sprintf("Subject: %d\r\n\r\nbody\r\n", i)
		reviewCommand(t, client, reader, "P", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
	}
	reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
	reviewCommand(t, client, reader, "V", `SEARCH RETURN (SAVE) SUBJECT "1"`, "OK")
	for command, want := range map[string]string{
		"ESEARCH IN (selected) $":     "UID ALL 2",
		"ESEARCH IN (selected) NOT $": "UID ALL 1,3",
		"ESEARCH IN (selected) 3":     "UID ALL 3",
	} {
		if line := esearchLines(reviewCommand(t, client, reader, "Q", command, "OK"))["INBOX"]; !strings.HasSuffix(line, want) {
			t.Errorf("%s = %q, want %s", command, line, want)
		}
	}
	for _, command := range []string{"ESEARCH IN (mailboxes INBOX) $", "ESEARCH IN (selected mailboxes INBOX) 1"} {
		reviewCommand(t, client, reader, "Q", command, "BAD")
	}
}
