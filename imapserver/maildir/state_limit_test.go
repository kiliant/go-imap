package maildir

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestReadStateRejectsOversizedFile(t *testing.T) {
	path := t.TempDir()
	file, err := os.Create(filepath.Join(path, stateFile))
	if err != nil {
		t.Fatal(err)
	}
	// A sparse file exercises the actual production limit without retaining a
	// huge fixture. Oversized files should be rejected before reading payload.
	if err := file.Truncate((256 << 20) + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	_, err = readFile(root, stateFile)
	var protocol *imap.Error
	if !errors.As(err, &protocol) || protocol.Code != imap.CodeLimit {
		t.Fatalf("oversized file read = %v, want LIMIT", err)
	}
}

func TestOversizedCommitDoesNotPrepareJournal(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	b := openTest(t, path)
	s := loginTest(t, b)
	message := appendTest(t, s, "INBOX")
	validity := s.account.state.Mailboxes["INBOX"].UIDValidity
	func() {
		a := s.account
		if err := a.enter(ctx); err != nil {
			t.Fatal(err)
		}
		defer a.leave()
		next, err := cloneState(a.state)
		if err != nil {
			t.Fatal(err)
		}
		m := next.Mailboxes["INBOX"]
		file := messagePath("INBOX", m.Messages[0])
		m.Messages = nil
		m.HighestModSeq++
		m.Tombstones = append(m.Tombstones, tombstone{UID: message.UID, ModSeq: m.HighestModSeq})
		m.Metadata["/private/large"] = metadataValue(strings.Repeat("x", 128))
		actions := []fileAction{{Kind: "remove", From: file}}
		raw, err := json.Marshal(diskJournal{Version: stateVersion, State: next, Actions: actions})
		if err != nil {
			t.Fatal(err)
		}
		err = a.commitWithLimit(next, actions, int64(len(raw)-1))
		var protocol *imap.Error
		if !errors.As(err, &protocol) || protocol.Code != imap.CodeLimit {
			t.Fatalf("oversized commit = %v, want LIMIT", err)
		}
		if a.failure != nil || len(a.state.Mailboxes["INBOX"].Messages) != 1 {
			t.Fatal("size refusal poisoned or changed the account")
		}
		if _, err := a.root.Stat(file); err != nil {
			t.Fatalf("size refusal applied a file action: %v", err)
		}
		if _, err := a.root.Stat(journalFile); !os.IsNotExist(err) {
			t.Fatalf("size refusal prepared a journal: %v", err)
		}
	}()
	appendTest(t, s, "INBOX") // a later ordinary mutation must remain possible
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, path)
	m := b.accounts["alice"].state.Mailboxes["INBOX"]
	if m.UIDValidity != validity || len(m.Messages) != 2 || m.Messages[0].UID != message.UID {
		t.Fatalf("size refusal changed durable message identities: %#v", m)
	}
	if _, present := m.Metadata["/private/large"]; present {
		t.Fatal("refused metadata appeared after reopening")
	}
}

func TestStateWriteSizeBoundary(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			b := openTest(t, t.TempDir())
			a := b.accounts["alice"]
			if err := a.enter(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer a.leave()
			next, err := cloneState(a.state)
			if err != nil {
				t.Fatal(err)
			}
			next.Metadata["/private/boundary"] = "fits"
			raw, err := json.Marshal(diskJournal{Version: stateVersion, State: next})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.commitWithLimit(next, nil, int64(len(raw)+extra)); err != nil {
				t.Fatalf("commit at/below the serialized budget failed: %v", err)
			}
			if a.state.Metadata["/private/boundary"] != "fits" {
				t.Fatal("commit at/below the budget did not publish its state")
			}
		})
	}
}

func TestAtomicStateWriteRefusesOversizeBeforeReplacing(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := atomicFileWithLimit(root, stateFile, []byte("old"), 3); err != nil {
		t.Fatal(err)
	}
	err = atomicFileWithLimit(root, stateFile, []byte("new!"), 3)
	var protocol *imap.Error
	if !errors.As(err, &protocol) || protocol.Code != imap.CodeLimit {
		t.Fatalf("oversized atomic replacement = %v, want LIMIT", err)
	}
	raw, err := readFile(root, stateFile)
	if err != nil || string(raw) != "old" {
		t.Fatalf("refused replacement changed existing bytes: %q, %v", raw, err)
	}
	entries, err := readDir(root, ".")
	if err != nil || len(entries) != 1 || entries[0].Name() != stateFile {
		t.Fatalf("refused replacement left temporary files: %v, %v", entries, err)
	}
}
