package imapserver_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func TestReviewSavedDateCapability(t *testing.T) {
	for _, kind := range []string{"memory", "maildir", "filtered", "unwitnessed"} {
		t.Run(kind, func(t *testing.T) {
			var backend imapserver.Backend
			if kind == "filtered" || kind == "unwitnessed" {
				b := &reviewWitnessBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
				if kind == "filtered" {
					b.filtered = map[string]bool{"SAVEDATE": true}
				} else {
					b.withheld = map[string]bool{"SAVEDATE": true}
				}
				backend = b
			} else {
				backend = reviewBackend(t, kind)
			}
			client, reader := reviewRawSession(t, backend, nil)
			reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
			for _, key := range []string{"SAVEDBEFORE", "SAVEDON", "SAVEDSINCE"} {
				for _, prefix := range []string{"SEARCH ", "UID SEARCH NOT ", "SEARCH OR ALL "} {
					status := "NO"
					if kind == "memory" {
						status = "OK"
					}
					lines := reviewCommand(t, client, reader, "Q", prefix+key+" 1-Jan-2030", status)
					if status == "NO" && !strings.Contains(strings.Join(lines, ""), "[CANNOT] search key "+key) {
						t.Fatalf("missing capability refusal: %q", lines)
					}
				}
			}
			reviewCommand(t, client, reader, "Q", "SEARCH BEFORE 1-Jan-2030 LARGER 0", "OK")
		})
	}
}

func TestReviewCreateTrailingDelimiter(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			client, reader := reviewRawSession(t, reviewBackend(t, kind), nil)
			created := reviewCommand(t, client, reader, "C", "CREATE parent/", "OK")
			if kind == "memory" && !strings.Contains(strings.Join(created, ""), "[MAILBOXID (") {
				t.Fatalf("CREATE omitted identity: %q", created)
			}
			lines := reviewCommand(t, client, reader, "L", `LIST "" "parent*"`, "OK")
			wire := strings.Join(lines, "")
			if strings.Contains(wire, "parent/") || strings.Contains(wire, `\NonExistent`) {
				t.Fatalf("created trailing delimiter or placeholder: %q", lines)
			}
			reviewCommand(t, client, reader, "S", "SELECT parent", "OK")
			reviewCommand(t, client, reader, "C", "CREATE parent/", "NO")
			reviewCommand(t, client, reader, "C", "CREATE /", "NO")
		})
	}
}

type dotHierarchyBackend struct{ *memory.Backend }
type dotHierarchySession struct{ imapserver.Session }

func (b *dotHierarchyBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, credentials, options)
	return &dotHierarchySession{Session: s}, err
}

func (s *dotHierarchySession) List(ctx context.Context, writer *imapserver.ListWriter, reference string, patterns []string, options *imapserver.ListOptions) error {
	if slices.Equal(patterns, []string{""}) {
		return writer.WriteList(ctx, &imap.ListData{Delimiter: '.', Mailbox: ""})
	}
	return s.Session.List(ctx, writer, reference, patterns, options)
}

func (s *dotHierarchySession) Create(ctx context.Context, name string, options *imapserver.CreateOptions) error {
	return s.Session.Create(ctx, strings.TrimSuffix(name, "."), options)
}

func TestReviewCreateBackendDelimiter(t *testing.T) {
	b := &dotHierarchyBackend{memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
	client, reader := reviewRawSession(t, b, nil)
	lines := reviewCommand(t, client, reader, "C", "CREATE parent.", "OK")
	if !strings.Contains(strings.Join(lines, ""), "[MAILBOXID (") {
		t.Fatalf("CREATE identity for backend delimiter = %q", lines)
	}
	reviewCommand(t, client, reader, "S", "SELECT parent", "OK")
}

func TestReviewRev2RecentStatus(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			client, reader := reviewRawSession(t, reviewBackend(t, kind), nil)
			reviewCommand(t, client, reader, "T", "STATUS INBOX (RECENT)", "OK")
			reviewCommand(t, client, reader, "E", "ENABLE IMAP4rev2", "OK")
			reviewCommand(t, client, reader, "T", "STATUS INBOX (RECENT)", "BAD")
			reviewCommand(t, client, reader, "T", `LIST "" "*" RETURN (STATUS (RECENT))`, "BAD")
			reviewCommand(t, client, reader, "T", "STATUS INBOX (MESSAGES)", "OK")
		})
	}
}

func TestReviewNotifyStatus(t *testing.T) {
	for _, mode := range []string{"rev1", "condstore", "rev2", "filtered"} {
		t.Run(mode, func(t *testing.T) {
			b := &reviewWitnessBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
			if mode == "filtered" {
				b.filtered = map[string]bool{"CONDSTORE": true}
			}
			client, reader := reviewRawSession(t, b, nil)
			reviewCommand(t, client, reader, "C", "CREATE other", "OK")
			if mode == "condstore" {
				reviewCommand(t, client, reader, "E", "ENABLE CONDSTORE", "OK")
			} else if mode == "rev2" {
				reviewCommand(t, client, reader, "E", "ENABLE IMAP4rev2", "OK")
			}
			lines := reviewCommand(t, client, reader, "N", "NOTIFY SET STATUS (mailboxes other (MessageNew MessageExpunge))", "OK")
			lines = append(lines, reviewCommand(t, client, reader, "F", "NOOP", "OK")...)
			check := func(lines []string) {
				t.Helper()
				var status string
				for _, line := range lines {
					if strings.HasPrefix(line, "* STATUS ") {
						status = line
					}
				}
				if status == "" {
					t.Fatalf("missing NOTIFY STATUS: %q", lines)
				}
				for _, item := range []string{"UIDNEXT", "MESSAGES", "UIDVALIDITY"} {
					if !strings.Contains(status, item+" ") {
						t.Errorf("missing %s: %s", item, status)
					}
				}
				// RFC 5465 section 5: only a client that enabled CONDSTORE gets it.
				if strings.Contains(status, "HIGHESTMODSEQ ") != (mode == "condstore") {
					t.Errorf("HIGHESTMODSEQ projection: %s", status)
				}
				if strings.Contains(status, "RECENT ") != (mode != "rev2") {
					t.Errorf("RECENT projection: %s", status)
				}
			}
			check(lines)
			raw := "Subject: notify\r\n\r\nhello\r\n"
			lines = reviewCommand(t, client, reader, "A", fmt.Sprintf("APPEND other {%d+}\r\n%s", len(raw), raw), "OK")
			lines = append(lines, reviewCommand(t, client, reader, "F", "NOOP", "OK")...)
			check(lines)
		})
	}
}

func TestReviewQuotaReplacement(t *testing.T) {
	client, reader := reviewRawSession(t, reviewBackend(t, "memory"), nil)
	check := func(command, status, want string) {
		t.Helper()
		lines := reviewCommand(t, client, reader, "Q", command, status)
		if want != "" && !slices.Contains(lines, want) {
			t.Fatalf("%s = %q, want %s", command, lines, want)
		}
	}
	check("GETQUOTA root", "OK", "* QUOTA root ()")
	check("SETQUOTA root (STORAGE 10 MESSAGE 2)", "OK", "* QUOTA root (STORAGE 0 10 MESSAGE 0 2)")
	check("SETQUOTA root (STORAGE 20 FUTURE 1)", "NO", "")
	check("GETQUOTA root", "OK", "* QUOTA root (STORAGE 0 10 MESSAGE 0 2)")
	check("SETQUOTA root (MESSAGE 0)", "OK", "* QUOTA root (MESSAGE 0 0)")
	check("SETQUOTA root ()", "OK", "* QUOTA root ()")
}

func TestReviewACLCompatibilityRights(t *testing.T) {
	client, reader := reviewRawSession(t, reviewBackend(t, "memory"), nil)
	lines := reviewCommand(t, client, reader, "Q", "MYRIGHTS INBOX", "OK")
	if !strings.Contains(strings.Join(lines, ""), "acdeiklprstwx") {
		t.Fatalf("owner rights missing k: %q", lines)
	}
	lines = reviewCommand(t, client, reader, "Q", "LISTRIGHTS INBOX bob", "OK")
	if !strings.Contains(strings.Join(lines, " "), " k ") {
		t.Fatalf("LISTRIGHTS missing k: %q", lines)
	}
	for _, tc := range []struct{ input, want string }{
		{"lrk", "cklr"}, {"-k", "lr"}, {"+c", "cklr"},
		{"-c", "lr"}, {"+d", "delrtx"}, {"-t", "delrx"}, {"-d", "lr"},
	} {
		reviewCommand(t, client, reader, "Q", "SETACL INBOX bob "+tc.input, "OK")
		lines := reviewCommand(t, client, reader, "Q", "GETACL INBOX", "OK")
		if !strings.Contains(strings.Join(lines, ""), "bob "+tc.want) {
			t.Fatalf("SETACL %s = %q, want %s", tc.input, lines, tc.want)
		}
	}
}

func TestReviewMultiSearchSources(t *testing.T) {
	client, reader := reviewRawSession(t, reviewBackend(t, "memory"), nil)
	for _, name := range []string{"INBOX", "parent", "parent/child", "parent/child/deep", "other", "literal*"} {
		if name != "INBOX" {
			reviewCommand(t, client, reader, "C", fmt.Sprintf("CREATE %q", name), "OK")
		}
		raw := "Subject: source\r\n\r\nmessage\r\n"
		reviewCommand(t, client, reader, "A", fmt.Sprintf("APPEND %q {%d+}\r\n%s", name, len(raw), raw), "OK")
	}
	reviewCommand(t, client, reader, "C", "CREATE Empty", "OK")
	reviewCommand(t, client, reader, "U", "SUBSCRIBE other", "OK")
	reviewCommand(t, client, reader, "Q", "ESEARCH IN (selected) ALL", "BAD")
	reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
	for _, tc := range []struct {
		source string
		want   []string
	}{
		{"mailboxes INBOX", []string{"INBOX"}},
		{"mailboxes (INBOX other)", []string{"INBOX", "other"}},
		{`mailboxes "literal*"`, []string{"literal*"}},
		{"subtree parent", []string{"parent", "parent/child", "parent/child/deep"}},
		{"subtree-one parent", []string{"parent", "parent/child"}},
		{"subtree-one (parent other)", []string{"parent", "parent/child", "other"}},
		{"subscribed", []string{"other"}},
		{"selected", []string{"INBOX"}},
		{"mailboxes (Empty Missing)", nil},
		{"mailboxes inbox selected subtree parent", []string{"INBOX", "parent", "parent/child", "parent/child/deep"}},
		{"PeRsOnAl", []string{"INBOX", "parent", "parent/child", "parent/child/deep", "other", "literal*"}},
		{"inboxes", []string{"INBOX", "parent", "parent/child", "parent/child/deep", "other", "literal*"}},
	} {
		lines := reviewCommand(t, client, reader, "Q", "ESEARCH IN ("+tc.source+") ALL", "OK")
		var got []string
		for _, line := range lines {
			if !strings.HasPrefix(line, "* ESEARCH ") {
				continue
			}
			if name, ok := esearchMailbox(line); ok {
				got = append(got, name)
			}
		}
		slices.Sort(got)
		slices.Sort(tc.want)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s = %q, want %q; wire %q", tc.source, got, tc.want, lines)
		}
	}
	for _, source := range []string{"INBOX", "", "selected-delayed", "mailboxes ()", "mailboxes INBOX other", "future"} {
		reviewCommand(t, client, reader, "Q", "ESEARCH IN ("+source+") ALL", "BAD")
	}
}

func TestReviewMultiSearchClientRoundTrip(t *testing.T) {
	client, _ := newGroupAClient(t, t.Context())
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Create("other", nil).Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	seedMessages(t, t.Context(), client, 2)
	data, err := client.MultiSearch(imap.SearchAll, &imapclient.MultiSearchOptions{
		Sources: []imapclient.MultiSearchSource{imapclient.MultiSearchMailboxes{Names: []string{"INBOX", "other"}}},
	}).Wait(t.Context())
	if err != nil || len(data.Results) != 1 || data.Results[0].Mailbox != "INBOX" {
		t.Fatalf("client MULTISEARCH = %+v, %v", data, err)
	}
}

type prefixNamespaceBackend struct{ *memory.Backend }

// prefixNamespaceSession keeps memory's MULTISEARCH while presenting a
// Courier-style personal namespace whose prefix excludes INBOX itself.
type prefixNamespaceSession struct {
	imapserver.Session
	imapserver.MultiSearchSession
}

func (b *prefixNamespaceBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &prefixNamespaceSession{Session: s, MultiSearchSession: s.(imapserver.MultiSearchSession)}, nil
}

func (s *prefixNamespaceSession) Namespace(context.Context, *imapserver.NamespaceOptions) (*imap.NamespaceData, error) {
	return &imap.NamespaceData{Personal: []imap.NamespaceDescriptor{{Prefix: "INBOX/", Delimiter: '/'}}}, nil
}

func TestReviewMultiSearchPersonalPrefixIncludesInbox(t *testing.T) {
	b := &prefixNamespaceBackend{memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
	client, reader := reviewRawSession(t, b, nil)
	for _, name := range []string{"INBOX", "INBOX/sub", "other"} {
		if name != "INBOX" {
			reviewCommand(t, client, reader, "C", "CREATE "+name, "OK")
		}
		// Empty mailboxes produce no ESEARCH response, so give each a message.
		raw := "Subject: source\r\n\r\nmessage\r\n"
		reviewCommand(t, client, reader, "A", fmt.Sprintf("APPEND %s {%d+}\r\n%s", name, len(raw), raw), "OK")
	}
	for _, source := range []string{"personal", "inboxes"} {
		lines := reviewCommand(t, client, reader, "Q", "ESEARCH IN ("+source+") ALL", "OK")
		var got []string
		for _, line := range lines {
			if name, ok := esearchMailbox(line); ok {
				got = append(got, name)
			}
		}
		slices.Sort(got)
		if want := []string{"INBOX", "INBOX/sub"}; !slices.Equal(got, want) {
			t.Errorf("%s = %q, want %q", source, got, want)
		}
	}
}

// TestReviewRecursiveMatchRedundantChildInfo mirrors imaptest's listext script:
// RFC 5258 section 3.5 suppresses CHILDINFO when every subscribed descendant is
// returned anyway, and omits a parent returned only for it. memory evaluates
// natively; the other two exercise the framework's evaluation.
func TestReviewRecursiveMatchRedundantChildInfo(t *testing.T) {
	for _, kind := range []string{"memory", "framework", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			var backend imapserver.Backend
			if kind == "framework" {
				backend = &withoutRecursiveMatchBackend{inner: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), t: t}
			} else {
				backend = reviewBackend(t, kind)
			}
			client, reader := reviewRawSession(t, backend, nil)
			for _, name := range []string{"p/", "p/test1", "p/test2/", "p/test2/test1", "p/test8/test8", "p/test8/test9"} {
				reviewCommand(t, client, reader, "C", "CREATE "+name, "OK")
			}
			for _, name := range []string{"p/test1", "p/test2/test1", "p/test8/test8", "p/test8/test9"} {
				reviewCommand(t, client, reader, "U", "SUBSCRIBE "+name, "OK")
			}
			list := func(args string) map[string]string {
				t.Helper()
				lines := reviewCommand(t, client, reader, "L", "LIST (SUBSCRIBED RECURSIVEMATCH) "+args, "OK")
				byName := make(map[string]string)
				for _, line := range lines {
					_, rest, ok := strings.Cut(line, `* LIST `)
					if !ok {
						continue
					}
					_, rest, _ = strings.Cut(rest, `"/" `)
					name, _, _ := strings.Cut(strings.TrimRight(rest, "\r\n"), " ")
					byName[strings.Trim(name, `"`)] = line
				}
				return byName
			}
			childInfo := func(line string) bool { return strings.Contains(line, `"CHILDINFO" ("SUBSCRIBED")`) }

			if got := list(`"" (p/test2)`); !childInfo(got["p/test2"]) {
				t.Errorf("parent alone = %q, want p/test2 with CHILDINFO", got)
			}
			got := list(`"" (p/test2 p/test2/test1)`)
			if _, ok := got["p/test2"]; ok || got["p/test2/test1"] == "" || childInfo(got["p/test2/test1"]) {
				t.Errorf("parent and child = %q, want only p/test2/test1 without CHILDINFO", got)
			}
			if got := list(`"" "p*test8"`); !childInfo(got["p/test8"]) || got["p/test8/test8"] == "" {
				t.Errorf("partial children = %q, want p/test8 with CHILDINFO and p/test8/test8", got)
			}
			got = list(`"" "p/*"`)
			for _, name := range []string{"p/test2", "p/test8"} {
				if line, ok := got[name]; ok {
					t.Errorf("all children returned, but %s was too: %q", name, line)
				}
			}
			for _, name := range []string{"p/test1", "p/test2/test1", "p/test8/test8", "p/test8/test9"} {
				if got[name] == "" || childInfo(got[name]) {
					t.Errorf("%s = %q, want it subscribed without CHILDINFO", name, got[name])
				}
			}
		})
	}
}

// esearchMailbox extracts an ESEARCH MAILBOX correlator, which RFC 7377 encodes
// as an astring: an atom where possible, a quoted string otherwise.
func esearchMailbox(line string) (string, bool) {
	_, rest, ok := strings.Cut(line, "MAILBOX ")
	if !ok {
		return "", false
	}
	if quoted, ok := strings.CutPrefix(rest, `"`); ok {
		name, _, _ := strings.Cut(quoted, `"`)
		return name, true
	}
	name, _, _ := strings.Cut(rest, " ")
	return name, true
}
