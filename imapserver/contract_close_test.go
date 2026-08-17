package imapserver_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
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
			go func() { defer close(serveDone); _ = server.ServeConn(ctx, serverSide, nil) }()
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
	// deny withholds capability tokens the inner backend would witness.
	// memory implements CapabilitySupport on the Backend, not the Session, so
	// this is the level at which a token can be taken away.
	deny map[string]bool
	// denyAll withholds every token, which is what a third-party backend that
	// witnesses nothing looks like.
	denyAll bool
}

// SupportsMove forwards the witness memory declares on its Backend. Without
// this the wrapper silently withholds atomic MOVE, and with it IMAP4rev2 —
// which is the wrapper trap the examples document, met in a test.
func (b *wrappingBackend) SupportsMove() bool {
	inner, ok := b.inner.(imapserver.MoveSupport)
	return ok && inner.SupportsMove()
}

func (b *wrappingBackend) SupportsCapability(name string) bool {
	if b.denyAll || b.deny[name] {
		return false
	}
	if inner, ok := b.inner.(imapserver.CapabilitySupport); ok {
		return inner.SupportsCapability(name)
	}
	return false
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
	go func() { _ = server.ServeConn(ctx, serverSide, nil) }()
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

// noWitnessSession witnesses nothing at all: no CapabilitySupport, no optional
// interfaces. It is what a third-party backend compiled against a fixed set of
// search keys and fetch items looks like from the framework's side.
type noWitnessSession struct {
	imapserver.Session
	onSelect func(*noWitnessMailbox)
}

func (s *noWitnessSession) Select(ctx context.Context, mailbox string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, mailbox, updater, options)
	if err != nil {
		return nil, err
	}
	wrapped := &noWitnessMailbox{SelectedMailbox: result.Mailbox}
	result.Mailbox = wrapped
	if s.onSelect != nil {
		s.onSelect(wrapped)
	}
	return result, nil
}

// noWitnessMailbox records every criterion and item it is handed, so the test
// asserts on what actually reached the backend rather than on the wire reply.
type noWitnessMailbox struct {
	imapserver.SelectedMailbox
	criteria []imap.SearchCriteria
	items    []imap.FetchItem
}

func (m *noWitnessMailbox) Search(ctx context.Context, query *imapserver.SearchQuery, options *imapserver.SearchOptions) (*imapserver.SearchResult, error) {
	m.criteria = append(m.criteria, query.Criteria())
	return m.SelectedMailbox.Search(ctx, query, options)
}

func (m *noWitnessMailbox) Fetch(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, options *imapserver.FetchOptions) error {
	if options != nil {
		m.items = append(m.items, options.Items...)
	}
	return m.SelectedMailbox.Fetch(ctx, writer, uids, options)
}

// TestExtensionKeysAndItemsAreGated is the regression for the finding that an
// extension SEARCH key or FETCH item reached the backend with no capability
// gate at all.
//
// Every extension *command* handler calls requireCapability. A search key is
// not a command and a fetch item is not a command, so both escaped it: a
// backend witnessing nothing still received MODSEQ and FUZZY, having been
// offered neither CONDSTORE nor SEARCH=FUZZY.
//
// The failure this prevents is not a crash. It is the next release of package
// imap adding RFC 5257's ANNOTATION, every already-compiled backend receiving
// it, and the permissive default branch such a backend reasonably wrote
// returning a silently empty result.
func TestExtensionKeysAndItemsAreGated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var (
		mu      sync.Mutex
		mailbox *noWitnessMailbox
	)
	backend := &wrappingBackend{
		denyAll: true,
		inner:   memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}),
		wrap: func(session imapserver.Session) imapserver.Session {
			return &noWitnessSession{
				Session: session,
				onSelect: func(selected *noWitnessMailbox) {
					mu.Lock()
					defer mu.Unlock()
					mailbox = selected
				},
			}
		},
	}
	server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})

	serverSide, clientSide := net.Pipe()
	go func() { _ = server.ServeConn(ctx, serverSide, nil) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	writeRawCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
	if _, tagged := collectUntilTag(t, reader, "A1 "); !strings.HasPrefix(tagged, "A1 OK") {
		t.Fatalf("LOGIN failed: %q", tagged)
	}
	writeRawCommand(t, clientSide, "A2 SELECT INBOX\r\n")
	if _, tagged := collectUntilTag(t, reader, "A2 "); !strings.HasPrefix(tagged, "A2 OK") {
		t.Fatalf("SELECT failed: %q", tagged)
	}

	for _, testCase := range []struct{ name, command string }{
		{"search-fuzzy", "A3 SEARCH FUZZY SUBJECT \"x\""},
		{"search-modseq", "A4 SEARCH MODSEQ 1"},
		{"search-nested", "A5 SEARCH OR ALL MODSEQ 1"},
		{"fetch-modseq", "A6 FETCH 1 (MODSEQ)"},
		{"fetch-emailid", "A7 FETCH 1 (EMAILID)"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tag := strings.SplitN(testCase.command, " ", 2)[0]
			writeRawCommand(t, clientSide, testCase.command+"\r\n")
			_, tagged := collectUntilTag(t, reader, tag+" ")
			if !strings.HasPrefix(tagged, tag+" NO") {
				t.Errorf("%s = %q, want NO: the session advertised no capability that licenses it",
					testCase.command, tagged)
			}
		})
	}

	// The wire reply is the symptom. What the finding was actually about is
	// what reached the backend, so assert on that directly — and fail if the
	// recorder was never installed, because a nil here would otherwise make the
	// whole check vacuous.
	mu.Lock()
	recorded := mailbox
	mu.Unlock()
	if recorded == nil {
		t.Fatal("the recording mailbox was never installed; this test asserts nothing")
	}
	if len(recorded.criteria) != 0 || len(recorded.items) != 0 {
		t.Errorf("ungated keys reached the backend: criteria=%v items=%v", recorded.criteria, recorded.items)
	}

	// A baseline search still works: the gate refuses what was never offered,
	// not everything.
	writeRawCommand(t, clientSide, "A8 SEARCH ALL\r\n")
	if _, tagged := collectUntilTag(t, reader, "A8 "); !strings.HasPrefix(tagged, "A8 OK") {
		t.Errorf("baseline SEARCH ALL was refused: %q", tagged)
	}
	writeRawCommand(t, clientSide, "A9 FETCH 1 (FLAGS)\r\n")
	if _, tagged := collectUntilTag(t, reader, "A9 "); !strings.HasPrefix(tagged, "A9 OK") {
		t.Errorf("baseline FETCH was refused: %q", tagged)
	}
}

// TestParseAdmitsImpliesGateAdmits is the gate on the bug class the capability
// gate introduced: a key the parser accepts and the framework otherwise
// supports, refused by the gate.
//
// The instance is BINARY[]. RFC 9051 incorporates RFC 3516's *fetch* half, so
// BINARY[] is legal for a session that enabled IMAP4rev2 whether or not the
// BINARY token — which additionally claims the APPEND half rev2 did not
// incorporate — was ever advertised. Gating on the token answered NO [CANNOT]
// to a client the server had just told to ENABLE IMAP4REV2.
//
// The framework already knew the right answer: featureBinaryFetch says
// "rev2 or the token". The gate now asks that predicate instead of re-deriving
// it, and this test is what keeps the two from drifting apart again.
func TestParseAdmitsImpliesGateAdmits(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		denyBinary bool
		enableRev2 bool
		want       string
	}{
		{name: "rev2-without-binary-token", denyBinary: true, enableRev2: true, want: "OK"},
		{name: "rev1-with-binary-token", denyBinary: false, enableRev2: false, want: "OK"},
		{name: "rev1-without-binary-token", denyBinary: true, enableRev2: false, want: "NO"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			deny := map[string]bool{}
			if testCase.denyBinary {
				deny["BINARY"] = true
			}
			backend := &wrappingBackend{
				inner: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}),
				wrap:  func(session imapserver.Session) imapserver.Session { return session },
				deny:  deny,
			}
			server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})

			serverSide, clientSide := net.Pipe()
			go func() { _ = server.ServeConn(ctx, serverSide, nil) }()
			reader := bufio.NewReader(clientSide)
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			writeRawCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
			if _, tagged := collectUntilTag(t, reader, "A1 "); !strings.HasPrefix(tagged, "A1 OK") {
				t.Fatalf("LOGIN failed: %q", tagged)
			}
			if testCase.enableRev2 {
				writeRawCommand(t, clientSide, "A2 ENABLE IMAP4REV2\r\n")
				untagged, tagged := collectUntilTag(t, reader, "A2 ")
				if !strings.HasPrefix(tagged, "A2 OK") {
					t.Fatalf("ENABLE failed: %q", tagged)
				}
				enabled := false
				for _, line := range untagged {
					if strings.Contains(line, "ENABLED") && strings.Contains(line, "IMAP4REV2") {
						enabled = true
					}
				}
				if !enabled {
					t.Fatalf("IMAP4REV2 was not enabled; the test would prove nothing: %v", untagged)
				}
			}
			writeRawCommand(t, clientSide, "A3 SELECT INBOX\r\n")
			if _, tagged := collectUntilTag(t, reader, "A3 "); !strings.HasPrefix(tagged, "A3 OK") {
				t.Fatalf("SELECT failed: %q", tagged)
			}

			writeRawCommand(t, clientSide, "A4 FETCH 1 (BINARY[])\r\n")
			_, tagged := collectUntilTag(t, reader, "A4 ")
			if !strings.HasPrefix(tagged, "A4 "+testCase.want) {
				t.Errorf("FETCH BINARY[] = %q, want %s", tagged, testCase.want)
			}
			// A refusal must be *this* refusal. Asserting the NO prefix alone
			// would pass on an unrelated failure and quietly stop testing the
			// gate.
			if testCase.want == "NO" && !strings.Contains(tagged, "[CANNOT]") {
				t.Errorf("FETCH BINARY[] refused without the CANNOT code: %q", tagged)
			}
		})
	}
}
