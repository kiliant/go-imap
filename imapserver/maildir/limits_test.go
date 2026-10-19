package maildir

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func TestMetadataStorageLimitsAreAtomicAndRecoverable(t *testing.T) {
	ctx := context.Background()
	for _, scope := range []string{"INBOX", ""} {
		t.Run(scope, func(t *testing.T) {
			root := t.TempDir()
			opts := testOptions()
			opts.MaxMetadataValueBytes = 1024
			opts.MaxMetadataEntries = 10
			b, err := Open(ctx, root, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close(ctx, nil)
			s := loginTest(t, b)
			value := strings.Repeat("x", 1024)
			var entries []imap.MetadataEntry
			for i := 0; i < 10; i++ {
				entries = append(entries, imap.MetadataEntry{Name: imap.MetadataEntryName(fmt.Sprintf("/private/e%d", i)), Value: &value})
			}
			if err := s.SetMetadata(ctx, scope, entries, nil); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(accountPath(root), stateFile))
			if err != nil {
				t.Fatal(err)
			}
			tooLarge := value + "x"
			for _, tt := range []struct {
				entries []imap.MetadataEntry
				code    string
			}{
				{[]imap.MetadataEntry{{Name: "/private/e0"}, {Name: "/private/e1", Value: &tooLarge}}, "MAXSIZE 1024"},
				{[]imap.MetadataEntry{{Name: "/private/new", Value: &value}}, "TOOMANY"},
			} {
				var protocol *imap.Error
				if err := s.SetMetadata(ctx, scope, tt.entries, nil); !errors.As(err, &protocol) || protocol.Code != imap.CodeMetadata || protocol.CodeArgs != tt.code {
					t.Fatalf("quota error=%v, want METADATA %s", err, tt.code)
				}
				after, err := os.ReadFile(filepath.Join(accountPath(root), stateFile))
				if err != nil || string(before) != string(after) {
					t.Fatalf("refusal changed state: %v", err)
				}
				if _, err := os.Stat(filepath.Join(accountPath(root), journalFile)); !os.IsNotExist(err) {
					t.Fatalf("refusal prepared journal: %v", err)
				}
			}
			if err := s.SetMetadata(ctx, scope, []imap.MetadataEntry{{Name: "/private/e0"}, {Name: "/private/new", Value: &value}}, nil); err != nil {
				t.Fatal(err)
			}
			if err := b.Close(ctx, nil); err != nil {
				t.Fatal(err)
			}
			b, err = Open(ctx, root, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close(ctx, nil)
			got, err := loginTest(t, b).GetMetadata(ctx, scope, []imap.MetadataEntryName{"/private"}, &imapserver.GetMetadataOptions{Depth: "infinity"})
			if err != nil || len(got.Entries) != 10 {
				t.Fatalf("quota boundary did not survive reopen: %v, %+v", err, got)
			}
		})
	}
}

func TestPrunedExpungeHistorySurvivesRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	opts := testOptions()
	opts.MaxTombstones = 2
	b, err := Open(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx, nil)
	s := loginTest(t, b)
	sel := selectTest(t, s, "INBOX")
	for i := 0; i < 5; i++ {
		appendTest(t, s, "INBOX")
	}
	for _, uid := range []imap.UID{1, 3, 4} {
		if err := sel.Store(ctx, discardFetch(), imap.UIDSetNum(uid), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil); err != nil {
			t.Fatal(err)
		}
		if err := sel.Expunge(ctx, discardExpunge(), ptrUIDSet(uid), nil); err != nil {
			t.Fatal(err)
		}
	}
	m := s.account.state.Mailboxes["INBOX"]
	floor := m.TombstoneFloor
	validity := m.UIDValidity
	if len(m.Tombstones) != 2 || floor == 0 {
		t.Fatalf("unbounded history: %+v", m)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	b, err = Open(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx, nil)
	s = loginTest(t, b)
	sel = selectTest(t, s, "INBOX")
	for _, tt := range []struct {
		modseq uint64
		known  imap.UIDSet
		want   imap.UIDSet
	}{
		{floor - 1, nil, imap.UIDSetNum(1, 3, 4)},
		{floor, nil, imap.UIDSetNum(3, 4)},
		{floor - 1, imap.UIDSetRange(2, 4), imap.UIDSetNum(3, 4)},
		{math.MaxUint64, nil, nil},
	} {
		got, err := sel.Resync(ctx, &imapserver.QResyncSelect{UIDValidity: validity, ModSeq: tt.modseq, KnownUIDs: tt.known}, nil)
		if err != nil || !got.Vanished.Equal(tt.want) {
			t.Fatalf("resync at %d: %+v, %v, want %v", tt.modseq, got, err, tt.want)
		}
	}
	got, err := sel.Resync(ctx, &imapserver.QResyncSelect{UIDValidity: validity, ModSeq: floor - 1, SeqMatchSeqNums: imap.SeqSetNum(1), SeqMatchUIDs: imap.UIDSetNum(2)}, nil)
	if err != nil || !got.Vanished.Equal(imap.UIDSetNum(3, 4)) {
		t.Fatalf("known sequence match did not restrict old history: %+v, %v", got, err)
	}
	if m := s.account.state.Mailboxes["INBOX"]; m.TombstoneFloor != floor || len(m.Tombstones) != 2 {
		t.Fatal("history checkpoint lost on reopen")
	}
}

func ptrUIDSet(uid imap.UID) *imap.UIDSet { set := imap.UIDSetNum(uid); return &set }

func TestConservativeHistoryUsesRanges(t *testing.T) {
	m := &mailboxState{UIDNext: math.MaxUint32, Messages: []*messageRecord{{UID: 2}, {UID: math.MaxUint32 - 2}}}
	want := imap.UIDSet{{Start: 1, Stop: 1}, {Start: 3, Stop: math.MaxUint32 - 3}, {Start: math.MaxUint32 - 1, Stop: math.MaxUint32 - 1}}
	if got := missingUIDs(m); !reflect.DeepEqual(got, want) {
		t.Fatalf("missing UID gaps: %v", got)
	}
	known := imap.UIDSet{{Start: 9, Stop: 3}, {Start: math.MaxUint32 - 1, Stop: math.MaxUint32}}
	if got := intersectUIDs(want, known); !got.Equal(imap.UIDSet{{Start: 3, Stop: 9}, {Start: math.MaxUint32 - 1, Stop: math.MaxUint32 - 1}}) {
		t.Fatalf("scoped UID gaps: %v", got)
	}
}

func TestMetadataLimitsCanBeLoweredWithoutLosingExistingValues(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	large := strings.Repeat("x", 2048)
	var entries []imap.MetadataEntry
	for i := 0; i < 12; i++ {
		entries = append(entries, imap.MetadataEntry{Name: imap.MetadataEntryName(fmt.Sprintf("/private/e%d", i)), Value: &large})
	}
	if err := s.SetMetadata(ctx, "INBOX", entries, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	opts := testOptions()
	opts.MaxMetadataEntries = 10
	opts.MaxMetadataValueBytes = 1024
	b, err := Open(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx, nil)
	s = loginTest(t, b)
	got, err := s.GetMetadata(ctx, "INBOX", []imap.MetadataEntryName{"/private/e0"}, nil)
	if err != nil || len(got.Entries) != 1 || *got.Entries[0].Value != large {
		t.Fatalf("lowered limit lost existing value: %+v, %v", got, err)
	}
	for _, name := range []imap.MetadataEntryName{"/private/e0", "/private/e1", "/private/e2"} {
		if err := s.SetMetadata(ctx, "INBOX", []imap.MetadataEntry{{Name: name}}, nil); err != nil {
			t.Fatalf("quota prevented reducing existing state: %v", err)
		}
	}
	small := "replacement"
	if err := s.SetMetadata(ctx, "INBOX", []imap.MetadataEntry{{Name: "/private/new", Value: &small}}, nil); err != nil {
		t.Fatal(err)
	}
}
