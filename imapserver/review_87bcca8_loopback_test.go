package imapserver_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// Regressions for the independent T33 review of 87bcca8 (R1–R7).

// review87Conn starts one connection on server and logs in. done closes once
// ServeConn has returned, which is after the connection's backend cleanup.
func review87Conn(t *testing.T, server *imapserver.Server) (net.Conn, *bufio.Reader, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	_ = client.SetDeadline(time.Now().Add(20 * time.Second))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.ServeConn(ctx, peer, nil)
	}()
	reader := bufio.NewReader(client)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	reviewCommand(t, client, reader, "L", "LOGIN alice secret", "OK")
	return client, reader, done
}

func review87Wire(lines []string) string { return strings.Join(lines, " | ") }

// resyncFailureBackend injects QRESYNC SELECT failures and records the
// release of every selection it hands out.
type resyncFailureBackend struct {
	*memory.Backend
	mu         sync.Mutex
	resync     func(context.Context) error
	fetchErr   error
	updaters   []*imapserver.Updater
	unselects  atomic.Int32
	releaseErr []error
}

type resyncFailureSession struct {
	imapserver.Session
	owner *resyncFailureBackend
}

type resyncFailureSelected struct {
	imapserver.SelectedMailbox
	owner *resyncFailureBackend
}

func (b *resyncFailureBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &resyncFailureSession{Session: s, owner: b}, nil
}

func (s *resyncFailureSession) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, name, updater, options)
	if err == nil {
		s.owner.mu.Lock()
		s.owner.updaters = append(s.owner.updaters, updater)
		s.owner.mu.Unlock()
		result.Mailbox = &resyncFailureSelected{SelectedMailbox: result.Mailbox, owner: s.owner}
	}
	return result, err
}

func (m *resyncFailureSelected) Resync(ctx context.Context, params *imapserver.QResyncSelect, options *imapserver.QResyncOptions) (*imapserver.QResyncResult, error) {
	m.owner.mu.Lock()
	fail := m.owner.resync
	m.owner.mu.Unlock()
	if fail != nil {
		if err := fail(ctx); err != nil {
			return nil, err
		}
	}
	return m.SelectedMailbox.(imapserver.QResyncMailbox).Resync(ctx, params, options)
}

func (m *resyncFailureSelected) Fetch(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, options *imapserver.FetchOptions) error {
	m.owner.mu.Lock()
	err := m.owner.fetchErr
	m.owner.mu.Unlock()
	if err != nil {
		return err
	}
	return m.SelectedMailbox.Fetch(ctx, writer, uids, options)
}

func (m *resyncFailureSelected) Unselect(ctx context.Context, options *imapserver.UnselectOptions) error {
	m.owner.unselects.Add(1)
	m.owner.mu.Lock()
	m.owner.releaseErr = append(m.owner.releaseErr, ctx.Err())
	m.owner.mu.Unlock()
	return m.SelectedMailbox.Unselect(ctx, options)
}

func (b *resyncFailureBackend) inject(resync func(context.Context) error, fetchErr error) {
	b.mu.Lock()
	b.resync, b.fetchErr = resync, fetchErr
	b.mu.Unlock()
}

func (b *resyncFailureBackend) lastUpdater() *imapserver.Updater {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.updaters[len(b.updaters)-1]
}

// R1: a SELECT or EXAMINE that fails in Resync or in the changed-message FETCH
// leaves nothing selected, invalidates the updater and releases the backend
// handle exactly once before the tagged response, with the error mapped as
// for any other backend failure.
func TestReview87FailedQResyncSelectReleasesSelection(t *testing.T) {
	private := errors.New("private resync diagnostic")
	cases := []struct {
		name     string
		resync   func(context.Context) error
		fetchErr error
		timeout  time.Duration
		want     string
	}{
		{name: "private", resync: func(context.Context) error { return private }, want: "NO [SERVERBUG] "},
		{name: "protocol-no", resync: func(context.Context) error {
			return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeCannot, Text: "resync refused"}
		}, want: "NO [CANNOT] resync refused"},
		{name: "protocol-bad", resync: func(context.Context) error {
			return fmt.Errorf("wrapped: %w", &imap.Error{Type: imap.ErrorTypeBad, Code: imap.CodeLimit, CodeArgs: "5", Text: "too much"})
		}, want: "BAD [LIMIT 5] too much"},
		{name: "cancelled", resync: func(context.Context) error { return fmt.Errorf("wrapped: %w", context.Canceled) }, want: "NO [UNAVAILABLE] "},
		{name: "timeout", resync: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, timeout: 150 * time.Millisecond, want: "NO [UNAVAILABLE] "},
		{name: "changed-fetch", fetchErr: private, want: "NO [SERVERBUG] "},
	}
	for _, tc := range cases {
		for _, mode := range []string{"first", "replacement", "examine"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				backend := &resyncFailureBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
				events := make(chan *imapserver.ServerEvent, 16)
				options := &imapserver.Options{AllowInsecureAuth: true, OnError: func(_ context.Context, event *imapserver.ServerEvent) {
					select {
					case events <- event:
					default:
					}
				}}
				if tc.timeout > 0 {
					options.Limits.CommandTimeout = tc.timeout
				}
				server := imapserver.New(backend, options)
				t.Cleanup(func() { _ = server.Close(context.Background(), nil) })
				client, reader, done := review87Conn(t, server)
				other, others, _ := review87Conn(t, server)
				raw := "Subject: seed\r\n\r\nbody"
				reviewCommand(t, other, others, "A", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
				reviewCommand(t, other, others, "C", "CREATE other", "OK")
				status := review87Wire(reviewCommand(t, client, reader, "U", "STATUS INBOX (UIDVALIDITY)", "OK"))
				var validity uint32
				if _, err := fmt.Sscanf(status[strings.Index(status, "UIDVALIDITY"):], "UIDVALIDITY %d", &validity); err != nil {
					t.Fatalf("STATUS = %q: %v", status, err)
				}
				reviewCommand(t, client, reader, "E", "ENABLE QRESYNC", "OK")
				released := int32(0)
				if mode == "replacement" {
					reviewCommand(t, client, reader, "P", "SELECT other", "OK")
					released = 1
				}
				backend.inject(tc.resync, tc.fetchErr)
				verb := "SELECT"
				if mode == "examine" {
					verb = "EXAMINE"
				}
				// MODSEQ 1 predates the seeded message, so the report has a
				// non-empty Changed set and the changed-message FETCH runs.
				writeRawCommand(t, client, fmt.Sprintf("S %s INBOX (QRESYNC (%d 1))\r\n", verb, validity))
				lines, tagged := collectUntilTag(t, reader, "S ")
				wire := review87Wire(append(lines, tagged))
				if !strings.HasPrefix(tagged, "S "+tc.want) {
					t.Fatalf("failed %s = %q, want %q", verb, wire, tc.want)
				}
				if strings.Contains(wire, "private") {
					t.Fatalf("private diagnostic reached the wire: %q", wire)
				}
				if got := strings.Contains(wire, "[CLOSED]"); got != (mode == "replacement") {
					t.Fatalf("CLOSED for %s = %v: %q", mode, got, wire)
				}
				// The release happened before the tagged response, on a live
				// context even when the command's own context had expired.
				if got := backend.unselects.Load(); got != released+1 {
					t.Fatalf("Unselect calls before the tagged response = %d, want %d", got, released+1)
				}
				backend.mu.Lock()
				for i, err := range backend.releaseErr {
					if err != nil {
						t.Errorf("Unselect %d ran on a done context: %v", i, err)
					}
				}
				backend.mu.Unlock()
				if err := backend.lastUpdater().Push(&imapserver.UpdateBatch{Before: "x", After: "y"}); !errors.Is(err, imapserver.ErrUpdaterClosed) {
					t.Fatalf("failed selection's updater Push = %v, want ErrUpdaterClosed", err)
				}
				backend.inject(nil, nil)
				// Selected-state commands are refused: the connection is in the
				// authenticated state, as the client was told.
				reviewCommand(t, client, reader, "F", "UID FETCH 1:* (UID)", "BAD")
				reviewCommand(t, other, others, "B", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
				if noop := review87Wire(reviewCommand(t, client, reader, "N", "NOOP", "OK")); strings.Contains(noop, "EXISTS") {
					t.Fatalf("released selection still delivered updates: %q", noop)
				}
				if tc.name == "private" {
					select {
					case event := <-events:
						if !errors.Is(event.Err, private) {
							t.Fatalf("diagnostic = %#v, want the private Resync error", event)
						}
					case <-time.After(5 * time.Second):
						t.Fatal("private Resync error was not reported to OnError")
					}
				}
				// The connection remains usable, and teardown releases only the
				// new selection: the failed one is never released twice.
				reviewCommand(t, client, reader, "R", "SELECT INBOX", "OK")
				reviewCommand(t, client, reader, "O", "LOGOUT", "OK")
				<-done
				if got := backend.unselects.Load(); got != released+2 {
					t.Fatalf("Unselect calls after teardown = %d, want %d", got, released+2)
				}
			})
		}
	}
}

// review87Seed appends count messages to INBOX through a separate session.
func review87Seed(t *testing.T, backend imapserver.Backend, count int) {
	t.Helper()
	seeder, reader := reviewRawSession(t, backend, nil)
	for i := range count {
		raw := fmt.Sprintf("Subject: seed %d\r\n\r\nbody %d", i, i)
		reviewCommand(t, seeder, reader, fmt.Sprintf("A%d", i), fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
	}
	reviewCommand(t, seeder, reader, "Z", "LOGOUT", "OK")
}

// review87Responses regroups the LF-split lines of reviewCommand into whole
// responses: a literal's octets, and the rest of the line after them, belong
// to the response that announced the literal.
func review87Responses(lines []string) []string {
	var responses []string
	pending, continues := 0, false
	for _, line := range lines {
		rest := line
		switch {
		case pending > 0:
			responses[len(responses)-1] += "\r\n" + line
			if len(line) < pending {
				// The literal spans this whole line and its CRLF.
				pending -= len(line) + 2
				continues = pending <= 0
				continue
			}
			rest, pending = line[pending:], 0
		case continues:
			responses[len(responses)-1] += line
		default:
			responses = append(responses, line)
		}
		continues = false
		if open := strings.LastIndex(rest, "{"); open >= 0 && strings.HasSuffix(rest, "}") {
			if _, err := fmt.Sscanf(strings.TrimSuffix(rest[open+1:], "}"), "%d", &pending); err != nil {
				pending = 0
			}
			continues = pending == 0
		}
	}
	return responses
}

// review87Fetch returns the untagged FETCH and UIDFETCH responses in lines.
func review87Fetch(lines []string) []string {
	var fetches []string
	for _, response := range review87Responses(lines) {
		if strings.HasPrefix(response, "* ") && (strings.Contains(response, " FETCH (") || strings.Contains(response, " UIDFETCH (")) {
			fetches = append(fetches, response)
		}
	}
	return fetches
}

func review87ModSeq(t *testing.T, line string) uint64 {
	t.Helper()
	at := strings.Index(line, "MODSEQ (")
	if at < 0 {
		t.Fatalf("no MODSEQ in %q", line)
	}
	var value uint64
	if _, err := fmt.Sscanf(line[at:], "MODSEQ (%d)", &value); err != nil {
		t.Fatalf("MODSEQ in %q: %v", line, err)
	}
	return value
}

// R2: a successful conditional STORE.SILENT reports MODSEQ (and UID) for every
// stored message, without FLAGS; MODIFIED still names the refused messages;
// and the reported clock is usable by the next conditional command.
func TestReview87ConditionalSilentStore(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		for _, form := range []string{"", "UID "} {
			t.Run(kind+"/"+strings.TrimSpace(form+"STORE"), func(t *testing.T) {
				backend := reviewBackend(t, kind)
				review87Seed(t, backend, 2)
				client, reader := reviewRawSession(t, backend, nil)
				watcher, watches := reviewRawSession(t, backend, nil)
				reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
				reviewCommand(t, watcher, watches, "W", "SELECT INBOX", "OK")
				// Make message 2's clock newer than message 1's. The first
				// UNCHANGEDSINCE below is CONDSTORE's enabling command.
				reviewCommand(t, client, reader, "P", `STORE 2 +FLAGS (\Answered)`, "OK")
				clocks := review87Fetch(reviewCommand(t, client, reader, "M", "FETCH 1:2 (MODSEQ)", "OK"))
				if len(clocks) != 2 {
					t.Fatalf("FETCH MODSEQ = %q", clocks)
				}
				first := review87ModSeq(t, clocks[0])

				// Partial: message 1 passes, message 2 is refused.
				lines := reviewCommand(t, client, reader, "C", form+`STORE 1:2 (UNCHANGEDSINCE `+fmt.Sprint(first)+`) +FLAGS.SILENT (\Flagged)`, "OK")
				wire := review87Wire(lines)
				fetches := review87Fetch(lines)
				if len(fetches) != 1 || !strings.HasPrefix(fetches[0], "* 1 FETCH (") || !strings.Contains(fetches[0], "UID 1") || strings.Contains(fetches[0], "FLAGS") {
					t.Fatalf("conditional .SILENT responses = %q, want one MODSEQ+UID FETCH for message 1 without FLAGS", wire)
				}
				stored := review87ModSeq(t, fetches[0])
				if stored <= first {
					t.Fatalf("stored MODSEQ %d does not advance %d", stored, first)
				}
				modified := "[MODIFIED 2]"
				if !strings.Contains(lines[len(lines)-1], modified) {
					t.Fatalf("tagged response = %q, want %s", lines[len(lines)-1], modified)
				}
				// The response accounted for the change: no duplicate FETCH.
				if after := review87Fetch(reviewCommand(t, client, reader, "N", "NOOP", "OK")); len(after) != 0 {
					t.Fatalf("own conditional store delivered again: %q", after)
				}
				// Another session still hears about it, with the same clock.
				seen := review87Fetch(reviewCommand(t, watcher, watches, "V", "NOOP", "OK"))
				if !slices.ContainsFunc(seen, func(line string) bool {
					return strings.HasPrefix(line, "* 1 FETCH (") && strings.Contains(line, `\Flagged`)
				}) {
					t.Fatalf("other session = %q", seen)
				}
				// The reported clock is current: the next conditional store
				// with it succeeds, and the state matches what was reported.
				state := review87Fetch(reviewCommand(t, client, reader, "F", "FETCH 1 (FLAGS MODSEQ)", "OK"))
				if len(state) != 1 || review87ModSeq(t, state[0]) != stored || !strings.Contains(state[0], `\Flagged`) {
					t.Fatalf("stored state = %q, reported MODSEQ %d", state, stored)
				}
				lines = reviewCommand(t, client, reader, "R", form+`STORE 1 (UNCHANGEDSINCE `+fmt.Sprint(stored)+`) -FLAGS.SILENT (\Flagged)`, "OK")
				if strings.Contains(lines[len(lines)-1], "MODIFIED") || len(review87Fetch(lines)) != 1 {
					t.Fatalf("retry with the reported clock = %q", lines)
				}
				// Rejected: UNCHANGEDSINCE 0 refuses every message and
				// reports no new clock.
				lines = reviewCommand(t, client, reader, "X", form+`STORE 1:2 (UNCHANGEDSINCE 0) +FLAGS.SILENT (\Deleted)`, "OK")
				if !strings.Contains(lines[len(lines)-1], "[MODIFIED ") {
					t.Fatalf("rejected store = %q", lines)
				}
				for _, line := range review87Fetch(lines) {
					if !strings.Contains(line, "FLAGS") {
						t.Fatalf("rejected store reported a new clock: %q", lines)
					}
				}
				// An unconditional .SILENT store may stay silent.
				if fetches := review87Fetch(reviewCommand(t, client, reader, "Q", form+`STORE 1 +FLAGS.SILENT (\Seen)`, "OK")); len(fetches) != 0 {
					t.Fatalf("unconditional .SILENT = %q", fetches)
				}
			})
		}
	}
}

// silentHonouringBackend writes nothing from StoreCondStore under Silent, as
// the StoreOptions.Silent documentation permits, and nothing from Store when
// underReport is set, which a correct backend would never do.
type silentHonouringBackend struct {
	*memory.Backend
	underReport bool
}

type silentHonouringSession struct {
	imapserver.Session
	owner *silentHonouringBackend
}

type silentHonouringSelected struct {
	imapserver.SelectedMailbox
	inner imapserver.CondStoreMailbox
	owner *silentHonouringBackend
}

func (b *silentHonouringBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &silentHonouringSession{Session: s, owner: b}, nil
}

func (s *silentHonouringSession) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, name, updater, options)
	if err == nil {
		result.Mailbox = &silentHonouringSelected{SelectedMailbox: result.Mailbox, inner: result.Mailbox.(imapserver.CondStoreMailbox), owner: s.owner}
	}
	return result, err
}

func discardingFetchWriter() *imapserver.FetchWriter {
	return &imapserver.FetchWriter{WriteFunc: func(context.Context, *imap.FetchMessageData) error { return nil }}
}

func (m *silentHonouringSelected) StoreCondStore(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, flags *imapserver.StoreFlags, options *imapserver.StoreOptions) (*imapserver.CondStoreResult, error) {
	if options != nil && options.Silent {
		writer = discardingFetchWriter()
	}
	return m.inner.StoreCondStore(ctx, writer, uids, flags, options)
}

func (m *silentHonouringSelected) Store(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, flags *imapserver.StoreFlags, options *imapserver.StoreOptions) error {
	if m.owner.underReport {
		writer = discardingFetchWriter()
	}
	return m.SelectedMailbox.Store(ctx, writer, uids, flags, options)
}

// R2: the mandatory MODSEQ does not depend on a backend ignoring Silent, and
// the framework accounts only for what the response actually reported.
func TestReview87StoreReportsWhatTheBackendDidNot(t *testing.T) {
	t.Run("silent-condstore", func(t *testing.T) {
		backend := &silentHonouringBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
		review87Seed(t, backend, 1)
		client, reader := reviewRawSession(t, backend, nil)
		reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
		fetches := review87Fetch(reviewCommand(t, client, reader, "C", `STORE 1 (UNCHANGEDSINCE 100) +FLAGS.SILENT (\Flagged)`, "OK"))
		if len(fetches) != 1 || !strings.Contains(fetches[0], "UID 1") || strings.Contains(fetches[0], "FLAGS") {
			t.Fatalf("conditional .SILENT with a silent backend = %q", fetches)
		}
		state := review87Fetch(reviewCommand(t, client, reader, "F", "FETCH 1 (MODSEQ)", "OK"))
		if len(state) != 1 || review87ModSeq(t, state[0]) != review87ModSeq(t, fetches[0]) {
			t.Fatalf("reported %q, stored %q", fetches, state)
		}
	})
	t.Run("under-reporting-store", func(t *testing.T) {
		backend := &silentHonouringBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), underReport: true}
		review87Seed(t, backend, 1)
		client, reader := reviewRawSession(t, backend, nil)
		reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
		lines := reviewCommand(t, client, reader, "C", `STORE 1 +FLAGS (\Flagged)`, "OK")
		lines = append(lines, reviewCommand(t, client, reader, "N", "NOOP", "OK")...)
		fetches := review87Fetch(lines)
		if len(fetches) != 1 || !strings.Contains(fetches[0], `\Flagged`) {
			t.Fatalf("an unreported change was suppressed as accounted for: %q", lines)
		}
	})
}

// R3: once any CONDSTORE enabling command has been issued, untagged FETCH
// responses caused by STORE, by a FETCH asking for MODSEQ or implicitly
// setting \Seen, and by other sessions carry UID; the RFC's FETCH
// exceptions stay UID-less.
func TestReview87CondStoreFetchCarriesUID(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		for _, enabling := range []string{"ENABLE CONDSTORE", "ENABLE QRESYNC", "SELECT INBOX (CONDSTORE)", "FETCH 2 (MODSEQ)", "STATUS INBOX (HIGHESTMODSEQ)", "SEARCH MODSEQ 1"} {
			t.Run(kind+"/"+enabling, func(t *testing.T) {
				backend := reviewBackend(t, kind)
				review87Seed(t, backend, 3)
				client, reader := reviewRawSession(t, backend, nil)
				other, others := reviewRawSession(t, backend, nil)
				if strings.HasPrefix(enabling, "ENABLE") {
					reviewCommand(t, client, reader, "E", enabling, "OK")
				}
				reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
				reviewCommand(t, other, others, "S", "SELECT INBOX", "OK")
				if !strings.HasPrefix(enabling, "ENABLE") {
					reviewCommand(t, client, reader, "E", enabling, "OK")
				}
				check := func(command string, uid bool) {
					t.Helper()
					fetches := review87Fetch(reviewCommand(t, client, reader, "T", command, "OK"))
					if len(fetches) == 0 {
						t.Fatalf("%s: no FETCH", command)
					}
					for _, line := range fetches {
						if got := strings.Contains(line, " UID "); got != uid {
							t.Fatalf("%s: UID present = %v, want %v: %q", command, got, uid, fetches)
						}
						if !strings.Contains(line, "MODSEQ (") {
							t.Fatalf("%s: no MODSEQ: %q", command, fetches)
						}
					}
				}
				check(`STORE 1 +FLAGS (\Flagged)`, true)
				check("FETCH 1 (MODSEQ)", true)
				check("FETCH 1 (FLAGS)", false)             // asked for neither UID nor MODSEQ
				check("FETCH 1 (BODY.PEEK[HEADER])", false) // and sets nothing
				check("UID FETCH 1 (FLAGS)", true)
				check("FETCH 3 (BODY[TEXT])", true) // implicitly sets \Seen
				reviewCommand(t, other, others, "X", `STORE 1 +FLAGS (\Answered)`, "OK")
				check("NOOP", true)
			})
		}
	}
	// Without an enabling command nothing changes.
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind+"/not-enabled", func(t *testing.T) {
			backend := reviewBackend(t, kind)
			review87Seed(t, backend, 1)
			client, reader := reviewRawSession(t, backend, nil)
			other, others := reviewRawSession(t, backend, nil)
			reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
			reviewCommand(t, other, others, "S", "SELECT INBOX", "OK")
			lines := reviewCommand(t, client, reader, "T", `STORE 1 +FLAGS (\Flagged)`, "OK")
			reviewCommand(t, other, others, "X", `STORE 1 +FLAGS (\Answered)`, "OK")
			lines = append(lines, reviewCommand(t, client, reader, "N", "NOOP", "OK")...)
			for _, line := range review87Fetch(lines) {
				if strings.Contains(line, "UID") || strings.Contains(line, "MODSEQ") {
					t.Fatalf("CONDSTORE data without an enabling command: %q", line)
				}
			}
		})
	}
}

// R3: UIDONLY keeps its framing, and a selected FlagChange registration
// carries UID even without CONDSTORE (RFC 5465 section 5.1).
func TestReview87UnsolicitedFlagUIDFraming(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind+"/uidonly", func(t *testing.T) {
			backend := reviewBackend(t, kind)
			review87Seed(t, backend, 1)
			client, reader := reviewRawSession(t, backend, nil)
			other, others := reviewRawSession(t, backend, nil)
			reviewCommand(t, client, reader, "E", "ENABLE UIDONLY CONDSTORE", "OK")
			reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
			reviewCommand(t, other, others, "S", "SELECT INBOX", "OK")
			reviewCommand(t, other, others, "X", `STORE 1 +FLAGS (\Answered)`, "OK")
			fetches := review87Fetch(reviewCommand(t, client, reader, "N", "NOOP", "OK"))
			if len(fetches) != 1 || !strings.HasPrefix(fetches[0], "* 1 UIDFETCH (") || strings.Contains(fetches[0], " UID ") || !strings.Contains(fetches[0], "MODSEQ") {
				t.Fatalf("UIDONLY flag update = %q", fetches)
			}
		})
		t.Run(kind+"/notify-flagchange", func(t *testing.T) {
			backend := reviewBackend(t, kind)
			review87Seed(t, backend, 1)
			client, reader := reviewRawSession(t, backend, nil)
			if !strings.Contains(review87Wire(reviewCommand(t, client, reader, "C", "CAPABILITY", "OK")), " NOTIFY") {
				t.Skip("backend does not advertise NOTIFY")
			}
			other, others := reviewRawSession(t, backend, nil)
			reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
			reviewCommand(t, other, others, "S", "SELECT INBOX", "OK")
			reviewCommand(t, client, reader, "N", "NOTIFY SET (selected (MessageNew MessageExpunge FlagChange))", "OK")
			reviewCommand(t, other, others, "X", `STORE 1 +FLAGS (\Flagged)`, "OK")
			fetches := review87Fetch(reviewCommand(t, client, reader, "Q", "NOOP", "OK"))
			if len(fetches) != 1 || !strings.Contains(fetches[0], "UID 1") || !strings.Contains(fetches[0], "FLAGS (") || strings.Contains(fetches[0], "MODSEQ") {
				t.Fatalf("selected FlagChange = %q", fetches)
			}
		})
	}
}

// review87Kinds classifies untagged selected-mailbox responses.
func review87Kinds(lines []string) (exists, expunges, fetches, recent, flags int) {
	for _, response := range review87Responses(lines) {
		switch {
		case strings.HasSuffix(response, " EXISTS"):
			exists++
		case strings.HasSuffix(response, " EXPUNGE"), strings.HasPrefix(response, "* VANISHED "):
			expunges++
		case strings.HasSuffix(response, " RECENT"):
			recent++
		case strings.HasPrefix(response, "* FLAGS "):
			flags++
		case strings.Contains(response, " FETCH ("):
			fetches++
		}
	}
	return
}

// R5: each selected policy reports exactly the events it registered, for
// changes made by another session.
func TestReview87SelectedNotifyPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, notify    string
		messages, flags bool
	}{
		{"none", "NOTIFY NONE", false, false},
		{"selected-none", "NOTIFY SET (selected NONE)", false, false},
		{"omitted", "NOTIFY SET (personal (MessageNew MessageExpunge FlagChange))", false, false},
		{"messages", "NOTIFY SET (selected (MessageNew MessageExpunge))", true, false},
		{"delayed-messages", "NOTIFY SET (selected-delayed (MessageNew MessageExpunge))", true, false},
		{"everything", "NOTIFY SET (selected (MessageNew MessageExpunge FlagChange))", true, true},
		{"never-registered", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := reviewBackend(t, "memory")
			review87Seed(t, backend, 2)
			client, reader := reviewRawSession(t, backend, nil)
			other, others := reviewRawSession(t, backend, nil)
			reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
			reviewCommand(t, other, others, "S", "SELECT INBOX", "OK")
			if tc.notify != "" {
				reviewCommand(t, client, reader, "N", tc.notify, "OK")
			}
			raw := "Subject: new\r\n\r\nbody"
			reviewCommand(t, other, others, "F", `STORE 1 +FLAGS (\Flagged $Fresh)`, "OK")
			reviewCommand(t, other, others, "A", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
			reviewCommand(t, other, others, "D", `STORE 2 +FLAGS.SILENT (\Deleted)`, "OK")
			reviewCommand(t, other, others, "X", "EXPUNGE", "OK")
			lines := reviewCommand(t, client, reader, "Q", "NOOP", "OK")
			exists, expunges, fetches, recent, flags := review87Kinds(lines)
			if got := exists != 0 && expunges != 0; got != tc.messages || !tc.messages && recent != 0 {
				t.Fatalf("message events delivered = %v (EXISTS %d EXPUNGE %d RECENT %d), want %v: %q", got, exists, expunges, recent, tc.messages, lines)
			}
			if got := fetches != 0 && flags != 0; got != tc.flags || !tc.flags && (fetches != 0 || flags != 0) {
				t.Fatalf("flag events delivered = %v (FETCH %d FLAGS %d), want %v: %q", got, fetches, flags, tc.flags, lines)
			}
			if tc.flags && tc.notify != "" {
				for _, line := range review87Fetch(lines) {
					if !strings.Contains(line, "UID ") {
						t.Fatalf("selected FlagChange without UID: %q", line)
					}
				}
			}
		})
	}
}

// R5: while message events are suppressed the client's sequence view does not
// change under it. Its own commands still report their own effects in that
// view, and a NOTIFY SET returns everything accumulated before the new
// registration applies.
func TestReview87SuppressedSelectedView(t *testing.T) {
	backend := reviewBackend(t, "memory")
	review87Seed(t, backend, 3)
	client, reader := reviewRawSession(t, backend, nil)
	other, others := reviewRawSession(t, backend, nil)
	reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
	reviewCommand(t, other, others, "S", "SELECT INBOX", "OK")
	reviewCommand(t, client, reader, "N", "NOTIFY NONE", "OK")
	// Another session removes UID 1 and adds UID 4.
	reviewCommand(t, other, others, "D", `STORE 1 +FLAGS.SILENT (\Deleted)`, "OK")
	reviewCommand(t, other, others, "X", "EXPUNGE", "OK")
	raw := "Subject: new\r\n\r\nbody"
	reviewCommand(t, other, others, "A", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
	expect := func(tag, command, want string) []string {
		t.Helper()
		lines := reviewCommand(t, client, reader, tag, command, "OK")
		if got := review87Wire(lines[:len(lines)-1]); got != want {
			t.Fatalf("%s = %q, want %q", command, got, want)
		}
		return lines
	}
	// Still three messages numbered as before: UID 1 is unannounced, so it
	// keeps sequence number 1 and simply has no data; UID 4 is not visible.
	expect("Q", "NOOP", "")
	expect("F1", "FETCH 1:* (UID)", "* 2 FETCH (UID 2) | * 3 FETCH (UID 3)")
	expect("F2", "UID FETCH 4 (UID)", "")
	expect("F3", "SEARCH ALL", "* SEARCH 2 3")
	expect("F4", "UID SEARCH ALL", "* SEARCH 2 3")
	// Its own commands report their own effects in that view: UID 3 is still
	// message 3, and its EXPUNGE does not announce UID 1 along with it.
	expect("O1", `STORE 3 +FLAGS (\Deleted)`, `* 3 FETCH (FLAGS (\Deleted \Recent))`)
	expect("O2", "EXPUNGE", "* 3 EXPUNGE")
	// A NOTIFY SET returns what accumulated, even though the new
	// registration (SELECTED NONE) suppresses what follows.
	lines := reviewCommand(t, client, reader, "R", "NOTIFY SET (selected NONE)", "OK")
	exists, expunges, _, _, _ := review87Kinds(lines)
	if expunges != 1 || exists != 1 || !slices.Contains(lines, "* 1 EXPUNGE") || !slices.Contains(lines, "* 2 EXISTS") {
		t.Fatalf("implicit NOOP = %q, want the withheld EXPUNGE of UID 1 and the addition", lines)
	}
	expect("F5", "FETCH 1:* (UID)", "* 1 FETCH (UID 2) | * 2 FETCH (UID 4)")
	// Its own APPEND is reported even while suppressed, after any addition
	// it follows: EXISTS counts both.
	reviewCommand(t, other, others, "B", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
	lines = reviewCommand(t, client, reader, "P", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
	if !slices.Contains(lines, "* 4 EXISTS") {
		t.Fatalf("own APPEND under SELECTED NONE = %q, want * 4 EXISTS", lines)
	}
	expect("F6", "UID FETCH 1:* (UID)", "* 1 FETCH (UID 2) | * 2 FETCH (UID 4) | * 3 FETCH (UID 5) | * 4 FETCH (UID 6)")
	// Re-enabling releases nothing twice, and the new policy applies.
	reviewCommand(t, client, reader, "E", "NOTIFY SET (selected (MessageNew MessageExpunge))", "OK")
	reviewCommand(t, other, others, "C", `STORE 1 +FLAGS (\Answered)`, "OK")
	reviewCommand(t, other, others, "G", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
	lines = reviewCommand(t, client, reader, "Q2", "NOOP", "OK")
	if exists, _, fetches, _, _ := review87Kinds(lines); exists != 1 || fetches != 0 || !slices.Contains(lines, "* 5 EXISTS") {
		t.Fatalf("after re-enabling messages only = %q", lines)
	}
}

// R5: IDLE delivers exactly what the registration asks for, SELECTED reports
// removals as they happen, SELECTED-DELAYED waits for a command that allows
// them, and a new selection starts from a clean view.
func TestReview87SelectedNotifyTiming(t *testing.T) {
	readQuiet := func(t *testing.T, client net.Conn, reader *bufio.Reader, wait time.Duration) []string {
		t.Helper()
		var lines []string
		for {
			_ = client.SetReadDeadline(time.Now().Add(wait))
			line, err := reader.ReadString('\n')
			if err != nil {
				_ = client.SetReadDeadline(time.Now().Add(20 * time.Second))
				return lines
			}
			lines = append(lines, strings.TrimRight(line, "\r\n"))
		}
	}
	expunge := func(t *testing.T, other net.Conn, others *bufio.Reader) {
		t.Helper()
		reviewCommand(t, other, others, "D", `STORE 1 +FLAGS.SILENT (\Deleted)`, "OK")
		reviewCommand(t, other, others, "X", "EXPUNGE", "OK")
	}
	for _, tc := range []struct {
		name, notify string
		immediate    bool
	}{
		{"selected", "NOTIFY SET (selected (MessageNew MessageExpunge))", true},
		{"selected-delayed", "NOTIFY SET (selected-delayed (MessageNew MessageExpunge))", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := reviewBackend(t, "memory")
			review87Seed(t, backend, 2)
			client, reader := reviewRawSession(t, backend, nil)
			other, others := reviewRawSession(t, backend, nil)
			reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
			reviewCommand(t, other, others, "S", "SELECT INBOX", "OK")
			reviewCommand(t, client, reader, "N", tc.notify, "OK")
			expunge(t, other, others)
			// No command is in progress on the client.
			quiet := readQuiet(t, client, reader, 300*time.Millisecond)
			if got := slices.Contains(quiet, "* 1 EXPUNGE"); got != tc.immediate {
				t.Fatalf("between commands = %q, immediate EXPUNGE %v", quiet, tc.immediate)
			}
			lines := reviewCommand(t, client, reader, "Q", "NOOP", "OK")
			if !tc.immediate && !slices.Contains(lines, "* 1 EXPUNGE") {
				t.Fatalf("SELECTED-DELAYED did not deliver at NOOP: %q", lines)
			}
		})
	}
	t.Run("idle", func(t *testing.T) {
		backend := reviewBackend(t, "memory")
		review87Seed(t, backend, 2)
		client, reader := reviewRawSession(t, backend, nil)
		other, others := reviewRawSession(t, backend, nil)
		reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
		reviewCommand(t, other, others, "S", "SELECT INBOX", "OK")
		reviewCommand(t, client, reader, "N", "NOTIFY SET (selected (MessageNew MessageExpunge))", "OK")
		writeRawCommand(t, client, "I IDLE\r\n")
		if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+ ") {
			t.Fatalf("IDLE continuation = %q, %v", line, err)
		}
		reviewCommand(t, other, others, "F", `STORE 1 +FLAGS (\Flagged)`, "OK")
		expunge(t, other, others)
		idled := readQuiet(t, client, reader, 300*time.Millisecond)
		writeRawCommand(t, client, "DONE\r\n")
		lines, tagged := collectUntilTag(t, reader, "I ")
		idled = append(idled, lines...)
		if !strings.HasPrefix(tagged, "I OK") || !slices.Contains(idled, "* 1 EXPUNGE") {
			t.Fatalf("IDLE = %q %q, want the EXPUNGE", idled, tagged)
		}
		if _, _, fetches, _, _ := review87Kinds(idled); fetches != 0 {
			t.Fatalf("IDLE delivered an unregistered FlagChange: %q", idled)
		}
	})
	t.Run("selection-change", func(t *testing.T) {
		backend := reviewBackend(t, "memory")
		review87Seed(t, backend, 2)
		client, reader := reviewRawSession(t, backend, nil)
		other, others := reviewRawSession(t, backend, nil)
		reviewCommand(t, client, reader, "C", "CREATE other", "OK")
		reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
		reviewCommand(t, other, others, "S", "SELECT INBOX", "OK")
		reviewCommand(t, client, reader, "N", "NOTIFY NONE", "OK")
		expunge(t, other, others)
		// The withheld removal belongs to the abandoned selection; the new
		// one starts from its own snapshot and keeps the policy.
		lines := reviewCommand(t, client, reader, "S2", "SELECT INBOX", "OK")
		if !slices.Contains(lines, "* 1 EXISTS") {
			t.Fatalf("reselect = %q", lines)
		}
		raw := "Subject: new\r\n\r\nbody"
		reviewCommand(t, other, others, "A", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
		if lines := reviewCommand(t, client, reader, "Q", "NOOP", "OK"); len(lines) != 1 {
			t.Fatalf("policy lost across SELECT: %q", lines)
		}
	})
}

// unauthenticateNotifyBackend pushes an event for the departing user while the
// framework is ending its session.
type unauthenticateNotifyBackend struct{ *memory.Backend }

type unauthenticateNotifySession struct {
	imapserver.Session
	imapserver.NotifySession
	imapserver.UnauthenticateSession
	updater *imapserver.SessionUpdater
}

func (b *unauthenticateNotifyBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &unauthenticateNotifySession{Session: s, NotifySession: s.(imapserver.NotifySession), UnauthenticateSession: s.(imapserver.UnauthenticateSession)}, nil
}

func (s *unauthenticateNotifySession) Notify(ctx context.Context, updater *imapserver.SessionUpdater, config *imapserver.NotifyConfig, options *imapserver.NotifyOptions) error {
	s.updater = updater
	return s.NotifySession.Notify(ctx, updater, config, options)
}

func (s *unauthenticateNotifySession) Unauthenticate(ctx context.Context, options *imapserver.UnauthenticateOptions) error {
	if s.updater != nil {
		_ = s.updater.Push(&imapserver.SessionUpdate{Mailbox: "private-of-alice", Status: &imap.StatusData{Mailbox: "private-of-alice", Values: map[imap.StatusItemKeyword]any{imap.StatusItemMessages: uint32(7)}}})
	}
	return s.UnauthenticateSession.Unauthenticate(ctx, options)
}

// Adjacent to R5: UNAUTHENTICATE ends the NOTIFY registration with the
// session, both the queued events and the selected policy.
func TestReview87UnauthenticateEndsNotify(t *testing.T) {
	backend := &unauthenticateNotifyBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
	review87Seed(t, backend, 1)
	client, reader := reviewRawSession(t, backend, nil)
	other, others := reviewRawSession(t, backend, nil)
	reviewCommand(t, client, reader, "N", "NOTIFY SET (personal (MessageNew MessageExpunge))", "OK")
	reviewCommand(t, client, reader, "U", "UNAUTHENTICATE", "OK")
	lines := reviewCommand(t, client, reader, "C", "CAPABILITY", "OK")
	lines = append(lines, reviewCommand(t, client, reader, "L", "LOGIN alice secret", "OK")...)
	if wire := review87Wire(lines); strings.Contains(wire, "STATUS") {
		t.Fatalf("previous session's NOTIFY event delivered after UNAUTHENTICATE: %q", wire)
	}
	// The new session starts with the default selected behaviour.
	reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
	reviewCommand(t, other, others, "S", "SELECT INBOX", "OK")
	reviewCommand(t, other, others, "F", `STORE 1 +FLAGS (\Flagged)`, "OK")
	if fetches := review87Fetch(reviewCommand(t, client, reader, "Q", "NOOP", "OK")); len(fetches) != 1 {
		t.Fatalf("selected policy survived UNAUTHENTICATE: %q", fetches)
	}
}

// countingRefusingNotifyBackend counts Notify calls and refuses one marked
// registration the way a backend with a narrower event list would.
type countingRefusingNotifyBackend struct {
	*memory.Backend
	calls atomic.Int32
}

type countingRefusingNotifySession struct {
	imapserver.Session
	imapserver.NotifySession
	owner *countingRefusingNotifyBackend
}

func (b *countingRefusingNotifyBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &countingRefusingNotifySession{Session: s, NotifySession: s.(imapserver.NotifySession), owner: b}, nil
}

func (s *countingRefusingNotifySession) Notify(ctx context.Context, updater *imapserver.SessionUpdater, config *imapserver.NotifyConfig, options *imapserver.NotifyOptions) error {
	s.owner.calls.Add(1)
	if config != nil && slices.ContainsFunc(config.Watches, func(watch imapserver.NotifyWatch) bool { return slices.Contains(watch.Names, "refused") }) {
		return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeBadEvent, CodeArgs: "(MessageNew MessageExpunge)", Text: "no flag events here"}
	}
	return s.NotifySession.Notify(ctx, updater, config, options)
}

// R6: NOTIFY's rules are applied before the backend is asked or the current
// registration is touched; invalid requests are BAD, valid but unsupported
// ones NO [BADEVENT] with the server's complete event list.
func TestReview87NotifyValidation(t *testing.T) {
	backend := &countingRefusingNotifyBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
	review87Seed(t, backend, 1)
	client, reader := reviewRawSession(t, backend, nil)
	other, others := reviewRawSession(t, backend, nil)
	reviewCommand(t, other, others, "C", "CREATE other", "OK")
	reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
	reviewCommand(t, other, others, "S", "SELECT INBOX", "OK")
	// The registration every refusal below must leave intact: STATUS for
	// "other", and nothing for the selected mailbox.
	reviewCommand(t, client, reader, "N", "NOTIFY SET (mailboxes other (MessageNew MessageExpunge)) (selected NONE)", "OK")
	calls := backend.calls.Load()
	const complete = "[BADEVENT (MessageNew MessageExpunge FlagChange)]"
	for _, tc := range []struct{ command, want string }{
		{"NOTIFY SET (personal (MessageNew))", "BAD"},
		{"NOTIFY SET (personal (MessageExpunge))", "BAD"},
		{"NOTIFY SET (personal (FlagChange))", "BAD"},
		{"NOTIFY SET (personal (FlagChange MessageNew))", "BAD"},
		{"NOTIFY SET (personal (AnnotationChange))", "BAD"},
		{"NOTIFY SET (personal (MailboxName FlagChange))", "BAD"},
		{"NOTIFY SET (selected NONE) (selected-delayed NONE)", "BAD"},
		{"NOTIFY SET (selected NONE) (SELECTED (MessageNew MessageExpunge))", "BAD"},
		{"NOTIFY SET (selected (MessageNew MessageExpunge MailboxName))", "BAD"},
		{"NOTIFY SET (selected (SubscriptionChange))", "BAD"},
		{"NOTIFY SET (personal (MessageNew (UID) MessageExpunge))", "BAD"},
		{"NOTIFY SET (personal ())", "BAD"},
		{"NOTIFY SET (mailboxes () (MessageNew MessageExpunge))", "BAD"},
		{"NOTIFY SET (personal NONE) STATUS", "BAD"},
		{"NOTIFY SET STATUS STATUS (personal NONE)", "BAD"},
		{"NOTIFY SET", "BAD"},
		{"NOTIFY SET (elsewhere (MessageNew MessageExpunge))", "BAD"},
		// A malformed request is BAD even when it also names an
		// unsupported event or fetch attributes.
		{"NOTIFY SET (personal (MailboxName)) (selected NONE) (selected NONE)", "BAD"},
		{"NOTIFY SET (personal (MessageNew (UID) MessageExpunge AnnotationChange))", "BAD"},
		{"NOTIFY SET (personal (MailboxName))", "NO " + complete},
		{"NOTIFY SET (personal (SubscriptionChange))", "NO " + complete},
		{"NOTIFY SET (personal (MailboxMetadataChange))", "NO " + complete},
		{"NOTIFY SET (personal (ServerMetadataChange))", "NO " + complete},
		{"NOTIFY SET (personal (FutureEvent))", "NO " + complete},
		{"NOTIFY SET (personal (MessageNew MessageExpunge AnnotationChange))", "NO " + complete},
		{"NOTIFY SET (selected (MessageNew MessageExpunge FutureMessageEvent))", "NO " + complete},
		{"NOTIFY SET (selected (MessageNew (UID BODY[TEXT]) MessageExpunge))", "NO [CANNOT]"},
	} {
		lines := reviewCommand(t, client, reader, "V", tc.command, strings.Fields(tc.want)[0])
		if !strings.HasPrefix(lines[len(lines)-1], "V "+tc.want) {
			t.Errorf("%s = %q, want %s", tc.command, lines[len(lines)-1], tc.want)
		}
	}
	if got := backend.calls.Load(); got != calls {
		t.Fatalf("refused requests reached the backend: Notify calls %d, was %d", got, calls)
	}
	// Both halves of the previous registration survived every refusal.
	raw := "Subject: new\r\n\r\nbody"
	reviewCommand(t, other, others, "A", fmt.Sprintf("APPEND other {%d+}\r\n%s", len(raw), raw), "OK")
	reviewCommand(t, other, others, "B", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
	lines := reviewCommand(t, client, reader, "Q", "NOOP", "OK")
	if wire := review87Wire(lines); !strings.Contains(wire, "* STATUS other") || strings.Contains(wire, "EXISTS") {
		t.Fatalf("registration after refusals = %q", wire)
	}
	// Legal spellings in any case are accepted.
	reviewCommand(t, client, reader, "L", "NOTIFY SET status (SELECTED-delayed (messagenew MESSAGEEXPUNGE flagchange)) (Subtree (other INBOX) NONE) (inboxes (MessageNew MessageExpunge))", "OK")
	// A backend's narrower BADEVENT list becomes the server's complete one.
	lines = reviewCommand(t, client, reader, "R", "NOTIFY SET (mailboxes refused (MessageNew MessageExpunge FlagChange))", "NO")
	if tagged := lines[len(lines)-1]; !strings.Contains(tagged, complete) || !strings.Contains(tagged, "no flag events here") {
		t.Fatalf("backend BADEVENT = %q", tagged)
	}
	// The selected policy changes only on success: SELECTED-DELAYED with
	// FlagChange, set above, still applies after the backend refusal.
	reviewCommand(t, other, others, "F", `STORE 1 +FLAGS (\Flagged)`, "OK")
	if fetches := review87Fetch(reviewCommand(t, client, reader, "Q2", "NOOP", "OK")); len(fetches) != 1 || !strings.Contains(fetches[0], "UID ") {
		t.Fatalf("selected policy after a backend refusal = %q", fetches)
	}
}

func review87Number(t *testing.T, lines []string, format string) uint64 {
	t.Helper()
	prefix, _, _ := strings.Cut(format, "%")
	for _, line := range lines {
		if at := strings.Index(line, prefix); at >= 0 {
			var value uint64
			if _, err := fmt.Sscanf(line[at:], format, &value); err == nil {
				return value
			}
		}
	}
	t.Fatalf("no %q in %q", format, lines)
	return 0
}

// R7: RENAME INBOX moves INBOX's messages to a new mailbox, atomically and
// observably: a selection of INBOX sees them leave, QRESYNC reports their
// removal, the moved messages are recent in their new mailbox, INBOX and its
// inferiors remain, and an occupied destination changes nothing.
func TestReview87RenameInbox(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			backend := reviewBackend(t, kind)
			watcher, watches := reviewRawSession(t, backend, nil)
			client, reader := reviewRawSession(t, backend, nil)
			reviewCommand(t, client, reader, "C1", "CREATE INBOX/child", "OK")
			reviewCommand(t, client, reader, "C2", "CREATE taken", "OK")
			for i, raw := range []string{"Subject: one\r\n\r\none", "Subject: two\r\n\r\ntwo", "Subject: child\r\n\r\nchild"} {
				mailbox, flags := "INBOX", `(\Flagged $Kept)`
				if i == 2 {
					mailbox, flags = "INBOX/child", "()"
				}
				reviewCommand(t, client, reader, fmt.Sprintf("A%d", i), fmt.Sprintf("APPEND %s %s {%d+}\r\n%s", mailbox, flags, len(raw), raw), "OK")
			}
			reviewCommand(t, watcher, watches, "E", "ENABLE QRESYNC", "OK")
			selected := reviewCommand(t, watcher, watches, "S", "SELECT INBOX", "OK")
			validity := review87Number(t, selected, "[UIDVALIDITY %d]")
			clock := review87Number(t, selected, "[HIGHESTMODSEQ %d]")
			if !slices.Contains(selected, "* 2 RECENT") {
				t.Fatalf("read-write SELECT did not claim RECENT: %q", selected)
			}

			// An occupied destination: refused, and nothing moves.
			reviewCommand(t, client, reader, "R1", "RENAME INBOX taken", "NO")
			if lines := reviewCommand(t, watcher, watches, "N1", "NOOP", "OK"); len(lines) != 1 {
				t.Fatalf("refused RENAME changed INBOX: %q", lines)
			}
			if status := review87Wire(reviewCommand(t, client, reader, "T1", "STATUS taken (MESSAGES)", "OK")); !strings.Contains(status, "MESSAGES 0") {
				t.Fatalf("refused RENAME filled the destination: %q", status)
			}

			reviewCommand(t, client, reader, "R2", "RENAME INBOX archive", "OK")
			lines := reviewCommand(t, watcher, watches, "N2", "NOOP", "OK")
			if wire := review87Wire(lines); !strings.Contains(wire, "* VANISHED ") || !slices.Contains(lines, "* 0 RECENT") {
				t.Fatalf("INBOX selection after RENAME = %q, want both removals and RECENT 0", wire)
			}
			reviewCommand(t, watcher, watches, "U", "UNSELECT", "OK")
			resync := reviewCommand(t, watcher, watches, "Q", fmt.Sprintf("SELECT INBOX (QRESYNC (%d %d))", validity, clock), "OK")
			if !slices.Contains(resync, "* VANISHED (EARLIER) 1:2") || !slices.Contains(resync, "* 0 EXISTS") {
				t.Fatalf("QRESYNC after RENAME INBOX = %q", resync)
			}
			if after := review87Number(t, resync, "[HIGHESTMODSEQ %d]"); after <= clock {
				t.Fatalf("INBOX HIGHESTMODSEQ %d did not pass %d", after, clock)
			}
			if again := review87Number(t, resync, "[UIDVALIDITY %d]"); again != validity {
				t.Fatalf("INBOX UIDVALIDITY changed from %d to %d", validity, again)
			}

			// The new mailbox: both messages, recent there, flags kept.
			moved := reviewCommand(t, client, reader, "S2", "SELECT archive", "OK")
			if !slices.Contains(moved, "* 2 EXISTS") || !slices.Contains(moved, "* 2 RECENT") {
				t.Fatalf("SELECT archive = %q", moved)
			}
			fetches := review87Fetch(reviewCommand(t, client, reader, "F", "FETCH 1:* (FLAGS BODY.PEEK[TEXT])", "OK"))
			if len(fetches) != 2 || !strings.Contains(fetches[0], `\Flagged`) || !strings.Contains(fetches[0], "$Kept") || !strings.Contains(fetches[1], "two") {
				t.Fatalf("moved messages = %q", fetches)
			}
			// INBOX's inferior is untouched, and INBOX does not reuse UIDs.
			if status := review87Wire(reviewCommand(t, client, reader, "T2", "STATUS INBOX/child (MESSAGES)", "OK")); !strings.Contains(status, "MESSAGES 1") {
				t.Fatalf("INBOX/child after RENAME INBOX = %q", status)
			}
			raw := "Subject: later\r\n\r\nlater"
			appended := review87Wire(reviewCommand(t, client, reader, "A9", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK"))
			if !strings.Contains(appended, fmt.Sprintf("[APPENDUID %d 3]", validity)) {
				t.Fatalf("APPEND to INBOX after RENAME = %q, want UID 3 under the same UIDVALIDITY", appended)
			}
		})
	}
}

// Adjacent to R7: MOVE removes messages from its source at a new modification
// sequence, so QRESYNC reports them (RFC 7162 section 3.2).
func TestReview87MoveRecordsRemovalClock(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			backend := reviewBackend(t, kind)
			review87Seed(t, backend, 2)
			client, reader := reviewRawSession(t, backend, nil)
			reviewCommand(t, client, reader, "C", "CREATE dest", "OK")
			reviewCommand(t, client, reader, "E", "ENABLE QRESYNC", "OK")
			selected := reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
			validity := review87Number(t, selected, "[UIDVALIDITY %d]")
			clock := review87Number(t, selected, "[HIGHESTMODSEQ %d]")
			reviewCommand(t, client, reader, "M", "UID MOVE 1 dest", "OK")
			reviewCommand(t, client, reader, "U", "UNSELECT", "OK")
			resync := reviewCommand(t, client, reader, "Q", fmt.Sprintf("SELECT INBOX (QRESYNC (%d %d 1:2))", validity, clock), "OK")
			if !slices.Contains(resync, "* VANISHED (EARLIER) 1") || review87Number(t, resync, "[HIGHESTMODSEQ %d]") <= clock {
				t.Fatalf("QRESYNC after MOVE = %q (clock was %d)", resync, clock)
			}
		})
	}
}

// Adjacent to R1: Resync reports the backend's current state, which may name a
// message added after the selection's snapshot. That message has no sequence
// number yet and must not fail the SELECT; its addition arrives as EXISTS.
func TestReview87QResyncChangedAfterSnapshot(t *testing.T) {
	backend := &resyncFailureBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
	review87Seed(t, backend, 1)
	client, reader := reviewRawSession(t, backend, nil)
	other, others := reviewRawSession(t, backend, nil)
	reviewCommand(t, client, reader, "E", "ENABLE QRESYNC", "OK")
	raw := "Subject: late\r\n\r\nlate"
	// The append runs inside Resync, after Select took its snapshot.
	backend.inject(func(context.Context) error {
		if _, err := io.WriteString(other, fmt.Sprintf("A APPEND INBOX {%d+}\r\n%s\r\n", len(raw), raw)); err != nil {
			return err
		}
		for {
			line, err := others.ReadString('\n')
			if err != nil || strings.HasPrefix(line, "A OK") {
				return err
			}
			if strings.HasPrefix(line, "A ") {
				return fmt.Errorf("late APPEND failed: %s", line)
			}
		}
	}, nil)
	lines := reviewCommand(t, client, reader, "S", "SELECT INBOX (QRESYNC (1 1))", "OK")
	backend.inject(nil, nil)
	if !slices.Contains(lines, "* 1 EXISTS") || !slices.ContainsFunc(lines, func(line string) bool { return strings.HasPrefix(line, "* 1 FETCH (") }) {
		t.Fatalf("QRESYNC SELECT = %q", lines)
	}
	if !slices.Contains(append(lines, reviewCommand(t, client, reader, "N", "NOOP", "OK")...), "* 2 EXISTS") {
		t.Fatalf("the late addition was not announced: %q", lines)
	}
}
