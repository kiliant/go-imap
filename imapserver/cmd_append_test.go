package imapserver

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

type streamingAppendBackend struct {
	started chan struct{}
	once    sync.Once
}

func (b *streamingAppendBackend) Authenticate(context.Context, *ConnInfo, *Credentials, *AuthenticateOptions) (Session, error) {
	return &streamingAppendSession{stubSession: stubSession{}, backend: b}, nil
}

type streamingAppendSession struct {
	stubSession
	backend *streamingAppendBackend
}

func (s *streamingAppendSession) Append(_ context.Context, _ string, literal io.Reader, _ *AppendOptions) (*imap.AppendData, error) {
	s.backend.once.Do(func() { close(s.backend.started) })
	if _, err := io.Copy(io.Discard, literal); err != nil {
		return nil, err
	}
	return &imap.AppendData{HasUID: true, UIDValidity: 1, UID: 1}, nil
}

type gatedAppendReader struct {
	remaining int
	started   <-chan struct{}
	first     bool
}

func (r *gatedAppendReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if r.first {
		select {
		case <-r.started:
		case <-time.After(time.Second):
			return 0, context.DeadlineExceeded
		}
	} else {
		r.first = true
	}
	if len(p) > 1024 {
		p = p[:1024]
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 'x'
	}
	r.remaining -= len(p)
	return len(p), nil
}

func TestAppendStreamsIntoBackendBeforeClientFinishesProducing(t *testing.T) {
	backend := &streamingAppendBackend{started: make(chan struct{})}
	server := New(backend, &Options{AllowInsecureAuth: true})
	serverSide, clientSide := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide) }()

	client := imapclient.NewClient(clientSide, &imapclient.Options{AllowInsecureAuth: true})
	if err := client.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Login(ctx, "alice", "secret", nil); err != nil {
		t.Fatal(err)
	}
	const size = 32 << 10
	reader := &gatedAppendReader{remaining: size, started: backend.started}
	appendCommand := client.Append(ctx, "INBOX", nil, size, reader)
	data, err := appendCommand.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if data == nil || !data.HasUID || data.UID != 1 {
		t.Fatalf("APPEND data = %#v", data)
	}
	if err := client.Logout(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
