package maildir

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRenameCollisionDoesNotPrepareJournal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	for _, name := range []string{"Old", "New/Child"} {
		if err := s.Create(ctx, name, nil); err != nil {
			t.Fatal(err)
		}
	}
	appended := appendTest(t, s, "Old")
	for _, name := range []string{"Old", "New"} {
		if err := os.WriteFile(filepath.Join(accountPath(root), mailboxPath(name), ".DS_Store"), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Rename(ctx, "Old", "New", nil); err == nil {
		t.Fatal("RENAME accepted a colliding placeholder file")
	}
	if _, err := os.Stat(filepath.Join(accountPath(root), journalFile)); !os.IsNotExist(err) {
		t.Errorf("failed rename prepared a journal: %v", err)
	}
	if b.accounts["alice"].failure != nil {
		t.Error("failed preflight poisoned the account")
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, root)
	s = loginTest(t, b)
	if m := s.account.state.Mailboxes["Old"]; m == nil || len(m.Messages) != 1 || m.Messages[0].UID != appended.UID {
		t.Errorf("failed rename changed source mailbox: %#v", m)
	}
}
