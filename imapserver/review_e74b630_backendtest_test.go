package imapserver_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/backendtest"
)

type e74LowerSession struct{ imapserver.Session }
type e74LowerSelected struct{ imapserver.SelectedMailbox }

func (s *e74LowerSession) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, err := s.Session.Select(ctx, name, updater, options)
	if err == nil {
		r.Mailbox = &e74LowerSelected{r.Mailbox}
	}
	return r, err
}
func e74LowerWriter(writer *imapserver.FetchWriter) *imapserver.FetchWriter {
	return &imapserver.FetchWriter{WriteFunc: func(ctx context.Context, data *imap.FetchMessageData) error {
		copy := *data
		copy.Items = make(map[imap.FetchDataKey][]imap.FetchData, len(data.Items))
		for key, values := range data.Items {
			copy.Items[imap.FetchDataKey(strings.ToLower(string(key)))] = values
		}
		return writer.WriteMessage(ctx, &copy)
	}}
}
func (m *e74LowerSelected) Fetch(ctx context.Context, w *imapserver.FetchWriter, uids imap.UIDSet, options *imapserver.FetchOptions) error {
	return m.SelectedMailbox.Fetch(ctx, e74LowerWriter(w), uids, options)
}
func (m *e74LowerSelected) Store(ctx context.Context, w *imapserver.FetchWriter, uids imap.UIDSet, flags *imapserver.StoreFlags, options *imapserver.StoreOptions) error {
	return m.SelectedMailbox.Store(ctx, e74LowerWriter(w), uids, flags, options)
}
func TestReviewE74SuiteCaseInsensitiveKeys(t *testing.T) {
	backendtest.Run(t, r4Harness(func(s imapserver.Session) imapserver.Session { return &e74LowerSession{s} }))
}

type e74CondOnlySession struct {
	imapserver.Session
	removedClock *atomic.Uint64
}

func (*e74CondOnlySession) SupportsCapability(_ context.Context, name string, _ *imapserver.CapabilitySupportOptions) bool {
	return name == "CONDSTORE"
}
func (s *e74CondOnlySession) Select(ctx context.Context, name string, u *imapserver.Updater, o *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, err := s.Session.Select(ctx, name, u, o)
	if err == nil && strings.EqualFold(name, "INBOX") && s.removedClock.Load() != 0 {
		r.Snapshot.Status.HighestModSeq = s.removedClock.Load()
	}
	return r, err
}
func (s *e74CondOnlySession) Rename(ctx context.Context, oldName, newName string, o *imapserver.RenameOptions) error {
	var clock uint64
	if strings.EqualFold(oldName, "INBOX") {
		status, err := s.Session.Status(ctx, oldName, &imapserver.StatusOptions{Items: []imap.StatusItem{imap.StatusItemHighestModSeq}})
		if err != nil {
			return err
		}
		clock, _ = status.Number(imap.StatusItemHighestModSeq)
	}
	err := s.Session.Rename(ctx, oldName, newName, o)
	if err == nil && clock != 0 {
		s.removedClock.Store(clock)
	}
	return err
}
func TestReviewE74SuiteCondStoreOnlyRemovalClock(t *testing.T) {
	clock := &atomic.Uint64{}
	backendtest.Run(t, r4Harness(func(s imapserver.Session) imapserver.Session { return &e74CondOnlySession{s, clock} }))
}

type e74LateSession struct {
	imapserver.Session
	release chan struct{}
	once    *sync.Once
}

func (s *e74LateSession) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	late := &imapserver.Updater{PushFunc: func(batch *imapserver.UpdateBatch) error {
		go func() { <-s.release; _ = updater.Push(batch) }()
		return nil
	}}
	return s.Session.Select(ctx, name, late, options)
}
func (s *e74LateSession) Close(ctx context.Context, options *imapserver.SessionCloseOptions) error {
	s.once.Do(func() { close(s.release) })
	return s.Session.Close(ctx, options)
}

// The conformance suite must reject late publication, rather than allowing
// a notification timeout to mask the backend's violation of the new contract.
func TestReviewE74SuiteRejectsLatePublication(t *testing.T) {
	if os.Getenv("GO_IMAP_TEST_LATE_PUBLICATION") == "1" {
		release, once := make(chan struct{}), &sync.Once{}
		backendtest.Run(t, r4Harness(func(s imapserver.Session) imapserver.Session { return &e74LateSession{s, release, once} }))
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestReviewE74SuiteRejectsLatePublication$/synchronous-publication$")
	command.Env = append(os.Environ(), "GO_IMAP_TEST_LATE_PUBLICATION=1")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "returned before publishing") {
		t.Fatalf("late feed conformance result: %v\n%s", err, output)
	}
}
