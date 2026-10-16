package maildir

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	for _, accounts := range [][]AccountOptions{{{Username: ""}}, {{Username: "a"}, {Username: "a"}}} {
		if value, err := Open(ctx, t.TempDir(), &Options{Accounts: accounts}); err == nil {
			value.Close(ctx, nil)
			t.Fatal("invalid accounts accepted")
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
