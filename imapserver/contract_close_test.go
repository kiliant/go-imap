package imapserver_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// countingBackend wraps memory and counts Close on the sessions it hands out,
// optionally failing it.
type countingBackend struct {
	inner   imapserver.Backend
	failure error
	closes  atomic.Int64
}

func (b *countingBackend) Authenticate(ctx context.Context, conn *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.inner.Authenticate(ctx, conn, credentials, options)
	if err != nil {
		return nil, err
	}
	return &countingSession{Session: session, backend: b}, nil
}

type countingSession struct {
	imapserver.Session
	backend *countingBackend
}

func (s *countingSession) Close(ctx context.Context, _ *imapserver.SessionCloseOptions) error {
	s.backend.closes.Add(1)
	_ = s.Session.Close(ctx, nil)
	return s.backend.failure
}

// Unauthenticate has to be forwarded explicitly: the wrapper would otherwise
// hide the optional interface the inner session implements, which is the trap
// imapserver/examples/config.go documents.
func (s *countingSession) Unauthenticate(ctx context.Context, options *imapserver.UnauthenticateOptions) error {
	if inner, ok := s.Session.(imapserver.UnauthenticateSession); ok {
		return inner.Unauthenticate(ctx, options)
	}
	return nil
}

// TestUnauthenticateClosesSessionOnce drives the real UNAUTHENTICATE command,
// which is the point: the bug it covers was in the handler's ordering, so a
// test that reproduced the ordering itself would have passed against the broken
// code.
//
// Session.Close's documented contract is that the framework calls it once. That
// was false on the error path — handleUnauthenticate returned on a Close error
// before clearing the session, so connection teardown closed the same session
// again. A backend that releases a pooled handle or decrements a refcount in
// Close sees a double release, on an error path, where it is hardest to find.
func TestUnauthenticateClosesSessionOnce(t *testing.T) {
	for _, failing := range []bool{false, true} {
		name := "clean"
		if failing {
			name = "close-fails"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			backend := &countingBackend{
				inner: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}),
			}
			if failing {
				backend.failure = &imap.Error{Type: imap.ErrorTypeNo, Text: "close failed"}
			}
			server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})

			serverSide, clientSide := net.Pipe()
			serveDone := make(chan struct{})
			go func() { defer close(serveDone); _ = server.ServeConn(ctx, serverSide) }()
			reader := bufio.NewReader(clientSide)
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}

			writeRawCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
			if _, tagged := collectUntilTag(t, reader, "A1 "); !strings.HasPrefix(tagged, "A1 OK") {
				t.Fatalf("LOGIN failed: %q", tagged)
			}
			writeRawCommand(t, clientSide, "A2 UNAUTHENTICATE\r\n")
			_, tagged := collectUntilTag(t, reader, "A2 ")
			if failing && !strings.HasPrefix(tagged, "A2 NO") {
				t.Errorf("UNAUTHENTICATE with a failing Close = %q, want NO", tagged)
			}
			if !failing && !strings.HasPrefix(tagged, "A2 OK") {
				t.Errorf("UNAUTHENTICATE = %q, want OK", tagged)
			}

			// Tear the connection down, which is where the second Close came
			// from: the state still held a session the handler had closed.
			_ = clientSide.Close()
			<-serveDone

			if got := backend.closes.Load(); got != 1 {
				t.Errorf("Session.Close called %d times, want exactly 1", got)
			}
		})
	}
}

// overPopulatingSession fills its whole StatusData regardless of what was
// asked for — the obvious way to write a backend, and the case the narrowing
// exists for.
type overPopulatingSession struct {
	imapserver.Session
}

func (s *overPopulatingSession) Status(ctx context.Context, mailbox string, options *imapserver.StatusOptions) (*imap.StatusData, error) {
	return &imap.StatusData{
		Mailbox: mailbox,
		Values: map[imap.StatusItemKeyword]any{
			imap.StatusItemMessages:    uint64(0),
			imap.StatusItemRecent:      uint64(7),
			imap.StatusItemUnseen:      uint64(42),
			imap.StatusItemUIDNext:     uint64(99),
			imap.StatusItemUIDValidity: uint64(1),
			imap.StatusItemSize:        uint64(12345),
		},
	}, nil
}

type wrappingBackend struct {
	inner imapserver.Backend
	wrap  func(imapserver.Session) imapserver.Session
}

func (b *wrappingBackend) Authenticate(ctx context.Context, conn *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.inner.Authenticate(ctx, conn, credentials, options)
	if err != nil {
		return nil, err
	}
	return b.wrap(session), nil
}

// TestStatusResponseCarriesOnlyRequestedItems is the wire-level half of the
// guarantee. The unit test beside it exercises the narrowing helper, which
// would keep passing if the helper were never called — and it was not, until
// this release.
//
// It needs a backend that over-populates, because imapserver/memory does not:
// the first version of this test passed against the unfixed framework for that
// reason, which is the same shape of mistake as testing rev2 advertisement
// against a backend that implements everything.
func TestStatusResponseCarriesOnlyRequestedItems(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	backend := &wrappingBackend{
		inner: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}),
		wrap: func(session imapserver.Session) imapserver.Session {
			return &overPopulatingSession{Session: session}
		},
	}
	server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})

	serverSide, clientSide := net.Pipe()
	go func() { _ = server.ServeConn(ctx, serverSide) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	writeRawCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
	if _, tagged := collectUntilTag(t, reader, "A1 "); !strings.HasPrefix(tagged, "A1 OK") {
		t.Fatalf("LOGIN failed: %q", tagged)
	}

	writeRawCommand(t, clientSide, "A2 STATUS INBOX (MESSAGES)\r\n")
	untagged, tagged := collectUntilTag(t, reader, "A2 ")
	if !strings.HasPrefix(tagged, "A2 OK") {
		t.Fatalf("STATUS failed: %q", tagged)
	}
	line := findResponse(t, untagged, "* STATUS")
	for _, volunteered := range []string{"RECENT", "UNSEEN", "UIDNEXT", "UIDVALIDITY", "SIZE"} {
		if strings.Contains(line, volunteered) {
			t.Errorf("STATUS volunteered %s to a client that asked only for MESSAGES: %q", volunteered, line)
		}
	}
	if !strings.Contains(line, "MESSAGES") {
		t.Errorf("STATUS omitted the item that was asked for: %q", line)
	}
}
