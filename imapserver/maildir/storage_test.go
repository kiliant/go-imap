package maildir

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

const sampleMessage = "From: alice@example.test\r\nTo: bob@example.test\r\nSubject: durable\r\nContent-Type: text/plain\r\n\r\nbody\r\n"

func testOptions() *Options {
	return &Options{Accounts: []AccountOptions{{Username: "alice", Password: "secret"}}, PollInterval: time.Minute}
}
func openTest(t *testing.T, root string) *Backend {
	t.Helper()
	b, err := Open(context.Background(), root, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(context.Background(), nil); err != nil {
			t.Error(err)
		}
	})
	return b
}
func loginTest(t *testing.T, b *Backend) *session {
	t.Helper()
	s, err := b.Authenticate(context.Background(), nil, &imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s.(*session)
}
func selectTest(t *testing.T, s *session, name string) *selected {
	t.Helper()
	result, err := s.Select(context.Background(), name, &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { return nil }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result.Mailbox.(*selected)
}
func appendTest(t *testing.T, s *session, name string) *imap.AppendData {
	t.Helper()
	data, err := s.Append(context.Background(), name, strings.NewReader(sampleMessage), nil)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func discardFetch() *imapserver.FetchWriter {
	return &imapserver.FetchWriter{WriteFunc: func(context.Context, *imap.FetchMessageData) error { return nil }}
}
func discardExpunge() *imapserver.ExpungeWriter {
	return &imapserver.ExpungeWriter{WriteFunc: func(context.Context, imap.UID, *imapserver.WriteExpungeOptions) error { return nil }}
}
func accountPath(root string) string {
	return filepath.Join(root, fmt.Sprintf("%x", sha256.Sum256([]byte("alice"))))
}

func TestRestartPreservesMailboxState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	if err := s.Create(ctx, "Archive/cur", nil); err != nil {
		t.Fatal(err)
	}
	first := appendTest(t, s, "INBOX")
	second := appendTest(t, s, "INBOX")
	sel := selectTest(t, s, "INBOX")
	flags := []imap.Flag{imap.FlagSeen, imap.FlagDeleted, imap.Flag("persisted-keyword")}
	if err := sel.Store(ctx, discardFetch(), imap.UIDSetNum(first.UID), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: flags}, nil); err != nil {
		t.Fatal(err)
	}
	before := s.account.state.Mailboxes["INBOX"].HighestModSeq
	if err := sel.Expunge(ctx, discardExpunge(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Subscribe(ctx, "does/not/exist", nil); err != nil {
		t.Fatal(err)
	}
	value := ""
	for _, name := range []string{"", "INBOX"} {
		if err := s.SetMetadata(ctx, name, []imap.MetadataEntry{{Name: "/private/persisted", Value: &value}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	old, err := cloneState(s.account.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, root)
	s = loginTest(t, b)
	sel = selectTest(t, s, "INBOX")
	if !reflect.DeepEqual(old, s.account.state) {
		t.Fatalf("state changed across restart:\nbefore=%#v\nafter=%#v", old, s.account.state)
	}
	resync, err := sel.Resync(ctx, &imapserver.QResyncSelect{UIDValidity: first.UIDValidity, ModSeq: before}, nil)
	if err != nil || !resync.Vanished.Contains(first.UID) {
		t.Fatalf("resync=%#v, %v", resync, err)
	}
	if resync.Vanished.Contains(second.UID) {
		t.Fatal("surviving UID vanished")
	}
	next := appendTest(t, s, "INBOX")
	if next.UID <= second.UID || next.UIDValidity != first.UIDValidity {
		t.Fatalf("UID reused: %#v", next)
	}
	if !imap.ContainsFlag(s.account.state.Mailboxes["INBOX"].Flags, imap.Flag("persisted-keyword")) {
		t.Fatal("applicable keyword disappeared")
	}
}

func TestSearchReportsSurvivingMessageModSeq(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	first := appendTest(t, s, "INBOX")
	second := appendTest(t, s, "INBOX")
	sel := selectTest(t, s, "INBOX")
	if err := sel.Store(ctx, discardFetch(), imap.UIDSetNum(first.UID), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil); err != nil {
		t.Fatal(err)
	}
	result, err := sel.Search(ctx, nil, nil)
	if err != nil || result.ModSeq != s.account.state.Mailboxes["INBOX"].Messages[0].ModSeq {
		t.Fatalf("SEARCH omitted highest matching MODSEQ: %+v, %v", result, err)
	}
	want := s.account.state.Mailboxes["INBOX"].Messages[1].ModSeq
	if err := sel.Expunge(ctx, discardExpunge(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, root)
	sel = selectTest(t, loginTest(t, b), "INBOX")
	result, err = sel.Search(ctx, nil, nil)
	if err != nil || !slices.Equal(result.UIDs, []imap.UID{second.UID}) || result.ModSeq != want {
		t.Fatalf("SEARCH used mailbox/tombstone MODSEQ instead of surviving matches: %+v, %v", result, err)
	}
}

func TestMailboxMetadataAdvancesQResyncClock(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	appendTest(t, s, "INBOX")
	before := s.account.state.Mailboxes["INBOX"].HighestModSeq
	value := "annotation"
	entries := []imap.MetadataEntry{{Name: "/private/test", Value: &value}}
	if err := s.SetMetadata(ctx, "inbox", entries, nil); err != nil {
		t.Fatal(err)
	}
	clock := s.account.state.Mailboxes["INBOX"].HighestModSeq
	if clock <= before {
		t.Fatal("mailbox annotation did not advance QRESYNC clock")
	}
	for _, name := range []string{"INBOX", ""} {
		if err := s.SetMetadata(ctx, name, entries, nil); err != nil {
			t.Fatal(err)
		}
	}
	if s.account.state.Mailboxes["INBOX"].HighestModSeq != clock {
		t.Fatal("no-op or server metadata changed the mailbox clock")
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, root)
	s = loginTest(t, b)
	m := s.account.state.Mailboxes["INBOX"]
	if m.HighestModSeq != clock || m.Messages[0].ModSeq != before {
		t.Fatal("mailbox clock did not persist or message clock changed")
	}
	if err := s.SetMetadata(ctx, "INBOX", []imap.MetadataEntry{{Name: "/private/test"}}, nil); err != nil {
		t.Fatal(err)
	}
	if s.account.state.Mailboxes["INBOX"].HighestModSeq <= clock {
		t.Fatal("removing annotation did not advance QRESYNC clock")
	}
}

func TestModificationSequenceExhaustion(t *testing.T) {
	ctx := context.Background()
	b := openTest(t, t.TempDir())
	s := loginTest(t, b)
	appendTest(t, s, "INBOX")
	s.account.state.Mailboxes["INBOX"].HighestModSeq = math.MaxInt64
	sel := selectTest(t, s, "INBOX")
	err := sel.Store(ctx, discardFetch(), imap.UIDSetNum(1), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil)
	var protocolErr *imap.Error
	if !errors.As(err, &protocolErr) || protocolErr.Code != imap.CodeLimit || s.account.failure != nil {
		t.Fatalf("clock overflow did not fail safely: %v", err)
	}
	if imap.ContainsFlag(s.account.state.Mailboxes["INBOX"].Messages[0].Flags, imap.FlagSeen) {
		t.Fatal("failed STORE changed flags")
	}
	s.account.state.Mailboxes["INBOX"].HighestModSeq++
	raw, err := json.Marshal(s.account.state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeState(raw); err == nil {
		t.Fatal("out-of-range persistent modification sequence accepted")
	}
}

func TestRecoverEveryMutationBoundary(t *testing.T) {
	for _, operation := range []string{"append", "store", "expunge", "copy", "move", "rename", "rename-inbox", "rename-ancestor"} {
		stages := []string{"journal", "action", "state", "complete"}
		if operation == "rename" || operation == "rename-ancestor" {
			stages = append(stages, "relocate-stage", "relocate-merged")
		}
		for _, stage := range stages {
			t.Run(operation+"/"+stage, func(t *testing.T) {
				ctx := context.Background()
				root := t.TempDir()
				b := openTest(t, root)
				s := loginTest(t, b)
				if err := s.Create(ctx, "Target", nil); err != nil {
					t.Fatal(err)
				}
				name := "INBOX"
				if operation == "rename" {
					name = "Parent"
				}
				if operation == "rename-ancestor" {
					name = "Parent/Child"
				}
				if name != "INBOX" {
					if err := s.Create(ctx, name, nil); err != nil {
						t.Fatal(err)
					}
					if err := s.Create(ctx, name+"/Child", nil); err != nil {
						t.Fatal(err)
					}
				}
				original := appendTest(t, s, name)
				sel := selectTest(t, s, name)
				if operation == "expunge" {
					if err := sel.Store(ctx, discardFetch(), imap.UIDSetNum(original.UID), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil); err != nil {
						t.Fatal(err)
					}
				}
				injected := errors.New("injected interruption")
				fired := false
				b.mu.Lock()
				b.faultHook = func(at string) error {
					if at == stage && !fired {
						fired = true
						return injected
					}
					return nil
				}
				b.mu.Unlock()
				var err error
				switch operation {
				case "append":
					_, err = s.Append(ctx, name, strings.NewReader(sampleMessage), nil)
				case "store":
					err = sel.Store(ctx, discardFetch(), imap.UIDSetNum(original.UID), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil)
				case "expunge":
					err = sel.Expunge(ctx, discardExpunge(), nil, nil)
				case "copy":
					_, err = sel.Copy(ctx, imap.UIDSetNum(original.UID), "Target", nil)
				case "move":
					_, err = sel.Move(ctx, imap.UIDSetNum(original.UID), "Target", nil)
				case "rename":
					err = s.Rename(ctx, name, "Renamed", nil)
				case "rename-ancestor":
					err = s.Rename(ctx, name, "Parent", nil)
				case "rename-inbox":
					err = s.Rename(ctx, name, "Renamed", nil)
				}
				if !fired || !errors.Is(err, injected) {
					t.Fatalf("boundary not reached: fired=%v, %v", fired, err)
				}
				if err := b.Close(ctx, nil); err != nil {
					t.Fatal(err)
				}
				b = openTest(t, root)
				s = loginTest(t, b)
				for box, m := range s.account.state.Mailboxes {
					for _, msg := range m.Messages {
						raw, err := readFile(s.account.root, messagePath(box, msg))
						if err != nil || string(raw) != sampleMessage {
							t.Fatalf("recovered bytes in %q: %q, %v", box, raw, err)
						}
					}
				}
				switch operation {
				case "append":
					if len(s.account.state.Mailboxes[name].Messages) != 2 {
						t.Fatal("append not recovered")
					}
				case "store":
					if !imap.ContainsFlag(s.account.state.Mailboxes[name].Messages[0].Flags, imap.FlagSeen) {
						t.Fatal("STORE not recovered")
					}
				case "expunge":
					if len(s.account.state.Mailboxes[name].Messages) != 0 || len(s.account.state.Mailboxes[name].Tombstones) != 1 {
						t.Fatal("EXPUNGE not recovered")
					}
				case "copy", "move":
					if len(s.account.state.Mailboxes["Target"].Messages) != 1 {
						t.Fatal("destination not recovered")
					}
					want := 1
					if operation == "move" {
						want = 0
					}
					if len(s.account.state.Mailboxes[name].Messages) != want {
						t.Fatal("source not recovered")
					}
				case "rename", "rename-inbox":
					if len(s.account.state.Mailboxes["Renamed"].Messages) != 1 {
						t.Fatal("RENAME not recovered")
					}
					if operation == "rename-inbox" && len(s.account.state.Mailboxes["INBOX"].Messages) != 0 {
						t.Fatal("INBOX not recreated")
					}
				case "rename-ancestor":
					if len(s.account.state.Mailboxes["Parent"].Messages) != 1 || s.account.state.Mailboxes["Parent/Child"] == nil {
						t.Fatal("ancestor RENAME not recovered")
					}
				}
			})
		}
	}
}

func TestLostStateChangesIdentityAndMissingAllocatorFailsClosed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	old := appendTest(t, s, "INBOX")
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(accountPath(root), stateFile)); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, root)
	s = loginTest(t, b)
	m := s.account.state.Mailboxes["INBOX"]
	if m.UIDValidity == old.UIDValidity || len(m.Messages) != 1 {
		t.Fatalf("unsafe identity recovery: %#v", m)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(accountPath(root), identityFile)); err != nil {
		t.Fatal(err)
	}
	if next, err := Open(ctx, root, testOptions()); err == nil {
		next.Close(ctx, nil)
		t.Fatal("missing allocator accepted")
	}
}

func TestRegisteredIdentityLossFailsClosed(t *testing.T) {
	for _, loss := range []string{"account", "empty-account", "root-registry"} {
		t.Run(loss, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			b := openTest(t, root)
			appendTest(t, loginTest(t, b), "INBOX")
			if err := b.Close(ctx, nil); err != nil {
				t.Fatal(err)
			}
			switch loss {
			case "account":
				if err := os.RemoveAll(accountPath(root)); err != nil {
					t.Fatal(err)
				}
			case "empty-account":
				if err := os.RemoveAll(accountPath(root)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(accountPath(root), 0700); err != nil {
					t.Fatal(err)
				}
			case "root-registry":
				if err := os.Remove(filepath.Join(root, ".imap-root.json")); err != nil {
					t.Fatal(err)
				}
			}
			if next, err := Open(ctx, root, testOptions()); err == nil {
				next.Close(ctx, nil)
				t.Fatal("lost identity was silently reused")
			}
		})
	}
}

func TestStorageVersionsAndUnknownFields(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	appendTest(t, s, "INBOX")
	s.account.state.Extra = extraFields{"future": json.RawMessage(`{"value":17}`)}
	s.account.state.Mailboxes["INBOX"].Extra = extraFields{"future-mailbox": json.RawMessage(`[1,2]`)}
	s.account.state.Mailboxes["INBOX"].Messages[0].Extra = extraFields{"future-message": json.RawMessage(`true`)}
	raw, err := json.Marshal(s.account.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicFile(s.account.root, stateFile, raw); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(accountPath(root), stateFile)
	raw = bytes.Replace(raw, []byte(`"version":2`), []byte(`"version":1`), 1)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, root)
	s = loginTest(t, b)
	appendTest(t, s, "INBOX")
	raw, err = readFile(s.account.root, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"future":{"value":17}`, `"future-mailbox":[1,2]`, `"future-message":true`, `"version":2`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("lost %s in %s", want, raw)
		}
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"version":2`), []byte(`"version":999`), 1)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if next, err := Open(ctx, root, testOptions()); err == nil {
		next.Close(ctx, nil)
		t.Fatal("future version accepted")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("future state modified", err)
	}
}

func TestAccountIsolationAndConfigurationCopy(t *testing.T) {
	ctx := context.Background()
	opts := testOptions()
	opts.Accounts = append(opts.Accounts, AccountOptions{Username: "../other/用户", Password: "other"})
	root := t.TempDir()
	b, err := Open(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx, nil)
	opts.Accounts[0].Username = "changed"
	opts.Accounts[0].Password = "changed"
	s := loginTest(t, b)
	appendTest(t, s, "INBOX")
	other, err := b.Authenticate(ctx, nil, &imapserver.Credentials{Mechanism: "LOGIN", Username: "../other/用户", Password: "other"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	status, err := other.Status(ctx, "INBOX", nil)
	if err != nil || status.NumMessages != 0 {
		t.Fatal("cross-account data", status, err)
	}
	for _, mechanism := range []string{"XOAUTH2", "SCRAM-SHA-256", ""} {
		if _, err := b.Authenticate(ctx, nil, &imapserver.Credentials{Mechanism: mechanism, Username: "alice", Password: "secret"}, nil); err == nil {
			t.Fatal("unsupported credentials accepted")
		}
	}
	for _, accounts := range [][]AccountOptions{
		{{Username: "", Password: "p"}},
		{{Username: "a", Password: "p"}, {Username: "a", Password: "q"}},
		// No credential: an empty Password must not mean "the empty password".
		{{Username: "a"}},
	} {
		if value, err := Open(ctx, t.TempDir(), &Options{Accounts: accounts}); err == nil {
			value.Close(ctx, nil)
			t.Fatal("invalid accounts accepted", accounts)
		}
	}
	// Defence in depth behind Open's check: an empty stored password never
	// matches, so an empty LOGIN password cannot authenticate.
	b.accounts["alice"].password = ""
	for _, password := range []string{"", "secret"} {
		if _, err := b.Authenticate(ctx, nil, &imapserver.Credentials{Mechanism: "LOGIN", Username: "alice", Password: password}, nil); err == nil {
			t.Fatalf("password %q authenticated an account with no credential", password)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() && len(entry.Name()) != 64 {
			t.Fatalf("identity leaked into path: %q", entry.Name())
		}
	}
}

func TestAppendExactBytesAndDeliveryNormalization(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	raw := "Subject: exact\n\nAPPEND keeps LF\n"
	date := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := s.Append(ctx, "INBOX", strings.NewReader(raw), &imapserver.AppendOptions{InternalDate: date}); err != nil {
		t.Fatal(err)
	}
	msg := s.account.state.Mailboxes["INBOX"].Messages[0]
	got, err := readFile(s.account.root, messagePath("INBOX", msg))
	if err != nil || string(got) != raw || !msg.Date.Equal(date) {
		t.Fatalf("append: %q, %v, %v", got, msg.Date, err)
	}
	delivery := filepath.Join(accountPath(root), mailboxPath("INBOX"), "new", "external:2,FS")
	if err := os.WriteFile(delivery, []byte("Subject: external\n\nline1\nline2\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(delivery, date, date); err != nil {
		t.Fatal(err)
	}
	if err := s.account.reconcile(); err != nil {
		t.Fatal(err)
	}
	msg = s.account.state.Mailboxes["INBOX"].Messages[1]
	got, err = readFile(s.account.root, messagePath("INBOX", msg))
	want := "Subject: external\r\n\r\nline1\r\nline2\r\n"
	if err != nil || string(got) != want || msg.Size != int64(len(want)) || !msg.Date.Equal(date) || !imap.ContainsFlag(msg.Flags, imap.FlagSeen) {
		t.Fatalf("delivery: %#v, %q, %v", msg, got, err)
	}
	if _, err := os.Stat(delivery); !os.IsNotExist(err) {
		t.Fatal("delivery not consumed", err)
	}
}

func TestLargeAppendAndFetchStream(t *testing.T) {
	if testing.Short() {
		t.Skip("200 MiB streaming fixture")
	}
	ctx := context.Background()
	b := openTest(t, t.TempDir())
	s := loginTest(t, b)
	const size = 200 << 20
	reader := io.MultiReader(strings.NewReader("Subject: large\r\n\r\n"), io.LimitReader(zeroReader{}, size))
	data, err := s.Append(ctx, "INBOX", reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	sel := selectTest(t, s, "INBOX")
	var copied int64
	writer := &imapserver.FetchWriter{WriteFunc: func(ctx context.Context, data *imap.FetchMessageData) error {
		for _, value := range data.Items["BODY[]"] {
			section := value.(*imap.FetchDataBodySection)
			n, err := io.Copy(io.Discard, section.Literal)
			copied += n
			if err != nil {
				return err
			}
		}
		return nil
	}}
	if err := sel.Fetch(ctx, writer, imap.UIDSetNum(data.UID), &imapserver.FetchOptions{Items: []imap.FetchItem{&imap.FetchItemBodySection{Peek: true}}}); err != nil {
		t.Fatal(err)
	}
	if copied != size+int64(len("Subject: large\r\n\r\n")) {
		t.Fatalf("streamed %d bytes", copied)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestSegmentsMatchBackupLayout(t *testing.T) {
	for _, tt := range []struct{ raw, escaped string }{{"cur", "%63ur"}, {"NEW", "%4EEW"}, {"tmp", "%74mp"}, {".", "%2E"}, {"..", "%2E."}, {".hidden", "%2Ehidden"}, {"a/b", "a%2Fb"}, {"%", "%25"}, {"", "%"}, {"A\x00B", "A%00B"}, {"用户", "用户"}} {
		if got := escapeSegment(tt.raw); got != tt.escaped {
			t.Fatalf("%q => %q, want %q", tt.raw, got, tt.escaped)
		}
		got, err := unescapeSegment(tt.escaped)
		if err != nil || got != tt.raw {
			t.Fatalf("roundtrip %q: %q, %v", tt.raw, got, err)
		}
	}
	if got := mailboxPath("a//cur"); got != filepath.Join("mailboxes", "a", "%", "%63ur") {
		t.Fatal(got)
	}
}

func TestSymlinksRefused(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	path := filepath.Join(accountPath(root), mailboxPath("INBOX"), "new", "link")
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := s.account.reconcile(); err == nil {
		t.Fatal("delivery symlink accepted")
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if next, err := Open(ctx, root, testOptions()); err == nil {
		next.Close(ctx, nil)
		t.Fatal("symlink accepted on reopen")
	}
}

func TestFlagFilenameAndPersistentKeyword(t *testing.T) {
	flags := []imap.Flag{imap.FlagDeleted, imap.FlagSeen, imap.FlagAnswered, imap.FlagFlagged, imap.FlagDraft, "keyword"}
	if got := flagSuffix(flags); got != ":2,DFRST" {
		t.Fatal(got)
	}
	parsed := flagsFromFilename("a:2,DFRST")
	if len(parsed) != 5 {
		t.Fatal(parsed)
	}
	for _, flag := range flags[:5] {
		if !slices.Contains(parsed, flag) {
			t.Fatal("missing", flag)
		}
	}
}

// TestCopyAnnouncesNewestMailboxFlags pins the order of mailbox FLAGS updates
// within one batch. Each carries the complete set, and a COPY that introduces
// two keywords used to prepend both, leaving the older, smaller set to be
// applied last: the selected view and its client then lacked a keyword that
// a later FETCH reported (RFC 3501 section 7.2.6).
func TestCopyAnnouncesNewestMailboxFlags(t *testing.T) {
	ctx := context.Background()
	s := loginTest(t, openTest(t, t.TempDir()))
	if err := s.Create(ctx, "Target", nil); err != nil {
		t.Fatal(err)
	}
	first := appendTest(t, s, "INBOX")
	second := appendTest(t, s, "INBOX")
	inbox := selectTest(t, s, "INBOX")
	for uid, keyword := range map[imap.UID]imap.Flag{first.UID: "k1", second.UID: "k2"} {
		if err := inbox.Store(ctx, discardFetch(), imap.UIDSetNum(uid), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{keyword}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	var batches []*imapserver.UpdateBatch
	if _, err := s.Select(ctx, "Target", &imapserver.Updater{PushFunc: func(batch *imapserver.UpdateBatch) error {
		batches = append(batches, batch)
		return nil
	}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := inbox.Copy(ctx, imap.UIDSetNum(first.UID, second.UID), "Target", nil); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Fatalf("COPY published %d batches to the destination, want 1", len(batches))
	}
	var flagUpdates []*imapserver.UpdateMailboxFlags
	for i, change := range batches[0].Changes {
		if update, ok := change.(*imapserver.UpdateMailboxFlags); ok {
			if i != 0 {
				t.Fatalf("mailbox FLAGS update at position %d, want it ahead of every message update", i)
			}
			flagUpdates = append(flagUpdates, update)
		}
	}
	if len(flagUpdates) != 1 || !imap.ContainsFlag(flagUpdates[0].Flags, "k1") || !imap.ContainsFlag(flagUpdates[0].Flags, "k2") {
		t.Fatalf("mailbox FLAGS updates = %v, want one carrying both k1 and k2", flagUpdates)
	}
}

// TestBinaryMetadataSurvivesRewriteAndRestart pins RFC 5464 section 3.2's
// "binary data of zero or more octets". JSON strings cannot carry invalid
// UTF-8, and encoding/json silently replaced it with U+FFFD: the value read
// back correctly until the next state rewrite or restart.
func TestBinaryMetadataSurvivesRewriteAndRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	values := map[imap.MetadataEntryName]string{
		"/private/binary": "bin\xff\xfe\x80end",
		"/private/text":   "line one\r\nline two\x00",
	}
	var entries []imap.MetadataEntry
	for name, value := range values {
		copy := value
		entries = append(entries, imap.MetadataEntry{Name: name, Value: &copy})
	}
	for _, mailbox := range []string{"", "INBOX"} {
		if err := s.SetMetadata(ctx, mailbox, entries, nil); err != nil {
			t.Fatal(err)
		}
	}
	check := func(when string, s *session) {
		t.Helper()
		for _, mailbox := range []string{"", "INBOX"} {
			got, err := s.GetMetadata(ctx, mailbox, []imap.MetadataEntryName{"/private"}, &imapserver.MetadataOptions{Depth: "infinity"})
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Entries) != len(values) {
				t.Fatalf("%s: %q has %d entries, want %d", when, mailbox, len(got.Entries), len(values))
			}
			for _, entry := range got.Entries {
				if entry.Value == nil {
					t.Fatalf("%s: %q %s has no value", when, mailbox, entry.Name)
				}
				if *entry.Value != values[entry.Name] {
					t.Fatalf("%s: %q %s = %q, want %q", when, mailbox, entry.Name, *entry.Value, values[entry.Name])
				}
			}
		}
	}
	// Any later mutation rewrites the whole state through JSON.
	appendTest(t, s, "INBOX")
	check("after a rewrite", s)
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	check("after a restart", loginTest(t, openTest(t, root)))
}

// TestInvalidMetadataEntryNamesAreBad applies RFC 5464 section 3.2: invalid
// entry names "result in a BAD response". A name outside ASCII would also not
// survive as a JSON object key in the stored state.
func TestInvalidMetadataEntryNamesAreBad(t *testing.T) {
	ctx := context.Background()
	s := loginTest(t, openTest(t, t.TempDir()))
	value := "v"
	for _, name := range []imap.MetadataEntryName{"/private/caf\xc3\xa9", "/private/\xff", "/private/a*", "/private/a%", "/private//a", "/private/a/", "/private/a\x19", ""} {
		set := s.SetMetadata(ctx, "INBOX", []imap.MetadataEntry{{Name: name, Value: &value}}, nil)
		_, get := s.GetMetadata(ctx, "INBOX", []imap.MetadataEntryName{name}, nil)
		for _, err := range []error{set, get} {
			var protocol *imap.Error
			if !errors.As(err, &protocol) || protocol.Type != imap.ErrorTypeBad {
				t.Fatalf("entry name %q: error = %v, want BAD", name, err)
			}
		}
	}
	if len(s.account.state.Mailboxes["INBOX"].Metadata) != 0 {
		t.Fatal("an invalid entry name was stored")
	}
}

// TestFetchMarksSeenInOneBatch pins the implicit \Seen of a non-PEEK FETCH to
// one commit and one update batch. One batch per message overflowed every
// selected session's update queue once a FETCH covered more messages than
// Limits.MaxQueuedUpdates, since the framework drains only after the FETCH.
func TestFetchMarksSeenInOneBatch(t *testing.T) {
	ctx := context.Background()
	s := loginTest(t, openTest(t, t.TempDir()))
	const count = 5
	for range count {
		appendTest(t, s, "INBOX")
	}
	var batches []*imapserver.UpdateBatch
	if _, err := s.Select(ctx, "INBOX", &imapserver.Updater{PushFunc: func(batch *imapserver.UpdateBatch) error {
		batches = append(batches, batch)
		return nil
	}}, nil); err != nil {
		t.Fatal(err)
	}
	inbox := selectTest(t, s, "INBOX")
	all := imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}

	// An unsupported item fails before anything is marked.
	unsupported := []imap.FetchItem{&imap.FetchItemBodySection{}, imap.FetchItemKeyword("X-UNSUPPORTED")}
	if err := inbox.Fetch(ctx, discardFetch(), all, &imapserver.FetchOptions{Items: unsupported}); err == nil {
		t.Fatal("unsupported FETCH item accepted")
	}
	if len(batches) != 0 {
		t.Fatalf("a refused FETCH published %d batches", len(batches))
	}

	var fetched []*imap.FetchMessageData
	writer := &imapserver.FetchWriter{WriteFunc: func(_ context.Context, data *imap.FetchMessageData) error {
		fetched = append(fetched, data)
		return nil
	}}
	items := []imap.FetchItem{imap.FetchItemFlags, &imap.FetchItemBodySection{}}
	if err := inbox.Fetch(ctx, writer, all, &imapserver.FetchOptions{Items: items}); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || len(batches[0].Changes) != count {
		t.Fatalf("FETCH of %d unseen messages published %d batches, want one with %d changes", count, len(batches), count)
	}
	if len(fetched) != count {
		t.Fatalf("FETCH wrote %d messages, want %d", len(fetched), count)
	}
	for _, data := range fetched {
		flags, _ := data.Items["FLAGS"][0].(imap.FetchDataFlags)
		if !imap.ContainsFlag([]imap.Flag(flags), imap.FlagSeen) {
			t.Fatalf("FETCH response FLAGS %v lack the \\Seen it set", flags)
		}
	}
	for _, msg := range s.account.state.Mailboxes["INBOX"].Messages {
		if !imap.ContainsFlag(msg.Flags, imap.FlagSeen) || !strings.HasSuffix(msg.File, "S") {
			t.Fatalf("message %d not durably \\Seen: %v %q", msg.UID, msg.Flags, msg.File)
		}
	}

	// Already \Seen: nothing to commit, nothing to publish.
	if err := inbox.Fetch(ctx, discardFetch(), all, &imapserver.FetchOptions{Items: items}); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Fatalf("FETCH of already-seen messages published %d more batches", len(batches)-1)
	}
}

// TestVanishedMessageFileIsNotJournalled pins the recovery hazard from the
// T32 storage review. A STORE on a message whose cur/ file was removed outside
// the backend journalled a rename that replay can never complete, and because
// Open fails if any account fails to load, every later Open of the root failed.
func TestVanishedMessageFileIsNotJournalled(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	appended := appendTest(t, s, "INBOX")
	inbox := selectTest(t, s, "INBOX")
	msg := s.account.state.Mailboxes["INBOX"].Messages[0]
	if err := os.Remove(filepath.Join(accountPath(root), messagePath("INBOX", msg))); err != nil {
		t.Fatal(err)
	}
	err := inbox.Store(ctx, discardFetch(), imap.UIDSetNum(appended.UID), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil)
	var protocol *imap.Error
	if !errors.As(err, &protocol) || protocol.Code != imap.CodeExpungeIssued {
		t.Fatalf("STORE on a vanished message: %v, want NO [EXPUNGEISSUED]", err)
	}
	if _, err := os.Stat(filepath.Join(accountPath(root), journalFile)); !os.IsNotExist(err) {
		t.Fatalf("a journal was left behind: %v", err)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	s = loginTest(t, openTest(t, root))
	if n := len(s.account.state.Mailboxes["INBOX"].Messages); n != 0 {
		t.Fatalf("reopened INBOX has %d messages, want the vanished one reconciled away", n)
	}
}
