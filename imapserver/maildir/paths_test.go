package maildir

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kiliant/go-imap"
)

func filesystemAliases(t *testing.T, root, first, second string) bool {
	t.Helper()
	dir, err := os.MkdirTemp(root, "alias-probe-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Mkdir(filepath.Join(dir, first), 0700); err != nil {
		t.Fatal(err)
	}
	a, err := os.Stat(filepath.Join(dir, first))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(filepath.Join(dir, second))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(a, b)
}

func TestMailboxFilesystemAliases(t *testing.T) {
	for _, pair := range [][2]string{{"Archive", "archive"}, {"caf\u00e9", "cafe\u0301"}, {"cur", "cUR"}} {
		for _, operation := range []string{"create", "create-child", "rename", "rename-inbox", "rename-descendant"} {
			t.Run(pair[0]+"/"+operation, func(t *testing.T) {
				ctx := context.Background()
				root := t.TempDir()
				aliases := filesystemAliases(t, root, escapeSegment(pair[0]), escapeSegment(pair[1]))
				b := openTest(t, root)
				s := loginTest(t, b)
				first, second := pair[0], pair[1]
				if operation == "rename-descendant" {
					first, second = "Target/"+first, "Source/"+second
				}
				if err := s.Create(ctx, first, nil); err != nil {
					t.Fatal(err)
				}
				appendTest(t, s, first)
				if operation == "rename-descendant" {
					if err := s.Create(ctx, "Source", nil); err != nil {
						t.Fatal(err)
					}
					if err := s.Create(ctx, second, nil); err != nil {
						t.Fatal(err)
					}
					appendTest(t, s, second)
				}
				before, err := cloneState(s.account.state)
				if err != nil {
					t.Fatal(err)
				}
				switch operation {
				case "create":
					err = s.Create(ctx, second, nil)
				case "create-child":
					err = s.Create(ctx, second+"/Child", nil)
				case "rename":
					err = s.Rename(ctx, first, second, nil)
				case "rename-inbox":
					err = s.Rename(ctx, "INBOX", second, nil)
				case "rename-descendant":
					err = s.Rename(ctx, "Source", "Target", nil)
				}
				if aliases {
					var protocolErr *imap.Error
					if !errors.As(err, &protocolErr) || protocolErr.Code != imap.CodeCannot {
						t.Fatalf("alias accepted or wrong error: %v", err)
					}
					after, cloneErr := cloneState(s.account.state)
					if cloneErr != nil || !reflect.DeepEqual(before, after) || s.account.failure != nil {
						t.Fatal("rejection changed state or disabled the account")
					}
					if _, err := s.account.root.Lstat(journalFile); !os.IsNotExist(err) {
						t.Fatalf("rejection prepared a journal: %v", err)
					}
					appendTest(t, s, first)
				} else if err != nil {
					t.Fatalf("distinct filesystem names rejected: %v", err)
				}
				want, err := cloneState(s.account.state)
				if err != nil {
					t.Fatal(err)
				}
				if err := b.Close(ctx, nil); err != nil {
					t.Fatal(err)
				}
				b = openTest(t, root)
				s = loginTest(t, b)
				if !reflect.DeepEqual(want, s.account.state) {
					t.Fatal("mailbox identities/messages changed on restart")
				}
			})
		}
	}
}

func TestOpenRefusesAliasedStateUnchanged(t *testing.T) {
	for _, scenario := range []string{"state", "missing-directories", "journal"} {
		t.Run(scenario, func(t *testing.T) { testOpenRefusesAliasedState(t, scenario) })
	}
}

func testOpenRefusesAliasedState(t *testing.T, scenario string) {
	ctx := context.Background()
	root := t.TempDir()
	if !filesystemAliases(t, root, "Archive", "archive") {
		t.Skip("filesystem distinguishes case aliases; covered by macOS CI")
	}
	b := openTest(t, root)
	s := loginTest(t, b)
	if err := s.Create(ctx, "Archive", nil); err != nil {
		t.Fatal(err)
	}
	appendTest(t, s, "Archive")
	m, err := s.account.newMailbox()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate state written by the earlier implementation, where both logical
	// names could refer to one physical directory.
	s.account.state.Mailboxes["archive"] = m
	raw, err := json.Marshal(s.account.state)
	if err != nil {
		t.Fatal(err)
	}
	refusedFile := stateFile
	if scenario == "journal" {
		refusedFile = journalFile
		raw, err = json.Marshal(diskJournal{Version: stateVersion, State: s.account.state, Actions: []fileAction{{Kind: "remove", From: filepath.Join(mailboxPath("Archive"), "cur")}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := atomicFile(s.account.root, refusedFile, raw); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if scenario == "missing-directories" {
		// Neither state path exists yet: only the filesystem probe can detect
		// that recreating this proposed namespace would share a directory.
		if err := os.RemoveAll(filepath.Join(accountPath(root), "mailboxes")); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := Open(ctx, root, testOptions()); err == nil {
		b.Close(ctx, nil)
		t.Fatal("aliased state accepted")
	}
	got, err := os.ReadFile(filepath.Join(accountPath(root), refusedFile))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("refused state changed: %v", err)
	}
	if scenario == "journal" {
		entries, err := os.ReadDir(filepath.Join(accountPath(root), mailboxPath("Archive"), "cur"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("aliased journal replayed: %v", err)
		}
	}
}
