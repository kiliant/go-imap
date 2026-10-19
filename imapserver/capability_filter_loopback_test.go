package imapserver_test

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

type filteredCatenateBackend struct {
	*memory.Backend
	calls atomic.Int32
}

func (*filteredCatenateBackend) AllowCapability(_ context.Context, name string, _ *imapserver.CapabilityFilterOptions) bool {
	return name != "CATENATE"
}

func (b *filteredCatenateBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return struct {
		imapserver.Session
		imapserver.CatenateSession
	}{session, b}, nil
}

func (b *filteredCatenateBackend) ResolveCatenateURL(context.Context, string, *imapserver.CatenateOptions) (io.ReadCloser, error) {
	b.calls.Add(1)
	return nil, nil
}

func TestFilteredCatenateDoesNotResolveURLs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	backend := &filteredCatenateBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
	_, client, reader := newRawSessionOn(t, ctx, imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true}))
	writeRawCommand(t, client, "F1 APPEND INBOX CATENATE (URL \"imap://localhost/INBOX/;UID=1\" TEXT {3+}\r\nabc)\r\n")
	if _, tagged := collectUntilTag(t, reader, "F1 "); !strings.HasPrefix(tagged, "F1 NO") {
		t.Fatalf("filtered CATENATE: %q", tagged)
	}
	writeRawCommand(t, client, "F2 NOOP\r\n")
	if lines, tagged := collectUntilTag(t, reader, "F2 "); len(lines) != 0 || !strings.HasPrefix(tagged, "F2 OK") {
		t.Fatalf("refusal desynchronized connection: %v %q", lines, tagged)
	}
	if backend.calls.Load() != 0 {
		t.Fatal("filtered CATENATE reached the backend")
	}
}
