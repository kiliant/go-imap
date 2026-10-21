package imapserver_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func review6fFill(t *testing.T, b imapserver.Backend, n int) {
	t.Helper()
	s, err := b.Authenticate(t.Context(), &imapserver.ConnInfo{}, &imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background(), nil)
	for i := 0; i < n; i++ {
		if _, err := s.Append(t.Context(), "INBOX", strings.NewReader("Subject: example\r\n\r\nbody\r\n"), nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReview6fRepeatedFetchUID(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			b := r4Backend(t, kind)
			review6fFill(t, b, 1)
			s, err := b.Authenticate(t.Context(), &imapserver.ConnInfo{}, &imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background(), nil)
			selected, err := s.Select(t.Context(), "INBOX", &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { return nil }}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer selected.Mailbox.Unselect(context.Background(), nil)
			writer := &imapserver.FetchWriter{WriteFunc: func(_ context.Context, data *imap.FetchMessageData) error {
				values := data.Items["UID"]
				if len(values) != 1 || values[0] != imap.FetchDataUID(selected.Snapshot.UIDs[0]) {
					t.Errorf("repeated backend UID request: %#v", values)
				}
				return nil
			}}
			if err := selected.Mailbox.Fetch(t.Context(), writer, imap.UIDSetNum(selected.Snapshot.UIDs[0]), &imapserver.FetchOptions{Items: []imap.FetchItem{imap.FetchItemUID, imap.FetchItemUID}}); err != nil {
				t.Fatal(err)
			}
			w := r4Open(t, b, &imapserver.Options{AllowInsecureAuth: true})
			w.cmd("S", "SELECT INBOX")
			for i, command := range []string{
				"FETCH 1 (UID UID FLAGS)",
				"FETCH 1 (uid uId FLAGS)",
				"UID FETCH 1 (UID UID BODY.PEEK[])",
				"FETCH 1 (UID UID MODSEQ)",
			} {
				tag := fmt.Sprintf("F%d", i)
				got := w.cmd(tag, command)
				if !strings.HasPrefix(r4Tagged(got), tag+" OK") {
					t.Errorf("%s: %q", command, got)
				}
				if !r4Has(got, `\* 1 FETCH \(.*UID 1[ )]`) {
					t.Errorf("missing message UID for %s: %q", command, got)
				}
			}
		})
	}
}

func TestReview6fHeldRemovalAndLargeStore(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		for _, form := range []string{"STORE", "UID STORE"} {
			t.Run(kind+"/"+form, func(t *testing.T) {
				b := r4Backend(t, kind)
				review6fFill(t, b, 12)
				opts := &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{MaxQueuedUpdateBytes: 128}}
				a, by, other := r4Open(t, b, opts), r4Open(t, b, opts), r4Open(t, b, opts)
				for _, w := range []*r4Wire{a, by, other} {
					w.cmd("S", "SELECT INBOX")
				}
				other.cmd("D", `STORE 12 +FLAGS.SILENT (\Deleted)`)
				other.cmd("X", "EXPUNGE")
				a.quiet(20 * time.Millisecond)
				by.quiet(20 * time.Millisecond)
				if got := a.cmd("F", form+` 1:11 +FLAGS.SILENT (\Seen)`); !strings.HasPrefix(r4Tagged(got), "F OK") {
					t.Fatal(got)
				}
				for _, w := range []*r4Wire{a, by, other} {
					if got := w.cmd("N", "NOOP"); !strings.HasPrefix(r4Tagged(got), "N OK") {
						t.Fatal(got)
					}
				}
			})
		}
	}
}

func TestReview6fLargeWithheldRemovalThenChange(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			b := r4Backend(t, kind)
			review6fFill(t, b, 12)
			opts := &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{MaxQueuedUpdateBytes: 32}}
			by, other := r4Open(t, b, opts), r4Open(t, b, opts)
			by.cmd("S", "SELECT INBOX")
			other.cmd("S", "SELECT INBOX")
			other.cmd("D", `STORE 1:11 +FLAGS.SILENT (\Deleted)`)
			by.quiet(20 * time.Millisecond)
			other.cmd("X", "EXPUNGE")
			by.quiet(20 * time.Millisecond)
			other.cmd("F", `UID STORE 12 +FLAGS.SILENT (\Flagged)`)
			got := by.cmd("N", "NOOP")
			if !strings.HasPrefix(r4Tagged(got), "N OK") {
				t.Fatal(got)
			}
			removals := 0
			for _, line := range got {
				if strings.HasSuffix(line, " EXPUNGE") {
					removals++
				}
			}
			if removals != 11 {
				t.Fatalf("lost removals: %d, %q", removals, got)
			}
		})
	}
}

func TestReview6fStoreStreamingCompletion(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		for _, mode := range []string{"plain", "condstore", "conditional", "conditional-silent"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				b := r4Backend(t, kind)
				review6fFill(t, b, 8)
				w := r4Open(t, b, &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{MaxStoreResponseBytes: 1100}})
				if mode != "plain" {
					w.cmd("E", "ENABLE CONDSTORE")
				}
				selected := w.cmd("S", "SELECT INBOX")
				command := `STORE 1:* +FLAGS (\Seen)`
				conditional := strings.HasPrefix(mode, "conditional")
				if conditional {
					clock := r4HighestModSeq(selected)
					w.cmd("A", `UID STORE 2 +FLAGS.SILENT (\Flagged)`)
					silent := ""
					if mode == "conditional-silent" {
						silent = ".SILENT"
					}
					command = fmt.Sprintf(`STORE 1:* (UNCHANGEDSINCE %d) +FLAGS%s (\Seen)`, clock, silent)
				}
				got := w.cmd("F", command)
				if !strings.HasPrefix(r4Tagged(got), "F OK") {
					t.Fatal(got)
				}
				if conditional && !strings.Contains(r4Tagged(got), "[MODIFIED 2]") {
					t.Fatalf("lost MODIFIED: %q", got)
				}
				count := 0
				for _, line := range got {
					if !strings.Contains(line, " FETCH ") {
						continue
					}
					count++
					if mode != "plain" && !strings.Contains(line, "MODSEQ") {
						t.Fatalf("missing MODSEQ: %s", line)
					}
					if mode == "conditional-silent" && !r4Has([]string{line}, `\bUID 2[ )]`) && strings.Contains(line, "FLAGS") {
						t.Fatalf("silent flag leak: %s", line)
					}
				}
				if count != 8 {
					t.Fatalf("got %d results: %q", count, got)
				}
				if after := w.cmd("N", "NOOP"); r4Has(after, ` FETCH `) {
					t.Fatalf("duplicate STORE updates: %q", after)
				}
			})
		}
	}
}

type review6fLowerBackend struct{ *memory.Backend }
type review6fLowerSession struct{ imapserver.Session }
type review6fLowerSelected struct {
	imapserver.SelectedMailbox
	imapserver.CondStoreMailbox
}

func (b *review6fLowerBackend) Authenticate(ctx context.Context, i *imapserver.ConnInfo, c *imapserver.Credentials, o *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, e := b.Backend.Authenticate(ctx, i, c, o)
	if e != nil {
		return nil, e
	}
	return &review6fLowerSession{s}, nil
}
func (s *review6fLowerSession) Select(ctx context.Context, n string, u *imapserver.Updater, o *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, e := s.Session.Select(ctx, n, u, o)
	if e == nil {
		r.Mailbox = &review6fLowerSelected{r.Mailbox, r.Mailbox.(imapserver.CondStoreMailbox)}
	}
	return r, e
}
func (s *review6fLowerSelected) Fetch(ctx context.Context, w *imapserver.FetchWriter, u imap.UIDSet, o *imapserver.FetchOptions) error {
	return s.SelectedMailbox.Fetch(ctx, e74LowerWriter(w), u, o)
}
func (s *review6fLowerSelected) Search(ctx context.Context, q *imapserver.SearchQuery, o *imapserver.SearchOptions) (*imapserver.SearchResult, error) {
	r, e := s.SelectedMailbox.Search(ctx, q, o)
	if r != nil {
		r.ModSeq = 0
	}
	return r, e
}

func TestReview6fLowerCaseFrameworkPaths(t *testing.T) {
	for _, test := range []struct{ enable, command string }{{"CONDSTORE", "SEARCH MODSEQ 1"}, {"CONDSTORE", "SEARCH RETURN (MIN) MODSEQ 1"}, {"CONDSTORE", "SEARCH RETURN (MAX) MODSEQ 1"}, {"CONDSTORE", "SEARCH RETURN (PARTIAL 1:1) MODSEQ 1"}, {"UIDONLY", "UID FETCH 1 (FLAGS)"}} {
		t.Run(test.command, func(t *testing.T) {
			b := &review6fLowerBackend{r4Memory()}
			review6fFill(t, b, 2)
			w := r4Open(t, b, nil)
			w.cmd("E", "ENABLE "+test.enable)
			w.cmd("S", "SELECT INBOX")
			got := w.cmd("F", test.command)
			if !strings.HasPrefix(r4Tagged(got), "F OK") || r4Has(got, `\buid `) {
				t.Fatal(got)
			}
		})
	}
}

func TestReview6fSortAndParameterGates(t *testing.T) {
	for _, test := range []struct{ deny, command string }{
		{"SORT=DISPLAY", "SORT (DISPLAYFROM) UTF-8 ALL"}, {"SORT=DISPLAY", "UID SORT (DISPLAYTO) UTF-8 ALL"},
		{"", "SORT (FOO) UTF-8 ALL"}, {"", "SORT (REVERSE REVERSE SUBJECT) UTF-8 ALL"}, {"", "SORT (RELEVANCY) UTF-8 ALL"},
		{"SEARCH=FUZZY", "SORT (RELEVANCY) UTF-8 FUZZY ALL"},
		{"URL-PARTIAL", `GENURLAUTH "imap://alice@localhost/INBOX/;UID=1/;PARTIAL=0.1;URLAUTH=user+alice" INTERNAL`},
		{"URL-PARTIAL", `URLFETCH "imap://alice@localhost/INBOX/;UID=1/;PARTIAL=0.1;URLAUTH=user+alice"`},
		{"FILTERS", `SEARCH FILTER x`}, {"FILTERS", `SORT (SUBJECT) UTF-8 FILTER x`}, {"FILTERS", `ESEARCH IN (mailboxes INBOX) FILTER x`},
	} {
		t.Run(test.command, func(t *testing.T) {
			w := r4Open(t, &e74FilteredBackend{r4Memory(), test.deny}, nil)
			w.cmd("S", "SELECT INBOX")
			if got := w.cmd("F", test.command); !strings.HasPrefix(r4Tagged(got), "F BAD") {
				t.Fatal(got)
			}
			w.cmd("N", "NOOP")
		})
	}
}

func TestReview6fClientCompressionReset(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		t.Run(fmt.Sprint(encrypted), func(t *testing.T) {
			server := imapserver.New(r4Memory(), &imapserver.Options{AllowInsecureAuth: true})
			peer, raw := net.Pipe()
			if encrypted {
				peer = tls.Server(peer, serverTLSConfig(t, testCertificate(t)))
				raw = tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			go func() { _ = server.ServeConn(ctx, peer, nil) }()
			c := imapclient.NewClient(raw, &imapclient.Options{AllowInsecureAuth: !encrypted})
			t.Cleanup(func() { _ = c.Close(); _ = server.Close(context.Background(), nil) })
			if err := c.WaitGreeting(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if err := c.Login(ctx, "alice", "secret", nil); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := c.Compress(ctx, nil); err != nil {
					t.Fatal(err)
				}
				if err := c.Unauthenticate(ctx, nil); err != nil {
					t.Fatal(err)
				}
				if c.Compressed() {
					t.Fatal("client kept COMPRESS")
				}
				if err := c.Login(ctx, "alice", "secret", nil); err != nil {
					t.Fatal(err)
				}
				if err := c.Noop(nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestReview6fReadTimeoutSendsBye(t *testing.T) {
	for _, partial := range []string{"", "P FETCH 1 ("} {
		t.Run(partial, func(t *testing.T) {
			w := r4Open(t, r4Memory(), &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{ReadTimeout: 150 * time.Millisecond, PreAuthTimeout: 2 * time.Second}})
			w.cmd("S", "SELECT INBOX")
			if partial != "" {
				w.write(partial)
			}
			if line, err := w.line(); err != nil || !strings.HasPrefix(line, "* BYE ") {
				t.Fatalf("read timeout = %q, %v", line, err)
			}
		})
	}
}
