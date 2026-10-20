package maildir

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func TestIncrementalStoreAndRollback(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	b := openTest(t, path)
	s := loginTest(t, b)
	for range 3 {
		appendTest(t, s, "INBOX")
	}
	a := s.account
	checkpoint, err := readFile(a.root, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	sel := selectTest(t, s, "INBOX")
	old := a.state
	if err := sel.Store(ctx, discardFetch(), imap.UIDSetNum(2), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil); err != nil {
		t.Fatal(err)
	}
	if imap.ContainsFlag(old.Mailboxes["INBOX"].Messages[1].Flags, imap.FlagSeen) {
		t.Fatal("STORE changed the previous state")
	}
	if old.Mailboxes["INBOX"].Messages[0] != a.state.Mailboxes["INBOX"].Messages[0] {
		t.Fatal("unchanged record copied")
	}
	after, err := readFile(a.root, stateFile)
	if err != nil || !bytes.Equal(checkpoint, after) {
		t.Fatalf("STORE replaced checkpoint: %v", err)
	}
	raw, err := readFile(a.root, changeFile(a.state.Sequence))
	if err != nil {
		t.Fatal(err)
	}
	var delta stateChange
	if err := json.Unmarshal(raw, &delta); err != nil {
		t.Fatal(err)
	}
	change := delta.Mailboxes["INBOX"]
	if change == nil || len(change.Upsert) != 1 || change.Upsert[0].UID != 2 || len(change.Remove) != 0 {
		t.Fatalf("STORE delta=%s", raw)
	}
	want, err := cloneState(a.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, path)
	if !reflect.DeepEqual(want, b.accounts["alice"].state) {
		t.Fatal("incremental STORE changed after reopen")
	}
}

func TestIncrementalCompactionAndCheckpointCrash(t *testing.T) {
	ctx := context.Background()
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprint(crash), func(t *testing.T) {
			path := t.TempDir()
			b := openTest(t, path)
			s := loginTest(t, b)
			a := s.account
			for i := 0; i < maxChanges; i++ {
				if err := s.Subscribe(ctx, fmt.Sprintf("box%d", i), nil); err != nil {
					t.Fatal(err)
				}
			}
			if a.changeCount != maxChanges {
				t.Fatalf("change count=%d", a.changeCount)
			}
			injected := errors.New("checkpoint crash")
			if crash {
				b.faultHook = func(stage string) error {
					if stage == "checkpoint" {
						return injected
					}
					return nil
				}
			}
			err := s.Subscribe(ctx, "last", nil)
			if crash && !errors.Is(err, injected) {
				t.Fatalf("checkpoint fault=%v", err)
			}
			if !crash && err != nil {
				t.Fatal(err)
			}
			if !crash && a.changeCount != 1 {
				t.Fatalf("checkpoint did not bound history: %d", a.changeCount)
			}
			if err := b.Close(ctx, nil); err != nil {
				t.Fatal(err)
			}
			b = openTest(t, path)
			a = b.accounts["alice"]
			if len(a.state.Subscriptions) != maxChanges+btoi(!crash) {
				t.Fatalf("subscriptions lost/partly committed: %d", len(a.state.Subscriptions))
			}
			if a.changeCount != 0 {
				t.Fatal("Open did not consolidate changes")
			}
			if err := loginTest(t, b).Subscribe(ctx, "after", nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestIncrementalHistoryLossFailsClosed(t *testing.T) {
	for _, scenario := range []string{"tail", "middle", "head", "future"} {
		t.Run(scenario, func(t *testing.T) {
			path := t.TempDir()
			b := openTest(t, path)
			s := loginTest(t, b)
			for range 3 {
				appendTest(t, s, "INBOX")
			}
			a := s.account
			if err := b.Close(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			file := changeFile(3)
			switch scenario {
			case "tail":
			case "middle":
				file = changeFile(2)
			case "head":
				file = headFile
			case "future":
				raw, err := os.ReadFile(filepath.Join(accountPath(path), file))
				if err != nil {
					t.Fatal(err)
				}
				var change stateChange
				if err := json.Unmarshal(raw, &change); err != nil {
					t.Fatal(err)
				}
				change.Header.Version = stateVersion + 1
				raw, err = json.Marshal(change)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(accountPath(path), file), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "future" {
				if err := os.Remove(filepath.Join(accountPath(path), file)); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(filepath.Join(accountPath(path), stateFile))
			if err != nil {
				t.Fatal(err)
			}
			if next, err := Open(context.Background(), path, testOptions()); err == nil {
				next.Close(context.Background(), nil)
				t.Fatalf("%s history loss accepted; UIDNEXT was %d", scenario, a.state.Mailboxes["INBOX"].UIDNext)
			}
			after, err := os.ReadFile(filepath.Join(accountPath(path), stateFile))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed Open rewrote checkpoint")
			}
		})
	}
}

func TestIncrementalNewRecoveryBoundaries(t *testing.T) {
	for _, stage := range []string{"head", "state"} {
		t.Run(stage, func(t *testing.T) {
			path := t.TempDir()
			b := openTest(t, path)
			s := loginTest(t, b)
			appendTest(t, s, "INBOX")
			validity := s.account.state.Mailboxes["INBOX"].UIDValidity
			b.faultHook = func(at string) error {
				if at == stage {
					return errors.New("crash")
				}
				return nil
			}
			if _, err := s.Append(context.Background(), "INBOX", strings.NewReader(sampleMessage), nil); err == nil {
				t.Fatal("fault not reached")
			}
			if err := b.Close(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			b = openTest(t, path)
			m := b.accounts["alice"].state.Mailboxes["INBOX"]
			if len(m.Messages) != 2 || m.UIDNext != 3 || m.UIDValidity != validity {
				t.Fatalf("partial/doubled transaction: %+v", m)
			}
		})
	}
}

func TestIncrementalReconstructionRetiresOldGeneration(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	b := openTest(t, path)
	s := loginTest(t, b)
	a := s.account
	appendTest(t, s, "INBOX")
	appendTest(t, s, "INBOX")
	old := a.state.Generation
	// Simulate a missing-state reconstruction, interrupted immediately after
	// publishing its new checkpoint and before retiring the old change files.
	fresh, err := a.newMailbox()
	if err != nil {
		t.Fatal(err)
	}
	state := &accountState{Version: stateVersion, Generation: fresh.UIDValidity, Mailboxes: map[string]*mailboxState{"INBOX": fresh}, Subscriptions: map[string]bool{}, Metadata: map[imap.MetadataEntryName]metadataValue{}}
	b.faultHook = func(stage string) error {
		if stage == "checkpoint" {
			return errors.New("crash")
		}
		return nil
	}
	if err := a.checkpoint(state); err == nil {
		t.Fatal("checkpoint fault not reached")
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, path)
	m := b.accounts["alice"].state.Mailboxes["INBOX"]
	if m.UIDValidity == old || m.UIDValidity != fresh.UIDValidity || len(m.Messages) != 2 {
		t.Fatalf("old history replayed into reconstructed state: %+v", m)
	}
}

func TestDirectoryIndexDetectsDeliveryAndRemoval(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	b := openTest(t, path)
	s := loginTest(t, b)
	a := s.account
	appendTest(t, s, "INBOX")
	if err := a.reconcileContext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(a.scanCache) == 0 {
		t.Fatal("idle directory scan was not indexed")
	}
	dir := filepath.Join(accountPath(path), mailboxPath("INBOX"), "new")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "delivery"), []byte(sampleMessage), 0600); err != nil {
		t.Fatal(err)
	}
	// A restored mtime must not conceal a directory entry change.
	if err := os.Chtimes(dir, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcileContext(ctx); err != nil {
		t.Fatal(err)
	}
	m := a.state.Mailboxes["INBOX"]
	if len(m.Messages) != 2 {
		t.Fatal("cached poll missed delivery")
	}
	if err := a.reconcileContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.root.Remove(messagePath("INBOX", m.Messages[1])); err != nil {
		t.Fatal(err)
	}
	if err := a.reconcileContext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(a.state.Mailboxes["INBOX"].Messages) != 1 || a.state.Mailboxes["INBOX"].UIDNext != 3 {
		t.Fatal("cached poll missed removal or reused UID")
	}
}

func TestDirectoryIndexReadsNewOnMatchingStamp(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	b := openTest(t, path)
	a := b.accounts["alice"]
	delivery := filepath.Join(accountPath(path), mailboxPath("INBOX"), "new", "coalesced")
	if err := os.WriteFile(delivery, []byte(sampleMessage), 0600); err != nil {
		t.Fatal(err)
	}
	// Model a filesystem coalescing the delivery's timestamps with the
	// previously cached directory stamps. new/ must be inspected regardless.
	stamp, err := a.directorySnapshot("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	a.scanCache["INBOX"] = stamp
	if err := a.reconcileContext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(a.state.Mailboxes["INBOX"].Messages) != 1 {
		t.Fatal("matching directory stamps concealed delivery")
	}
}

func TestMigrationPublishesVersionBeforeIncrementalDelivery(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	b := openTest(t, path)
	s := loginTest(t, b)
	appendTest(t, s, "INBOX")
	a := s.account
	if err := a.checkpoint(a.state); err != nil {
		t.Fatal(err)
	}
	legacy, err := cloneState(a.state)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Version, legacy.Sequence, legacy.Generation = 3, 0, 0
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicFile(a.root, stateFile, raw); err != nil {
		t.Fatal(err)
	}
	delivery := filepath.Join(accountPath(path), mailboxPath("INBOX"), "new", "migration")
	if err := os.WriteFile(delivery, []byte(sampleMessage), 0600); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("interrupted migration delivery")
	b.faultHook = func(stage string) error {
		if stage != "journal" {
			return nil
		}
		raw, err := readFile(a.root, stateFile)
		if err != nil {
			t.Fatal(err)
		}
		var header struct{ Version int }
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatal(err)
		}
		if header.Version != stateVersion {
			t.Fatal("incremental journal prepared before old-reader refusal barrier")
		}
		return injected
	}
	if err := a.load(); !errors.Is(err, injected) {
		t.Fatalf("migration fault=%v", err)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	b = openTest(t, path)
	m := b.accounts["alice"].state.Mailboxes["INBOX"]
	if len(m.Messages) != 2 || m.UIDNext != 3 || m.UIDValidity != legacy.Mailboxes["INBOX"].UIDValidity {
		t.Fatalf("migration delivery lost/reused identity: %+v", m)
	}
}
