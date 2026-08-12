// Package backendtest provides a reusable conformance suite for IMAP server
// backends. Backend authors run Run from their own test package with a fresh
// backend factory and optional test-only controls.
package backendtest

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// Controls supplies backend-specific pathological-state hooks to the suite.
// These hooks belong in test code, not a backend's production API.
// Construct with keyed fields only; fields may be added in a future release.
type Controls struct {
	// ForceUIDValidityChange changes one mailbox's UIDVALIDITY without changing
	// its name, simulating recreation or recovery.
	ForceUIDValidityChange func(context.Context, string) error
	// ForceSelectFailure makes Select fail after any tentative updater attachment.
	// Passing false restores normal selection.
	ForceSelectFailure func(context.Context, string, bool) error
	_                  struct{}
}

// Instance is one fresh backend under test.
// Construct with keyed fields only; fields may be added in a future release.
type Instance struct {
	// Backend is the fresh backend value.
	Backend imapserver.Backend
	// Credentials authenticate one writable test account.
	Credentials imapserver.Credentials
	// Controls optionally supplies pathological-state hooks.
	Controls Controls
	_        struct{}
}

// Harness constructs independent backend instances for conformance subtests.
// Construct with keyed fields only; fields may be added in a future release.
type Harness struct {
	// New returns a fresh, isolated instance for one subtest.
	New func() *Instance
	_   struct{}
}

// Run runs the backend conformance suite. It reports configuration failures on
// t rather than panicking so it composes with ordinary Go test output.
func Run(t *testing.T, harness *Harness) {
	t.Helper()
	if harness == nil || harness.New == nil {
		t.Fatal("backendtest: nil harness or factory")
	}
	t.Run("snapshot-consistency", func(t *testing.T) {
		instance, session := newSession(t, harness)
		mailbox := populate(t, session, "snapshot")
		result := selectMailbox(t, session, mailbox, &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { return nil }})
		validateSnapshot(t, result.Snapshot)
		if err := result.Mailbox.Unselect(context.Background()); err != nil {
			t.Fatal(err)
		}
		closeSession(t, instance, session)
	})

	t.Run("atomic-select-and-update-chain", func(t *testing.T) {
		instance, selecting := newSession(t, harness)
		_, mutating := authenticate(t, instance)
		mailbox := populate(t, selecting, "atomic")
		var (
			mu            sync.Mutex
			batches       []*imapserver.UpdateBatch
			notifications = make(chan struct{}, 1)
		)
		updater := &imapserver.Updater{PushFunc: func(batch *imapserver.UpdateBatch) error {
			mu.Lock()
			batches = append(batches, cloneBatch(batch))
			mu.Unlock()
			select {
			case notifications <- struct{}{}:
			default:
			}
			return nil
		}}
		result := selectMailbox(t, selecting, mailbox, updater)
		first := appendMessage(t, mutating, mailbox, "after select one")
		second := appendMessage(t, mutating, mailbox, "after select two")
		got := waitForBatchCount(&mu, &batches, notifications, 2)
		if len(got) != 2 {
			t.Fatalf("update batches = %d, want 2", len(got))
		}
		if got[0].Before != result.Snapshot.Revision || got[1].Before != got[0].After {
			t.Fatalf("update chain = snapshot %q, %q→%q, %q→%q", result.Snapshot.Revision, got[0].Before, got[0].After, got[1].Before, got[1].After)
		}
		if !batchAddsUID(got[0], first.UID) || !batchAddsUID(got[1], second.UID) {
			t.Fatalf("update batches omit appended UIDs %d and %d", first.UID, second.UID)
		}
		if err := result.Mailbox.Unselect(context.Background()); err != nil {
			t.Fatal(err)
		}
		closeSession(t, instance, selecting)
		closeSession(t, instance, mutating)
	})

	t.Run("atomic-select-race", func(t *testing.T) {
		instance, selecting := newSession(t, harness)
		_, mutating := authenticate(t, instance)
		for i := 0; i < 32; i++ {
			mailbox := fmt.Sprintf("backendtest-select-race-%d", i)
			if err := selecting.Create(context.Background(), mailbox, nil); err != nil {
				t.Fatalf("Create iteration %d: %v", i, err)
			}
			var (
				mu            sync.Mutex
				batches       []*imapserver.UpdateBatch
				notifications = make(chan struct{}, 1)
			)
			updater := &imapserver.Updater{PushFunc: func(batch *imapserver.UpdateBatch) error {
				mu.Lock()
				batches = append(batches, cloneBatch(batch))
				mu.Unlock()
				select {
				case notifications <- struct{}{}:
				default:
				}
				return nil
			}}
			type selectOutcome struct {
				result *imapserver.SelectResult
				err    error
			}
			type appendOutcome struct {
				data *imap.AppendData
				err  error
			}
			start := make(chan struct{})
			selected := make(chan selectOutcome, 1)
			appended := make(chan appendOutcome, 1)
			go func() {
				<-start
				result, err := selecting.Select(context.Background(), mailbox, updater, nil)
				selected <- selectOutcome{result: result, err: err}
			}()
			go func() {
				<-start
				raw := []byte(fmt.Sprintf("Subject: select race %d\r\n\r\nbody\r\n", i))
				data, err := mutating.Append(context.Background(), mailbox, bytes.NewReader(raw), nil)
				appended <- appendOutcome{data: data, err: err}
			}()
			close(start)
			selectResult, appendResult := <-selected, <-appended
			if selectResult.err != nil || selectResult.result == nil || selectResult.result.Mailbox == nil {
				t.Fatalf("Select iteration %d = %#v, %v", i, selectResult.result, selectResult.err)
			}
			if appendResult.err != nil || appendResult.data == nil || !appendResult.data.HasUID || appendResult.data.UID == 0 {
				t.Fatalf("Append iteration %d = %#v, %v", i, appendResult.data, appendResult.err)
			}
			validateSnapshot(t, selectResult.result.Snapshot)
			snapshotCount := 0
			if slices.Contains(selectResult.result.Snapshot.UIDs, appendResult.data.UID) {
				snapshotCount = 1
			}
			updateCount := 0
			if snapshotCount == 0 {
				updateCount = waitForAddedUID(&mu, &batches, notifications, appendResult.data.UID)
			} else {
				mu.Lock()
				updateCount = countBatchAddsUID(batches, appendResult.data.UID)
				mu.Unlock()
			}
			if snapshotCount+updateCount != 1 {
				t.Fatalf("iteration %d: appended UID %d appears %d times in snapshot and %d times in updates", i, appendResult.data.UID, snapshotCount, updateCount)
			}
			if updateCount == 1 && (len(batches) == 0 || batches[0].Before != selectResult.result.Snapshot.Revision) {
				t.Fatalf("iteration %d: first update does not start at snapshot revision %q", i, selectResult.result.Snapshot.Revision)
			}
			if err := selectResult.result.Mailbox.Unselect(context.Background()); err != nil {
				t.Fatalf("Unselect iteration %d: %v", i, err)
			}
		}
		closeSession(t, instance, selecting)
		closeSession(t, instance, mutating)
	})

	t.Run("streaming-and-mutations", func(t *testing.T) {
		instance, session := newSession(t, harness)
		mailbox := populate(t, session, "streaming")
		var listed []string
		if err := session.List(context.Background(), &imapserver.ListWriter{WriteFunc: func(_ context.Context, data *imap.ListData) error {
			listed = append(listed, data.Mailbox)
			return nil
		}}, "", []string{"*"}, nil); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(listed, mailbox) {
			t.Fatalf("LIST = %v, want %q", listed, mailbox)
		}
		result := selectMailbox(t, session, mailbox, &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { return nil }})
		var fetched []*imap.FetchMessageData
		fetchWriter := &imapserver.FetchWriter{WriteFunc: func(_ context.Context, data *imap.FetchMessageData) error {
			fetched = append(fetched, data)
			return nil
		}}
		if err := result.Mailbox.Fetch(context.Background(), fetchWriter, imap.UIDSetRange(1, 0), &imapserver.FetchOptions{Items: []imap.FetchItem{imap.FetchItemUID, imap.FetchItemFlags}}); err != nil {
			t.Fatal(err)
		}
		if len(fetched) != 2 {
			t.Fatalf("FETCH responses = %d, want 2", len(fetched))
		}
		firstUID := result.Snapshot.UIDs[0]
		if err := result.Mailbox.Store(context.Background(), fetchWriter, imap.UIDSetNum(firstUID), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil); err != nil {
			t.Fatal(err)
		}
		var expunged []imap.UID
		if err := result.Mailbox.Expunge(context.Background(), &imapserver.ExpungeWriter{WriteFunc: func(_ context.Context, uid imap.UID) error {
			expunged = append(expunged, uid)
			return nil
		}}, nil, nil); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(expunged, []imap.UID{firstUID}) {
			t.Fatalf("EXPUNGE = %v, want [%d]", expunged, firstUID)
		}
		destination := mailbox + "-destination"
		if err := session.Create(context.Background(), destination, nil); err != nil {
			t.Fatal(err)
		}
		remainingUID := result.Snapshot.UIDs[1]
		copyData, err := result.Mailbox.Copy(context.Background(), imap.UIDSetNum(remainingUID), destination, nil)
		if err != nil || copyData == nil || !copyData.HasUIDs || !copyData.SourceUIDs.Contains(remainingUID) {
			t.Fatalf("COPY = %#v, %v", copyData, err)
		}
		if supportsMove(instance, session) {
			moveMailbox, ok := result.Mailbox.(imapserver.MoveMailbox)
			if !ok {
				t.Fatal("MOVE is advertised but selected mailbox does not implement MoveMailbox")
			}
			moveData, err := moveMailbox.Move(context.Background(), imap.UIDSetNum(remainingUID), destination, nil)
			if err != nil || moveData == nil || !moveData.HasUIDs || !moveData.SourceUIDs.Contains(remainingUID) {
				t.Fatalf("MOVE = %#v, %v", moveData, err)
			}
			status, err := result.Mailbox.Status(context.Background(), nil)
			if err != nil || status.NumMessages != 0 {
				t.Fatalf("source status after MOVE = %#v, %v", status, err)
			}
		}
		if err := result.Mailbox.Unselect(context.Background()); err != nil {
			t.Fatal(err)
		}
		closeSession(t, instance, session)
	})

	t.Run("pathological-controls", func(t *testing.T) {
		instance, session := newSession(t, harness)
		mailbox := populate(t, session, "pathological")
		if instance.Controls.ForceUIDValidityChange != nil {
			before := selectMailbox(t, session, mailbox, &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { return nil }})
			if err := before.Mailbox.Unselect(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := instance.Controls.ForceUIDValidityChange(context.Background(), mailbox); err != nil {
				t.Fatal(err)
			}
			after := selectMailbox(t, session, mailbox, &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { return nil }})
			if before.Snapshot.Status.UIDValidity == after.Snapshot.Status.UIDValidity {
				t.Fatal("forced UIDVALIDITY change was not observed")
			}
			if err := after.Mailbox.Unselect(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if instance.Controls.ForceSelectFailure != nil {
			var pushes int
			updater := &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { pushes++; return nil }}
			if err := instance.Controls.ForceSelectFailure(context.Background(), mailbox, true); err != nil {
				t.Fatal(err)
			}
			if _, err := session.Select(context.Background(), mailbox, updater, nil); err == nil {
				t.Fatal("forced Select failure succeeded")
			}
			if err := instance.Controls.ForceSelectFailure(context.Background(), mailbox, false); err != nil {
				t.Fatal(err)
			}
			appendMessage(t, session, mailbox, "after failed select")
			if pushes != 0 {
				t.Fatalf("failed Select left updater attached: %d pushes", pushes)
			}
		}
		closeSession(t, instance, session)
	})
}

func newSession(t *testing.T, harness *Harness) (*Instance, imapserver.Session) {
	t.Helper()
	instance := harness.New()
	if instance == nil || instance.Backend == nil {
		t.Fatal("backendtest: factory returned nil instance or backend")
	}
	_, session := authenticate(t, instance)
	return instance, session
}

func authenticate(t *testing.T, instance *Instance) (imapserver.Backend, imapserver.Session) {
	t.Helper()
	session, err := instance.Backend.Authenticate(context.Background(), &imapserver.ConnInfo{}, &instance.Credentials, nil)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if session == nil {
		t.Fatal("Authenticate returned nil session")
	}
	return instance.Backend, session
}

func populate(t *testing.T, session imapserver.Session, suffix string) string {
	t.Helper()
	mailbox := "backendtest-" + suffix
	if err := session.Create(context.Background(), mailbox, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	appendMessage(t, session, mailbox, "one")
	appendMessage(t, session, mailbox, "two")
	return mailbox
}

func appendMessage(t *testing.T, session imapserver.Session, mailbox, subject string) *imap.AppendData {
	t.Helper()
	raw := []byte(fmt.Sprintf("From: sender@example.com\r\nTo: receiver@example.com\r\nSubject: %s\r\n\r\nbody %s\r\n", subject, subject))
	data, err := session.Append(context.Background(), mailbox, bytes.NewReader(raw), nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if data == nil || !data.HasUID || data.UID == 0 {
		t.Fatalf("Append data = %#v", data)
	}
	return data
}

func selectMailbox(t *testing.T, session imapserver.Session, mailbox string, updater *imapserver.Updater) *imapserver.SelectResult {
	t.Helper()
	result, err := session.Select(context.Background(), mailbox, updater, nil)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if result == nil || result.Mailbox == nil {
		t.Fatalf("Select result = %#v", result)
	}
	return result
}

func validateSnapshot(t *testing.T, snapshot imapserver.SelectSnapshot) {
	t.Helper()
	if len(snapshot.UIDs) != int(snapshot.Status.NumMessages) {
		t.Fatalf("UID count %d != message count %d", len(snapshot.UIDs), snapshot.Status.NumMessages)
	}
	var previous imap.UID
	for _, uid := range snapshot.UIDs {
		if uid == 0 || uid <= previous {
			t.Fatalf("UIDs are not non-zero and strictly ascending: %v", snapshot.UIDs)
		}
		previous = uid
	}
	if snapshot.Status.UIDNext == 0 || snapshot.Status.UIDNext <= previous {
		t.Fatalf("UIDNext %d is not greater than max UID %d", snapshot.Status.UIDNext, previous)
	}
	if snapshot.NumRecent > snapshot.Status.NumMessages {
		t.Fatalf("NumRecent %d > NumMessages %d", snapshot.NumRecent, snapshot.Status.NumMessages)
	}
	if snapshot.Revision == "" {
		t.Fatal("empty snapshot revision")
	}
}

func batchAddsUID(batch *imapserver.UpdateBatch, uid imap.UID) bool {
	for _, change := range batch.Changes {
		if add, ok := change.(*imapserver.UpdateAdd); ok && slices.Contains(add.UIDs, uid) {
			return true
		}
	}
	return false
}

func countBatchAddsUID(batches []*imapserver.UpdateBatch, uid imap.UID) int {
	count := 0
	for _, batch := range batches {
		for _, change := range batch.Changes {
			if add, ok := change.(*imapserver.UpdateAdd); ok {
				for _, added := range add.UIDs {
					if added == uid {
						count++
					}
				}
			}
		}
	}
	return count
}

func waitForBatchCount(mu *sync.Mutex, batches *[]*imapserver.UpdateBatch, notifications <-chan struct{}, want int) []*imapserver.UpdateBatch {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		mu.Lock()
		got := append([]*imapserver.UpdateBatch(nil), (*batches)...)
		mu.Unlock()
		if len(got) >= want {
			return got
		}
		select {
		case <-notifications:
		case <-timer.C:
			return got
		}
	}
}

func waitForAddedUID(mu *sync.Mutex, batches *[]*imapserver.UpdateBatch, notifications <-chan struct{}, uid imap.UID) int {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		mu.Lock()
		count := countBatchAddsUID(*batches, uid)
		mu.Unlock()
		if count != 0 {
			return count
		}
		select {
		case <-notifications:
		case <-timer.C:
			return 0
		}
	}
}

func supportsMove(instance *Instance, session imapserver.Session) bool {
	if support, ok := session.(imapserver.MoveSupport); ok {
		return support.SupportsMove()
	}
	support, ok := instance.Backend.(imapserver.MoveSupport)
	return ok && support.SupportsMove()
}

func cloneBatch(batch *imapserver.UpdateBatch) *imapserver.UpdateBatch {
	if batch == nil {
		return nil
	}
	clone := *batch
	clone.Changes = append([]imapserver.Update(nil), batch.Changes...)
	return &clone
}

func closeSession(t *testing.T, _ *Instance, session imapserver.Session) {
	t.Helper()
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
