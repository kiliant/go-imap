package imapserver_test

import (
	"bufio"
	"context"
	"fmt"
	"github.com/kiliant/go-imap"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/maildir"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func reviewBackend(t *testing.T, kind string) imapserver.Backend {
	t.Helper()
	if kind == "memory" {
		return memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	}
	b, err := maildir.Open(t.Context(), t.TempDir(), &maildir.Options{Accounts: []maildir.AccountOptions{{Username: "alice", Password: "secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background(), nil) })
	return b
}

func reviewRawSession(t *testing.T, backend imapserver.Backend, options *imapserver.Options) (net.Conn, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	if options == nil {
		options = &imapserver.Options{AllowInsecureAuth: true}
	}
	server := imapserver.New(backend, options)
	t.Cleanup(func() { _ = server.Close(context.Background(), nil) })
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetDeadline(time.Now().Add(20 * time.Second))
	go func() { _ = server.ServeConn(ctx, peer, nil) }()
	reader := bufio.NewReader(client)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	reviewCommand(t, client, reader, "L", "LOGIN alice secret", "OK")
	return client, reader
}

func reviewCommand(t *testing.T, client net.Conn, reader *bufio.Reader, tag, command, status string) []string {
	t.Helper()
	writeRawCommand(t, client, tag+" "+command+"\r\n")
	lines, tagged := collectUntilTag(t, reader, tag+" ")
	if !strings.HasPrefix(tagged, tag+" "+status) {
		t.Fatalf("%s: %s", command, tagged)
	}
	return append(lines, tagged)
}

func TestReviewSearchSavedResult(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			client, reader := reviewRawSession(t, reviewBackend(t, kind), nil)
			for i, from := range []string{"alice", "carol"} {
				raw := "From: " + from + "@example.test\r\nSubject: " + from + "\r\n\r\nhello\r\n"
				reviewCommand(t, client, reader, fmt.Sprintf("A%d", i), fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
			}
			reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
			// No saved result matches nothing, rather than ALL or an invalid key.
			lines := reviewCommand(t, client, reader, "Q", "SEARCH $", "OK")
			if !slices.Contains(lines, "* SEARCH") {
				t.Fatalf("unsaved $ = %q", lines)
			}
			reviewCommand(t, client, reader, "V", "SEARCH RETURN (SAVE) FROM alice", "OK")
			for _, tc := range []struct{ command, want string }{
				{"SEARCH $", "* SEARCH 1"},
				{"UID SEARCH NOT $", "* SEARCH 2"},
				{"SEARCH OR $ FROM carol", "* SEARCH 1 2"},
				{"SEARCH FUZZY $", "* SEARCH 1"},
				{"UID FETCH $ (UID)", "* 1 FETCH (UID 1)"},
				{"ESEARCH $", "ALL 1"},
				{"SEARCH RETURN (ALL) $", "ALL 1"},
			} {
				if kind == "maildir" && (strings.Contains(tc.command, "FUZZY") || strings.HasPrefix(tc.command, "ESEARCH")) {
					continue
				}
				lines := reviewCommand(t, client, reader, "Q", tc.command, "OK")
				if !strings.Contains(strings.Join(lines, ""), tc.want) {
					t.Errorf("%s: %q, want %s", tc.command, lines, tc.want)
				}
			}
			if kind == "memory" {
				for _, tc := range []struct{ command, want string }{
					{"SORT (SUBJECT) UTF-8 $", "* SORT 1"},
					{"THREAD ORDEREDSUBJECT UTF-8 $", "* THREAD (1)"},
				} {
					lines := reviewCommand(t, client, reader, "Q", tc.command, "OK")
					if !strings.Contains(strings.Join(lines, ""), tc.want) {
						t.Errorf("%s = %q", tc.command, lines)
					}
				}
				for _, criterion := range []string{"$", "NOT $", "OR $ ALL", "FUZZY $"} {
					reviewCommand(t, client, reader, "Q", "ESEARCH IN (mailboxes INBOX) "+criterion, "BAD")
				}
			}
			reviewCommand(t, client, reader, "V", "SEARCH RETURN (SAVE) FROM nobody", "OK")
			lines = reviewCommand(t, client, reader, "Q", "SEARCH NOT $", "OK")
			if !strings.Contains(strings.Join(lines, ""), "* SEARCH 1 2") {
				t.Fatalf("empty saved set NOT = %q", lines)
			}
		})
	}
}

func TestReviewStatusAndSelection(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		for _, rev2 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rev2=%t", kind, rev2), func(t *testing.T) {
				client, reader := reviewRawSession(t, reviewBackend(t, kind), nil)
				if rev2 {
					reviewCommand(t, client, reader, "E", "ENABLE IMAP4rev2", "OK")
				}
				created := reviewCommand(t, client, reader, "C", "CREATE Other", "OK")
				var id string
				if kind == "memory" {
					wire := strings.Join(created, "")
					start := strings.Index(wire, "[MAILBOXID (")
					if start < 0 {
						t.Fatalf("CREATE omitted MAILBOXID: %q", created)
					}
					id = wire[start : strings.Index(wire[start:], "]")+start+1]
					status := reviewCommand(t, client, reader, "T", "STATUS Other (MAILBOXID)", "OK")
					if !strings.Contains(strings.Join(status, ""), strings.TrimSuffix(strings.TrimPrefix(id, "["), "]")) {
						t.Fatalf("ID mismatch: %q vs %s", status, id)
					}
				}
				first := reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
				if strings.Contains(strings.Join(first, ""), "[CLOSED]") {
					t.Fatal("initial SELECT sent CLOSED")
				}
				second := reviewCommand(t, client, reader, "S", "EXAMINE Other", "OK")
				wire := strings.Join(second, "")
				if !strings.Contains(second[0], "[CLOSED]") {
					t.Fatalf("boundary not first: %q", second)
				}
				if kind == "memory" && !strings.Contains(wire, id) {
					t.Fatalf("EXAMINE omitted matching ID: %q", second)
				}
				failed := reviewCommand(t, client, reader, "S", "SELECT Missing", "NO")
				if !strings.Contains(strings.Join(failed, ""), "[CLOSED]") {
					t.Fatalf("failed reselect omitted boundary: %q", failed)
				}
				reviewCommand(t, client, reader, "F", "FETCH 1 (UID)", "BAD")
				for _, closeCommand := range []string{"CLOSE", "UNSELECT"} {
					reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
					lines := reviewCommand(t, client, reader, "X", closeCommand, "OK")
					if strings.Contains(strings.Join(lines, ""), "[CLOSED]") {
						t.Fatalf("%s sent CLOSED", closeCommand)
					}
				}
				reviewCommand(t, client, reader, "T", "STATUS INBOX (FOO)", "BAD")
				reviewCommand(t, client, reader, "T", `LIST "" "*" RETURN (STATUS (FOO))`, "BAD")
				if kind == "maildir" {
					reviewCommand(t, client, reader, "T", "STATUS INBOX (APPENDLIMIT)", "BAD")
				}
				if rev2 || kind == "memory" {
					raw := "Subject: deleted\r\n\r\n" + strings.Repeat("x", 1200)
					reviewCommand(t, client, reader, "A", fmt.Sprintf("APPEND INBOX (\\Deleted) {%d+}\r\n%s", len(raw), raw), "OK")
					lines := reviewCommand(t, client, reader, "T", "STATUS INBOX (DELETED)", "OK")
					if !strings.Contains(strings.Join(lines, ""), "DELETED 1") {
						t.Fatalf("DELETED = %q", lines)
					}
					if kind == "memory" {
						lines = reviewCommand(t, client, reader, "T", "STATUS INBOX (DELETED-STORAGE)", "OK")
						if !strings.Contains(strings.Join(lines, ""), fmt.Sprintf("DELETED-STORAGE %d)", len(raw))) {
							t.Fatalf("DELETED-STORAGE = %q", lines)
						}
					}
				}
			})
		}
	}
}

type reviewWitnessBackend struct {
	*memory.Backend
	withheld map[string]bool
	filtered map[string]bool
}

func (b *reviewWitnessBackend) SupportsCapability(ctx context.Context, name string, options *imapserver.CapabilitySupportOptions) bool {
	return !b.withheld[name] && b.Backend.SupportsCapability(ctx, name, options)
}
func (b *reviewWitnessBackend) AllowCapability(_ context.Context, name string, _ *imapserver.CapabilityFilterOptions) bool {
	return !b.filtered[name]
}

func TestReviewCapabilityDependencies(t *testing.T) {
	for _, tc := range []struct {
		name               string
		withheld, filtered map[string]bool
		present, absent    []string
	}{
		{name: "all", present: []string{"QUOTA", "QUOTASET", "QUOTA=RES-STORAGE", "QUOTA=RES-MESSAGE", "ACL", "RIGHTS=texk", "IMAP4REV2"}},
		{name: "rev2-missing-deleted", withheld: map[string]bool{"STATUS-DELETED": true}, absent: []string{"IMAP4REV2", "STATUS-DELETED"}},
		{name: "legacy-quota", withheld: map[string]bool{"QUOTA=RES-STORAGE": true, "QUOTA=RES-MESSAGE": true}, present: []string{"QUOTA"}, absent: []string{"QUOTASET", "QUOTA=RES-STORAGE", "QUOTA=RES-MESSAGE"}},
		{name: "resources-filtered", filtered: map[string]bool{"QUOTA=RES-STORAGE": true, "QUOTA=RES-MESSAGE": true}, present: []string{"QUOTA"}, absent: []string{"QUOTASET", "QUOTA=RES-STORAGE", "QUOTA=RES-MESSAGE"}},
		{name: "one-resource", filtered: map[string]bool{"QUOTA=RES-STORAGE": true}, present: []string{"QUOTA", "QUOTASET", "QUOTA=RES-MESSAGE"}, absent: []string{"QUOTA=RES-STORAGE"}},
		{name: "quota-filtered", filtered: map[string]bool{"QUOTA": true}, absent: []string{"QUOTA", "QUOTASET", "QUOTA=RES-STORAGE", "QUOTA=RES-MESSAGE"}},
		{name: "acl-filtered", filtered: map[string]bool{"ACL": true}, absent: []string{"ACL", "RIGHTS=texk"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &reviewWitnessBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), withheld: tc.withheld, filtered: tc.filtered}
			client, reader := reviewRawSession(t, b, nil)
			lines := reviewCommand(t, client, reader, "C", "CAPABILITY", "OK")
			caps := strings.Fields(strings.Join(lines, " "))
			for _, name := range tc.present {
				if !slices.Contains(caps, name) {
					t.Errorf("missing %s: %q", name, lines)
				}
			}
			for _, name := range tc.absent {
				if slices.Contains(caps, name) {
					t.Errorf("unexpected %s: %q", name, lines)
				}
			}
			if tc.withheld["STATUS-DELETED"] {
				reviewCommand(t, client, reader, "E", "ENABLE IMAP4rev2", "OK")
			}
			if tc.filtered["QUOTA=RES-STORAGE"] {
				reviewCommand(t, client, reader, "T", "STATUS INBOX (DELETED-STORAGE)", "BAD")
			}
		})
	}
}

func TestReviewClientErrorText(t *testing.T) {
	b := &reviewWitnessBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), filtered: map[string]bool{"BINARY": true, "CONDSTORE": true, "SEARCH=FUZZY": true}}
	client, reader := reviewRawSession(t, b, &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{MaxLiteralBytes: 20}})
	reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
	for command, want := range map[string]string{
		"FETCH 1 (BINARY.PEEK[1])": "BINARY.PEEK[...]",
		"FETCH 1 (BINARY.SIZE[1])": "BINARY.SIZE[...]",
		"SEARCH MODSEQ 1":          "MODSEQ",
		"SEARCH FUZZY ALL":         "FUZZY",
		"APPEND INBOX {21}":        "size limit",
	} {
		lines := reviewCommand(t, client, reader, "Q", command, "NO")
		wire := strings.Join(lines, "")
		if !strings.Contains(wire, want) || strings.Contains(wire, "imap.") || strings.Contains(wire, "resolved content") {
			t.Errorf("%s = %q", command, wire)
		}
	}
}

type reviewStatusBackend struct {
	*memory.Backend
	status func(context.Context, imapserver.Session, string, *imapserver.StatusOptions) (*imap.StatusData, error)
}

func (b *reviewStatusBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &reviewStatusSession{Session: session, status: b.status}, nil
}

type reviewStatusSession struct {
	imapserver.Session
	status func(context.Context, imapserver.Session, string, *imapserver.StatusOptions) (*imap.StatusData, error)
}

func (s *reviewStatusSession) Status(ctx context.Context, mailbox string, options *imapserver.StatusOptions) (*imap.StatusData, error) {
	return s.status(ctx, s.Session, mailbox, options)
}

func TestReviewStatusGatePrecedesBackend(t *testing.T) {
	var calls atomic.Int32
	b := &reviewStatusBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), status: func(ctx context.Context, session imapserver.Session, mailbox string, options *imapserver.StatusOptions) (*imap.StatusData, error) {
		calls.Add(1)
		return session.Status(ctx, mailbox, options)
	}}
	client, reader := reviewRawSession(t, b, nil)
	for _, command := range []string{"STATUS INBOX (FOO)", `LIST "" "*" RETURN (STATUS (FOO))`} {
		reviewCommand(t, client, reader, "Q", command, "BAD")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("invalid items reached Status %d times", got)
	}
	reviewCommand(t, client, reader, "Q", "STATUS INBOX (MESSAGES)", "OK")
	if got := calls.Load(); got != 1 {
		t.Fatalf("baseline Status calls = %d", got)
	}
}

func TestReviewObjectIDLookupFailure(t *testing.T) {
	for _, mode := range []string{"error", "missing", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			b := &reviewStatusBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), status: func(ctx context.Context, session imapserver.Session, mailbox string, options *imapserver.StatusOptions) (*imap.StatusData, error) {
				switch mode {
				case "error":
					return nil, fmt.Errorf("private backend failure")
				case "missing":
					return &imap.StatusData{Mailbox: mailbox}, nil
				default:
					return &imap.StatusData{Mailbox: mailbox, Values: map[imap.StatusItemKeyword]any{imap.StatusItemMailboxID: "bad) [injected"}}, nil
				}
			}}
			client, reader := reviewRawSession(t, b, nil)
			lines := reviewCommand(t, client, reader, "C", "CREATE Created", "NO")
			wire := strings.Join(lines, "")
			if !strings.Contains(wire, "[SERVERBUG]") || !strings.Contains(wire, "mailbox created") || strings.Contains(wire, "private backend") {
				t.Fatalf("CREATE partial success = %q", lines)
			}
			lines = reviewCommand(t, client, reader, "L", `LIST "" Created`, "OK")
			if !strings.Contains(strings.Join(lines, ""), "Created") {
				t.Fatal("created mailbox was rolled back")
			}
			lines = reviewCommand(t, client, reader, "S", "SELECT INBOX", "NO")
			wire = strings.Join(lines, "")
			if strings.Contains(wire, "FLAGS") || strings.Contains(wire, "EXISTS") || strings.Contains(wire, "[MAILBOXID") || strings.Contains(wire, "private backend") {
				t.Fatalf("failed SELECT leaked snapshot: %q", lines)
			}
			reviewCommand(t, client, reader, "F", "FETCH 1 (UID)", "BAD")
		})
	}
}
