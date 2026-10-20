package imapserver_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

type independentCloseBackend struct {
	*memory.Backend
	entered chan struct{}
	release chan struct{}
}
type independentCloseSession struct {
	imapserver.Session
	b *independentCloseBackend
}

func (b *independentCloseBackend) Authenticate(ctx context.Context, c *imapserver.ConnInfo, cr *imapserver.Credentials, o *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, e := b.Backend.Authenticate(ctx, c, cr, o)
	if e != nil {
		return nil, e
	}
	return &independentCloseSession{s, b}, nil
}
func (s *independentCloseSession) Close(ctx context.Context, o *imapserver.SessionCloseOptions) error {
	close(s.b.entered)
	<-s.b.release
	return s.Session.Close(ctx, o)
}
func TestReviewFinalConcurrentServerClose(t *testing.T) {
	b := &independentCloseBackend{r4Memory(), make(chan struct{}), make(chan struct{})}
	srv := imapserver.New(b, &imapserver.Options{AllowInsecureAuth: true})
	peer, client := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { done <- srv.ServeConn(context.Background(), peer, nil) }()
	w := &r4Wire{t: t, c: client, r: bufio.NewReader(client)}
	if _, e := w.line(); e != nil {
		t.Fatal(e)
	}
	w.cmd("L", "LOGIN alice secret")
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	result := make(chan error, 2)
	go func() { result <- srv.Close(ctx, nil) }()
	<-b.entered
	go func() { result <- srv.Close(ctx, nil) }()
	// Both Close calls are now waiting for the only ServeConn cleanup to finish.
	time.Sleep(30 * time.Millisecond)
	close(b.release)
	<-done
	for i := 0; i < 2; i++ {
		if e := <-result; e != nil {
			t.Errorf("concurrent Close returned %v after ServeConn finished", e)
		}
	}
}

type independentRejectedBackend struct{ *memory.Backend }
type independentRejectedSession struct{ imapserver.Session }
type independentRejectedSelected struct {
	imapserver.SelectedMailbox
	cond imapserver.CondStoreMailbox
}

func (b *independentRejectedBackend) Authenticate(ctx context.Context, c *imapserver.ConnInfo, cr *imapserver.Credentials, o *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, e := b.Backend.Authenticate(ctx, c, cr, o)
	if e != nil {
		return nil, e
	}
	return &independentRejectedSession{s}, nil
}
func (s *independentRejectedSession) Select(ctx context.Context, n string, u *imapserver.Updater, o *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, e := s.Session.Select(ctx, n, u, o)
	if e == nil {
		r.Mailbox = &independentRejectedSelected{r.Mailbox, r.Mailbox.(imapserver.CondStoreMailbox)}
	}
	return r, e
}
func (s *independentRejectedSelected) StoreCondStore(ctx context.Context, w *imapserver.FetchWriter, u imap.UIDSet, f *imapserver.StoreFlags, o *imapserver.StoreOptions) (*imapserver.CondStoreResult, error) {
	r, e := s.cond.StoreCondStore(ctx, w, u, f, o)
	if e == nil && !r.Modified.IsEmpty() {
		e = s.SelectedMailbox.Fetch(ctx, w, r.Modified, &imapserver.FetchOptions{Items: []imap.FetchItem{imap.FetchItemUID, imap.FetchItemFlags}})
	}
	return r, e
}
func TestReviewFinalRejectedFlagsOnly(t *testing.T) {
	w := r4Open(t, &independentRejectedBackend{r4Memory()}, nil)
	r4Append(w, "INBOX", "", "one")
	w.cmd("E", "ENABLE CONDSTORE")
	w.cmd("S", "SELECT INBOX")
	lines := w.cmd("X", `STORE 1 (UNCHANGEDSINCE 0) +FLAGS (\Flagged)`)
	r4Log(t, "rejected FLAGS-only", lines)
	for _, l := range lines {
		if strings.Contains(l, "FETCH") && !strings.Contains(l, "MODSEQ") {
			t.Errorf("CONDSTORE FETCH lacks MODSEQ: %s", l)
		}
	}
}

func TestReviewFinalCleanupProtocolDiagnostic(t *testing.T) {
	var got atomic.Int32
	b := &independentDiagnosticBackend{r4Memory()}
	w := r4Open(t, b, &imapserver.Options{AllowInsecureAuth: true, OnError: func(_ context.Context, e *imapserver.ServerEvent) {
		if errors.Is(e.Err, independentCleanupCause) {
			got.Add(1)
		}
	}})
	w.cmd("S", "SELECT INBOX")
	w.cmd("U", "UNSELECT")
	time.Sleep(50 * time.Millisecond)
	if got.Load() != 1 {
		t.Errorf("cleanup protocol error produced %d diagnostics, want 1", got.Load())
	}
}

var independentCleanupCause = errors.New("cleanup failed")

type independentDiagnosticBackend struct{ *memory.Backend }
type independentDiagnosticSession struct{ imapserver.Session }
type independentDiagnosticSelected struct{ imapserver.SelectedMailbox }

func (b *independentDiagnosticBackend) Authenticate(ctx context.Context, c *imapserver.ConnInfo, cr *imapserver.Credentials, o *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, e := b.Backend.Authenticate(ctx, c, cr, o)
	if e != nil {
		return nil, e
	}
	return &independentDiagnosticSession{s}, nil
}
func (s *independentDiagnosticSession) Select(ctx context.Context, n string, u *imapserver.Updater, o *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, e := s.Session.Select(ctx, n, u, o)
	if e == nil {
		r.Mailbox = &independentDiagnosticSelected{r.Mailbox}
	}
	return r, e
}
func (s *independentDiagnosticSelected) Unselect(ctx context.Context, o *imapserver.UnselectOptions) error {
	_ = s.SelectedMailbox.Unselect(ctx, o)
	return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeUnavailable, Text: "cleanup", Err: independentCleanupCause}
}
