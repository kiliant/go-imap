package imapserver

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type notifyOverflowBackend struct {
	updaters chan *SessionUpdater
	stopped  chan error
	stopErr  error
}

func (b *notifyOverflowBackend) Authenticate(context.Context, *ConnInfo, *Credentials, *AuthenticateOptions) (Session, error) {
	return &notifyOverflowSession{backend: b}, nil
}

type notifyOverflowSession struct {
	stubSession
	backend    *notifyOverflowBackend
	updater    *SessionUpdater
	generation int
}

func notifyStatus(name string, count uint32) *SessionUpdate {
	return &SessionUpdate{Mailbox: name, Status: &imap.StatusData{
		Mailbox: name, Values: map[imap.StatusItemKeyword]any{imap.StatusItemMessages: uint64(count)},
	}}
}

func (s *notifyOverflowSession) Notify(_ context.Context, updater *SessionUpdater, config *NotifyConfig, _ *NotifyOptions) error {
	if config == nil {
		if updater != nil {
			return errors.New("NOTIFY NONE received a non-nil updater")
		}
		s.backend.stopped <- s.updater.Push(notifyStatus("late", 4))
		return s.backend.stopErr
	}
	s.updater = updater
	s.backend.updaters <- updater
	s.generation++
	if s.generation > 1 {
		return updater.Push(notifyStatus("resumed", 5))
	}
	// Push synchronously while the handler owns the event loop, so the queue
	// deterministically overflows before it can be drained.
	for _, update := range []*SessionUpdate{
		notifyStatus("first", 1), notifyStatus("first", 2), notifyStatus("second", 3),
	} {
		if err := updater.Push(update); err != nil {
			return err
		}
	}
	return nil
}

func TestNotifyOverflowEndsRegistration(t *testing.T) {
	backend := &notifyOverflowBackend{updaters: make(chan *SessionUpdater, 2), stopped: make(chan error, 1)}
	server := New(backend, &Options{AllowInsecureAuth: true, Limits: Limits{MaxQueuedUpdates: 1}})
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	if err := clientSide.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = server.ServeConn(ctx, serverSide, nil) }()
	reader := bufio.NewReader(clientSide)
	readLine := func() string {
		t.Helper()
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	readLine() // greeting
	writeUpdateOrderCommand(t, clientSide, "L LOGIN alice secret\r\n")
	if line := readLine(); !strings.HasPrefix(line, "L OK ") {
		t.Fatal(line)
	}
	writeUpdateOrderCommand(t, clientSide, "N1 NOTIFY SET (PERSONAL (MessageNew))\r\n")
	if line := readLine(); !strings.HasPrefix(line, "N1 OK ") {
		t.Fatal(line)
	}
	old := <-backend.updaters
	for line := readLine(); !strings.HasPrefix(line, "* OK [NOTIFICATIONOVERFLOW]"); line = readLine() {
		if !strings.HasPrefix(line, "* STATUS ") {
			t.Fatalf("unexpected overflow response: %q", line)
		}
	}
	select {
	case err := <-backend.stopped:
		if !errors.Is(err, ErrUpdaterClosed) {
			t.Fatalf("old updater during NOTIFY NONE: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("overflow did not call backend NOTIFY NONE")
	}
	if err := old.Push(notifyStatus("late", 4)); !errors.Is(err, ErrUpdaterClosed) {
		t.Fatalf("push after overflow = %v, want ErrUpdaterClosed", err)
	}
	writeUpdateOrderCommand(t, clientSide, "N2 NOOP\r\n")
	if line := readLine(); !strings.HasPrefix(line, "N2 OK ") {
		t.Fatalf("notification after overflow: %q", line)
	}
	// A client can establish a fresh registration after the overflow.
	writeUpdateOrderCommand(t, clientSide, "N3 NOTIFY SET (PERSONAL (MessageNew))\r\n")
	if line := readLine(); !strings.HasPrefix(line, "N3 OK ") {
		t.Fatal(line)
	}
	if line := readLine(); !strings.HasPrefix(line, "* STATUS resumed (MESSAGES 5)") {
		t.Fatalf("new registration did not deliver: %q", line)
	}
	if fresh := <-backend.updaters; fresh == old {
		t.Fatal("new registration reused closed updater")
	}
}

func TestNotifyCoalescingDoesNotOverflow(t *testing.T) {
	queue := newSessionUpdateQueue(1)
	for _, count := range []uint32{1, 2, 3} {
		if err := queue.push(notifyStatus("same", count)); err != nil {
			t.Fatal(err)
		}
	}
	updates, overflow := queue.popAll()
	if overflow || len(updates) != 1 || updates[0].Status.Values[imap.StatusItemMessages] != uint64(3) {
		t.Fatalf("coalesced updates = %+v, overflow = %v", updates, overflow)
	}
}

func TestNotifyOverflowClosesRegistrationOnBackendFailure(t *testing.T) {
	failure := errors.New("backend teardown failed")
	backend := &notifyOverflowBackend{stopped: make(chan error, 1), stopErr: failure}
	queue := newSessionUpdateQueue(1)
	updater := &SessionUpdater{core: &sessionUpdaterCore{queue: queue, active: true}}
	session := &notifyOverflowSession{backend: backend, updater: updater}
	var output bytes.Buffer
	c := &conn{
		ctx: context.Background(), server: New(backend, nil),
		encoder:     imapwire.NewEncoder(&output, &imapwire.EncoderOptions{ServerResponse: true}),
		notifyQueue: queue, notifyUpdater: updater,
	}
	c.state.session = session
	for _, name := range []string{"first", "second"} {
		if err := updater.Push(notifyStatus(name, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.drainNotify(); !errors.Is(err, failure) {
		t.Fatalf("drainNotify = %v, want backend error", err)
	}
	if c.notifyQueue != nil || c.notifyUpdater != nil {
		t.Fatal("backend failure left the registration active")
	}
	if !strings.Contains(output.String(), "[NOTIFICATIONOVERFLOW]") {
		t.Fatalf("overflow was not reported: %q", output.String())
	}
}

func TestNotifyStatusItemsUseNegotiatedGates(t *testing.T) {
	for _, rev2 := range []bool{false, true} {
		state := newSessionState(true)
		state.authenticate(&stubSession{})
		if rev2 {
			state.revision = revisionIMAP4rev2
		}
		var wire bytes.Buffer
		data := &imap.StatusData{Mailbox: "Other", Values: map[imap.StatusItemKeyword]any{
			imap.StatusItemMessages: uint64(1), imap.StatusItemHighestModSeq: uint64(2),
			imap.StatusItemDeleted: uint64(3), imap.StatusItemSize: uint64(4), "FOO": uint64(5),
		}}
		queue := newSessionUpdateQueue(2)
		if err := queue.push(&SessionUpdate{Mailbox: "Other", Status: data}); err != nil {
			t.Fatal(err)
		}
		c := &conn{ctx: t.Context(), state: state, server: New(nil, nil), notifyQueue: queue, encoder: imapwire.NewEncoder(&wire, &imapwire.EncoderOptions{ServerResponse: true})}
		if err := c.drainNotify(); err != nil {
			t.Fatal(err)
		}
		got := wire.String()
		if !strings.Contains(got, "MESSAGES 1") || strings.Contains(got, "HIGHESTMODSEQ") || strings.Contains(got, "FOO") {
			t.Fatalf("NOTIFY STATUS = %s", got)
		}
		if strings.Contains(got, "DELETED 3") != rev2 || strings.Contains(got, "SIZE 4") != rev2 {
			t.Fatalf("rev2=%t, NOTIFY STATUS = %s", rev2, got)
		}
		if len(data.Values) != 5 {
			t.Fatal("backend's STATUS map was modified")
		}
	}
}
