package imapserver

import (
	"context"
	"fmt"
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
	options chan AppendOptions
	once    sync.Once
}

func (b *streamingAppendBackend) Authenticate(context.Context, *ConnInfo, *Credentials, *AuthenticateOptions) (Session, error) {
	return &streamingAppendSession{stubSession: stubSession{}, backend: b}, nil
}

type streamingAppendSession struct {
	stubSession
	backend *streamingAppendBackend
}

func (s *streamingAppendSession) Append(_ context.Context, _ string, literal io.Reader, options *AppendOptions) (*imap.AppendData, error) {
	if s.backend.options != nil {
		s.backend.options <- *options
	}
	s.backend.once.Do(func() { close(s.backend.started) })
	if _, err := io.Copy(io.Discard, literal); err != nil {
		return nil, err
	}
	return &imap.AppendData{HasUID: true, UIDValidity: 1, UID: 1}, nil
}

type stagedAppendReader struct {
	remaining int
	started   <-chan struct{}
}

func (r *stagedAppendReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	select {
	case <-r.started:
		return 0, fmt.Errorf("backend called before complete APPEND validation")
	default:
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

func TestAppendStagesCompleteCommandBeforeBackend(t *testing.T) {
	backend := &streamingAppendBackend{started: make(chan struct{}), options: make(chan AppendOptions, 1)}
	server := New(backend, &Options{AllowInsecureAuth: true})
	serverSide, clientSide := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide, nil) }()

	client := imapclient.NewClient(clientSide, &imapclient.Options{AllowInsecureAuth: true})
	if err := client.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Login(ctx, "alice", "secret", nil); err != nil {
		t.Fatal(err)
	}
	const size = 32 << 10
	reader := &stagedAppendReader{remaining: size, started: backend.started}
	appendCommand := client.Append(ctx, "INBOX", nil, size, reader)
	data, err := appendCommand.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if data == nil || !data.HasUID || data.UID != 1 {
		t.Fatalf("APPEND data = %#v", data)
	}
	if options := <-backend.options; !options.HasSize || options.Size != size {
		t.Fatalf("streaming APPEND size = %d (present %t), want %d", options.Size, options.HasSize, size)
	}
	if err := client.Logout(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
