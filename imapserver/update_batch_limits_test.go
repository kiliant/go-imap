package imapserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

const bulkUpdateCount = 45_000

type bulkUpdateBackend struct {
	expunge bool
	updater chan *Updater
}

func TestCompactUpdateBatchPreservesOrderAndAccounting(t *testing.T) {
	for _, effect := range []commandEffect{effectNone, effectStore, effectExpunge, effectMoveOut} {
		t.Run(fmt.Sprint(effect), func(t *testing.T) {
			batch := &UpdateBatch{Before: "r1", After: "r2", Origin: 7, Changes: []Update{
				&UpdateAdd{UIDs: []imap.UID{3}},
				&UpdateAdd{UIDs: []imap.UID{4}},
				&UpdateFlags{UID: 3, Flags: []imap.Flag{imap.FlagSeen}, ModSeq: 9},
				&UpdateExpunge{UID: 1},
				&UpdateExpunge{UID: 2},
				&UpdateAdd{UIDs: []imap.UID{5}},
				&UpdateAdd{UIDs: []imap.UID{6}},
				&UpdateExpunge{UID: 5},
				&UpdateExpunge{UID: 4},
			}}
			accounting := updateAccounting{origin: 7, effect: effect}
			before := &selectedState{uids: []imap.UID{1, 2}, revision: "r1"}
			want, err := before.applyBatch(batch, accounting)
			if err != nil {
				t.Fatal(err)
			}
			queue := newUpdateQueue(1, 1024, nil)
			if err := newUpdater(queue).Push(batch); err != nil {
				t.Fatal(err)
			}
			// Backend-owned inputs may be reused as soon as Push returns.
			batch.Before, batch.After, batch.Origin = "bad", "bad", 99
			for _, change := range batch.Changes {
				switch change := change.(type) {
				case *UpdateAdd:
					change.UIDs[0] = 99
				case *UpdateExpunge:
					change.UID = 99
				case *UpdateFlags:
					change.Flags[0] = imap.FlagAnswered
				}
			}
			if got := queue.popWhile(func(b *UpdateBatch) bool { return !batchRemovesMessages(b) }); len(got) != 0 {
				t.Fatal("compaction allowed removals past a deferral barrier")
			}
			retained := queue.popThroughOrigin(7)
			if len(retained) != 1 || queue.bytes != 0 {
				t.Fatalf("atomic batch/accounting changed: %d batches, %d bytes remain", len(retained), queue.bytes)
			}
			if cap(retained[0].Changes) != len(retained[0].Changes) {
				t.Fatal("compact batch retained the original interface-slice capacity")
			}
			after := &selectedState{uids: []imap.UID{1, 2}, revision: "r1"}
			got, err := after.applyBatch(retained[0], accounting)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(coalesceWireUpdates(got), coalesceWireUpdates(want)) ||
				!slices.Equal(before.uids, after.uids) || before.revision != after.revision {
				t.Fatalf("compaction changed ordered delivery: got %#v/%v, want %#v/%v", got, after.uids, want, before.uids)
			}
		})
	}
}

func TestCompactUpdateBatchStillBoundsBacklogPayload(t *testing.T) {
	var nilAdd *UpdateAdd
	var nilFlags *UpdateFlags
	var nilMailboxFlags *UpdateMailboxFlags
	var nilVanished *UpdateVanished
	for _, change := range []Update{
		&UpdateAdd{UIDs: make([]imap.UID, 64)},
		&queuedExpunges{uids: make([]imap.UID, 64)},
		nilAdd, nilFlags, nilMailboxFlags, nilVanished,
	} {
		t.Run(fmt.Sprintf("%T/nil=%t", change, reflect.ValueOf(change).IsNil()), func(t *testing.T) {
			changes := []Update{change}
			if reflect.ValueOf(change).IsNil() {
				changes = make([]Update, 20)
				for i := range changes {
					changes[i] = change
				}
			}
			overflowed := false
			queue := newUpdateQueue(2, 256, func() { overflowed = true })
			if err := newUpdater(queue).Push(&UpdateBatch{Before: "r0", After: "r1"}); err != nil {
				t.Fatal(err)
			}
			err := newUpdater(queue).Push(&UpdateBatch{Before: "r1", After: "r2", Changes: changes})
			if err != nil || overflowed || len(queue.items) != 2 {
				t.Fatalf("budget plus one atomic batch: %v, overflow=%t", err, overflowed)
			}
			if err := newUpdater(queue).Push(&UpdateBatch{Before: "r2", After: "r3"}); !errors.Is(err, ErrUpdateOverflow) || !overflowed {
				t.Fatalf("unbounded backlog: %v, overflow=%t", err, overflowed)
			}
		})
	}
}

func TestCompactUpdateBatchRetainsMalformedChanges(t *testing.T) {
	var nilExpunge *UpdateExpunge
	for _, changes := range [][]Update{
		{&UpdateAdd{UIDs: []imap.UID{3}}, &UpdateAdd{}, &UpdateAdd{UIDs: []imap.UID{4}}},
		{&UpdateAdd{UIDs: []imap.UID{3}}, (*UpdateAdd)(nil), &UpdateAdd{UIDs: []imap.UID{4}}},
		{&UpdateExpunge{UID: 1}, nilExpunge, &UpdateExpunge{UID: 2}},
		{&UpdateExpunge{UID: 1}, &UpdateExpunge{UID: 99}},
		{&UpdateExpunge{UID: 1}, &UpdateExpunge{UID: 1}},
	} {
		batch := cloneUpdateBatch(&UpdateBatch{Before: "r1", After: "r2", Changes: changes})
		state := &selectedState{uids: []imap.UID{1, 2}, revision: "r1"}
		if _, err := state.applyBatch(batch, updateAccounting{}); !errors.Is(err, ErrRevisionMismatch) {
			t.Fatalf("compaction concealed malformed changes %#v: %v", changes, err)
		}
	}
}

func (b *bulkUpdateBackend) Authenticate(context.Context, *ConnInfo, *Credentials, *AuthenticateOptions) (Session, error) {
	return &bulkUpdateSession{backend: b}, nil
}

type bulkUpdateSession struct {
	stubSession
	backend *bulkUpdateBackend
}

func (s *bulkUpdateSession) Select(_ context.Context, _ string, updater *Updater, _ *SelectOptions) (*SelectResult, error) {
	var uids []imap.UID
	if s.backend.expunge {
		uids = make([]imap.UID, bulkUpdateCount)
		for i := range uids {
			uids[i] = imap.UID(i + 1)
		}
	}
	s.backend.updater <- updater
	return &SelectResult{
		Mailbox: &stubSelectedMailbox{},
		Snapshot: SelectSnapshot{
			UIDs: uids, Revision: "r1",
			Status: imap.MailboxStatus{UIDValidity: 1, UIDNext: imap.UID(len(uids) + 1), NumMessages: uint32(len(uids)), NoModSeq: true},
		},
	}, nil
}

func TestBulkUpdatesFitDefaultQueue(t *testing.T) {
	for _, expunge := range []bool{false, true} {
		t.Run(fmt.Sprintf("expunge=%t", expunge), func(t *testing.T) {
			backend := &bulkUpdateBackend{expunge: expunge, updater: make(chan *Updater, 1)}
			server := New(backend, &Options{AllowInsecureAuth: true})
			serverSide, clientSide := net.Pipe()
			defer clientSide.Close()
			if err := clientSide.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- server.ServeConn(ctx, serverSide, nil) }()
			reader := bufio.NewReader(clientSide)
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			writeUpdateOrderCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
			readUpdateOrderTag(t, reader, "A1 OK ")
			writeUpdateOrderCommand(t, clientSide, "A2 SELECT INBOX\r\n")
			readUpdateOrderTag(t, reader, "A2 OK ")
			updater := <-backend.updater
			changes := make([]Update, bulkUpdateCount)
			for i := range changes {
				if expunge {
					changes[i] = &UpdateExpunge{UID: imap.UID(bulkUpdateCount - i)}
				} else {
					changes[i] = &UpdateAdd{UIDs: []imap.UID{imap.UID(i + 1)}}
				}
			}
			if err := updater.Push(&UpdateBatch{Before: "r1", After: "r2", Changes: changes}); err != nil {
				t.Fatalf("one bulk commit disconnected the selection: %v", err)
			}
			if expunge {
				requireNoResponseBetweenCommands(t, clientSide, reader)
			} else if line, err := reader.ReadString('\n'); err != nil || line != fmt.Sprintf("* %d EXISTS\r\n", bulkUpdateCount) {
				t.Fatalf("bulk delivery notification = %q, %v", line, err)
			}
			writeUpdateOrderCommand(t, clientSide, "A3 NOOP\r\n")
			removed := 0
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(line, "A3 OK ") {
					break
				}
				if !expunge || line != fmt.Sprintf("* %d EXPUNGE\r\n", bulkUpdateCount-removed) {
					t.Fatalf("bulk update response %d = %q", removed, line)
				}
				removed++
			}
			if expunge && removed != bulkUpdateCount {
				t.Fatalf("delivered %d removals, want %d", removed, bulkUpdateCount)
			}
			writeUpdateOrderCommand(t, clientSide, "A4 LOGOUT\r\n")
			readUpdateOrderTag(t, reader, "A4 OK ")
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
