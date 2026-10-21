package imapserver

import (
	"errors"
	"reflect"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestDeferredRemovalsValidateBackendAndPreserveClientView(t *testing.T) {
	for _, vanished := range []bool{false, true} {
		t.Run(map[bool]string{false: "expunge", true: "vanished"}[vanished], func(t *testing.T) {
			queue := newUpdateQueue(4, 4096, nil)
			s := &selectedState{uids: []imap.UID{1, 2, 3}, revision: "r1", queue: queue}
			var removal Update = &UpdateExpunge{UID: 1}
			if vanished {
				removal = &UpdateVanished{UIDs: imap.UIDSetNum(1)}
			}
			updates, err := s.applyBatchDeferringRemovals(&UpdateBatch{Before: "r1", After: "r2", Changes: []Update{
				removal, &UpdateAdd{UIDs: []imap.UID{4}}, &UpdateFlags{UID: 3, Flags: []imap.Flag{imap.FlagSeen}},
			}}, updateAccounting{}, true)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(s.uids, []imap.UID{1, 2, 3, 4}) || !reflect.DeepEqual(s.backendUIDs, []imap.UID{2, 3, 4}) || s.revision != "r2" ||
				len(updates) != 2 || updates[0].exists != 4 || updates[1].seqNum != 3 {
				t.Fatalf("views/updates diverged: client %v, backend %v, updates %+v", s.uids, s.backendUIDs, updates)
			}
			if len(s.withheld) != 1 || queue.bytes != 0 {
				t.Fatal("withheld UID must be retained outside the undrained batch budget")
			}
			for _, invalid := range []Update{&UpdateExpunge{UID: 1}, &UpdateFlags{UID: 1}} {
				if _, err := s.applyBatchDeferringRemovals(&UpdateBatch{Before: "r2", After: "r3", Changes: []Update{invalid}}, updateAccounting{}, true); !errors.Is(err, ErrRevisionMismatch) {
					t.Fatalf("accepted update to removed backend UID: %v", err)
				}
			}
			updates, err = s.deliverWithheldRemovals()
			if err != nil || len(updates) != 1 || !reflect.DeepEqual(s.uids, []imap.UID{2, 3, 4}) || queue.bytes != 0 || len(s.withheld) != 0 || s.backendUIDs != nil {
				t.Fatalf("delivery did not reconcile views/budget: %v, %+v, %v", err, updates, s.uids)
			}
		})
	}
}

func TestWithheldRemovalDoesNotConsumeBatchBacklog(t *testing.T) {
	overflowed := false
	q := newUpdateQueue(2, 512, func() { overflowed = true })
	s := &selectedState{uids: []imap.UID{1, 2}, revision: "r1", queue: q}
	if _, err := s.applyBatchDeferringRemovals(&UpdateBatch{Before: "r1", After: "r2", Changes: []Update{&UpdateExpunge{UID: 1}}}, updateAccounting{}, true); err != nil {
		t.Fatal(err)
	}
	if err := q.push(&UpdateBatch{Before: "r2", After: "r3", Changes: []Update{&UpdateFlags{UID: 2}}}); err != nil {
		t.Fatal(err)
	}
	if err := q.push(&UpdateBatch{Before: "r3", After: "r4", Changes: []Update{&UpdateFlags{UID: 2}}}); err != nil {
		t.Fatal(err)
	}
	if err := q.push(&UpdateBatch{Before: "r4", After: "r5"}); !errors.Is(err, ErrUpdateOverflow) || !overflowed {
		t.Fatalf("undrained batch count was not bounded: %v", err)
	}
}
