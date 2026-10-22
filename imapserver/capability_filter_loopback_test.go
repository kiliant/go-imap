package imapserver_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
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

// deadlineCapabilityBackend records the longest deadline any capability
// predicate, witness or filter, was called with.
type deadlineCapabilityBackend struct {
	*memory.Backend
	mu        sync.Mutex
	calls     int
	unbounded bool
	longest   time.Duration
}

func (b *deadlineCapabilityBackend) observe(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	deadline, ok := ctx.Deadline()
	b.unbounded = b.unbounded || !ok
	b.longest = max(b.longest, time.Until(deadline))
}

func (b *deadlineCapabilityBackend) SupportsCapability(ctx context.Context, name string, options *imapserver.CapabilitySupportOptions) bool {
	b.observe(ctx)
	return b.Backend.SupportsCapability(ctx, name, options)
}

func (b *deadlineCapabilityBackend) AllowCapability(ctx context.Context, _ string, _ *imapserver.CapabilityFilterOptions) bool {
	b.observe(ctx)
	return true
}

// Capability predicates are backend calls, and many of them run from helpers
// that hold only the connection's context: the greeting, and every handler
// asking whether a capability is advertised. Limits.CommandTimeout bounds all
// of them.
func TestCapabilityPredicatesHonourCommandTimeout(t *testing.T) {
	const timeout = 10 * time.Second
	b := &deadlineCapabilityBackend{Backend: r4Memory()}
	w := r4Open(t, b, &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{CommandTimeout: timeout}})
	raw := "Subject: m\r\n\r\nbody\r\n"
	for tag, command := range []string{
		"CAPABILITY",
		"SELECT INBOX",
		fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw),
		"SEARCH RETURN (MIN) ALL",
		`CREATE "box"`,
		`LIST (SUBSCRIBED) "" "*"`,
	} {
		sweepWant(t, w.cmd(fmt.Sprint("C", tag), command), "OK")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.calls == 0 {
		t.Fatal("no capability predicate was consulted")
	}
	if b.unbounded || b.longest > timeout {
		t.Fatalf("capability predicate deadline = %v (unbounded %v), want within %v", b.longest, b.unbounded, timeout)
	}
}
