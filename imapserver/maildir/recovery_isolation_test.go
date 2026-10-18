package maildir

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func TestVersion2PreparedJournalMigrates(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	appendTest(t, s, "INBOX")
	sel := selectTest(t, s, "INBOX")
	b.faultHook = func(stage string) error {
		if stage == "journal" {
			return errors.New("interrupted old writer")
		}
		return nil
	}
	if err := sel.Store(ctx, discardFetch(), imap.UIDSetNum(1), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil); err == nil {
		t.Fatal("fault not reached")
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(accountPath(root), journalFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var journal diskJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		t.Fatal(err)
	}
	journal.Version, journal.State.Version = 2, 2
	raw, err = json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, root)
	s = loginTest(t, b)
	if s.account.state.Version != 3 || !imap.ContainsFlag(s.account.state.Mailboxes["INBOX"].Messages[0].Flags, imap.FlagSeen) {
		t.Fatal("old prepared transaction did not migrate and commit")
	}
}

func TestReplayRecordsMessageLostAfterJournalPreparation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	data := appendTest(t, s, "INBOX")
	sel := selectTest(t, s, "INBOX")
	m := s.account.state.Mailboxes["INBOX"]
	validity, clock := m.UIDValidity, m.HighestModSeq
	oldPath := filepath.Join(accountPath(root), messagePath("INBOX", m.Messages[0]))
	b.faultHook = func(stage string) error {
		if stage == "journal" {
			return errors.New("interrupted after preparation")
		}
		return nil
	}
	if err := sel.Store(ctx, discardFetch(), imap.UIDSetNum(data.UID), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil); err == nil {
		t.Fatal("fault did not interrupt STORE")
	}
	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for reopen := 0; reopen < 2; reopen++ {
		b = openTest(t, root)
		s = loginTest(t, b)
		sel = selectTest(t, s, "INBOX")
		m = s.account.state.Mailboxes["INBOX"]
		if m.UIDValidity != validity || m.UIDNext != data.UID+1 || len(m.Messages) != 0 {
			t.Fatalf("recovery changed identity or reused UID: %+v", m)
		}
		result, err := sel.Resync(ctx, &imapserver.QResyncSelect{UIDValidity: validity, ModSeq: clock}, nil)
		if err != nil || !result.Vanished.Contains(data.UID) {
			t.Fatalf("lost message omitted from QRESYNC: %+v, %v", result, err)
		}
		if _, err := os.Stat(filepath.Join(accountPath(root), journalFile)); !os.IsNotExist(err) {
			t.Fatalf("journal did not complete: %v", err)
		}
		if err := b.Close(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	b = openTest(t, root)
	if got := appendTest(t, loginTest(t, b), "INBOX").UID; got <= data.UID {
		t.Fatalf("reused lost UID %d", got)
	}
}

func TestOpenCanIsolateUnrecoverableAccounts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	opts := testOptions()
	opts.Accounts = append(opts.Accounts, AccountOptions{Username: "bob", Password: "other-secret"})
	b, err := Open(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(accountPath(root), stateFile)
	original, err := os.ReadFile(broken)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte("invalid-state"), 0600); err != nil {
		t.Fatal(err)
	}
	if unexpected, err := Open(ctx, root, opts); err == nil {
		unexpected.Close(ctx, nil)
		t.Fatal("default Open accepted corrupt state")
	}
	opts.IsolateAccountErrors = true
	b, err = Open(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx, nil)
	if failures := b.PollFailures(); len(failures) != 1 || failures[0].Username != "alice" || failures[0].Err == nil {
		t.Fatalf("missing isolated-account diagnostic: %+v", failures)
	}
	if _, err := b.Authenticate(ctx, nil, &imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"}, nil); err == nil {
		t.Fatal("failed account accepted login")
	}
	bob, err := b.Authenticate(ctx, nil, &imapserver.Credentials{Mechanism: "PLAIN", Username: "bob", Password: "other-secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Append(ctx, "INBOX", strings.NewReader(sampleMessage), nil); err != nil {
		t.Fatalf("healthy account blocked: %v", err)
	}
	if raw, err := os.ReadFile(broken); err != nil || string(raw) != "invalid-state" {
		t.Fatal("isolation rewrote corrupt state")
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, original, 0600); err != nil {
		t.Fatal(err)
	}
	b, err = Open(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx, nil)
	if len(b.PollFailures()) != 0 {
		t.Fatal("repaired account remained unavailable")
	}
	loginTest(t, b)
}

func TestMissingRegisteredAccountIsIsolatedWithoutReprovisioning(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	path := accountPath(root)
	saved := path + "-saved"
	if err := os.Rename(path, saved); err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.IsolateAccountErrors = true
	b, err := Open(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx, nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing account was silently recreated")
	}
	if len(b.PollFailures()) != 1 {
		t.Fatal("missing account not reported")
	}
}

func TestPollFailureCarriesScopeAndCloseCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	opts := testOptions()
	opts.PollInterval = time.Millisecond
	received := make(chan PollFailure, 1)
	finished := make(chan struct{})
	opts.OnPollFailure = func(ctx context.Context, p *PollFailure) {
		received <- *p
		p.Username = "changed by callback"
		<-ctx.Done()
		close(finished)
	}
	opts.OnPollError = func(error) { t.Error("legacy callback ran despite OnPollFailure") }
	b, err := Open(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx, nil)
	if err := os.Mkdir(filepath.Join(accountPath(root), mailboxPath("INBOX"), "new", "invalid-delivery"), 0700); err != nil {
		t.Fatal(err)
	}
	select {
	case failure := <-received:
		if failure.Username != "alice" || failure.Mailbox != "INBOX" || failure.Err == nil {
			t.Fatalf("incomplete failure: %+v", failure)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if failures := b.PollFailures(); len(failures) != 1 || failures[0].Username != "alice" {
		t.Fatalf("callback mutated retained diagnostics: %+v", failures)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("Close returned before callback completed")
	}
}

func TestSuccessfulPollClearsCurrentFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	opts := testOptions()
	opts.PollInterval = time.Millisecond
	path := filepath.Join(root, fmt.Sprintf("%x", sha256.Sum256([]byte("alice"))), mailboxPath("INBOX"), "new", "invalid")
	b, err := Open(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx, nil)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	for len(b.PollFailures()) == 0 {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for len(b.PollFailures()) != 0 {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if b.LastPollError() == nil {
		t.Fatal("legacy sticky event was cleared")
	}
}
