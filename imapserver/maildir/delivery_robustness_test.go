package maildir

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBadNewEntriesDoNotBlockDeliveryOrOpen(t *testing.T) {
	root := t.TempDir()
	b := openTest(t, root)
	if err := b.Close(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(accountPath(root), mailboxPath("INBOX"), "new")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "a-symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "b-directory"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "z-valid"), []byte(sampleMessage), 0600); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, root)
	m := b.accounts["alice"].state.Mailboxes["INBOX"]
	if len(m.Messages) != 1 || m.UIDNext != 2 {
		t.Fatalf("good delivery blocked or bad entry ingested: %#v", m)
	}
	if b.LastPollError() == nil {
		t.Error("bad delivery was not recorded")
	}
	for _, name := range []string{"a-symlink", "b-directory"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("bad entry %s was removed: %v", name, err)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "outside" {
		t.Fatalf("symlink target changed: %q, %v", data, err)
	}
}
