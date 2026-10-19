package imapserver

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

type expungeOrderBackend struct {
	// selected receives the mailbox handed to the connection, so a test can
	// publish an update at a moment of its own choosing rather than only from
	// inside a command. The IDLE starvation case needs exactly that.
	selected chan *expungeOrderMailbox
}

func (b *expungeOrderBackend) Authenticate(context.Context, *ConnInfo, *Credentials, *AuthenticateOptions) (Session, error) {
	return &expungeOrderSession{backend: b}, nil
}

type expungeOrderSession struct {
	stubSession
	backend *expungeOrderBackend
}

func (s *expungeOrderSession) Select(_ context.Context, _ string, updater *Updater, _ *SelectOptions) (*SelectResult, error) {
	mailbox := &expungeOrderMailbox{updater: updater}
	if s.backend != nil && s.backend.selected != nil {
		select {
		case s.backend.selected <- mailbox:
		default:
		}
	}
	return &SelectResult{
		Mailbox: mailbox,
		Snapshot: SelectSnapshot{
			UIDs: []imap.UID{1, 2},
			Status: imap.MailboxStatus{
				NumMessages: 2,
				UIDValidity: 1,
				UIDNext:     3,
				NoModSeq:    true,
			},
			Revision: "r1",
		},
	}, nil
}

type expungeOrderMailbox struct {
	stubSelectedMailbox
	updater *Updater
	pushed  bool
}

// deferredAccountingBackend reproduces the ordering imaptest reached: an
// unrelated removal is queued first, then EXPUNGE publishes its own removal
// behind it while another command is already pipelined.
type deferredAccountingBackend struct {
	selected chan *deferredAccountingMailbox
}

func (b *deferredAccountingBackend) Authenticate(context.Context, *ConnInfo, *Credentials, *AuthenticateOptions) (Session, error) {
	return &deferredAccountingSession{backend: b}, nil
}

type deferredAccountingSession struct {
	stubSession
	backend *deferredAccountingBackend
}

func (s *deferredAccountingSession) Select(_ context.Context, _ string, updater *Updater, _ *SelectOptions) (*SelectResult, error) {
	mailbox := &deferredAccountingMailbox{updater: updater}
	s.backend.selected <- mailbox
	return &SelectResult{
		Mailbox: mailbox,
		Snapshot: SelectSnapshot{
			UIDs: []imap.UID{1, 2, 3},
			Status: imap.MailboxStatus{
				NumMessages: 3,
				UIDValidity: 1,
				UIDNext:     4,
				NoModSeq:    true,
			},
			Revision: "r1",
		},
	}, nil
}

type deferredAccountingMailbox struct {
	stubSelectedMailbox
	updater *Updater
}

func (m *deferredAccountingMailbox) pushUnrelatedExpunge() error {
	return m.updater.Push(&UpdateBatch{
		Before:  "r1",
		After:   "r2",
		Changes: []Update{&UpdateExpunge{UID: 1}},
	})
}

func (m *deferredAccountingMailbox) Expunge(ctx context.Context, writer *ExpungeWriter, _ *imap.UIDSet, options *ExpungeOptions) error {
	if err := writer.WriteExpunge(ctx, 3, nil); err != nil {
		return err
	}
	var origin ChangeToken
	if options != nil {
		origin = options.Origin
	}
	return m.updater.Push(&UpdateBatch{
		Before:  "r2",
		After:   "r3",
		Origin:  origin,
		Changes: []Update{&UpdateExpunge{UID: 3}},
	})
}

// pushExpunge publishes the same removal at most once. The revision chain is
// r1→r2, so pushing it twice — which the pipelined test does, since both of its
// commands call this — is a genuine revision mismatch and would fail the
// connection rather than exercise the ordering.
func (m *expungeOrderMailbox) pushExpunge() error {
	if m.pushed {
		return nil
	}
	m.pushed = true
	return m.updater.Push(&UpdateBatch{
		Before:  "r1",
		After:   "r2",
		Changes: []Update{&UpdateExpunge{UID: 1}},
	})
}

func (m *expungeOrderMailbox) Fetch(context.Context, *FetchWriter, imap.UIDSet, *FetchOptions) error {
	return m.pushExpunge()
}

func (m *expungeOrderMailbox) Store(context.Context, *FetchWriter, imap.UIDSet, *StoreFlags, *StoreOptions) error {
	return m.pushExpunge()
}

func (m *expungeOrderMailbox) Search(context.Context, *SearchQuery, *SearchOptions) (*SearchResult, error) {
	if err := m.pushExpunge(); err != nil {
		return nil, err
	}
	return &SearchResult{UIDs: []imap.UID{2}}, nil
}

// TestExpungeUpdateWaitsForCommandCompletion pins RFC 3501 §7.4.1 across the
// tagged response of FETCH, STORE and SEARCH, which is where #69 found it
// broken.
//
// Each command's backend publishes an unrelated removal while the command is
// running, so it cannot be sent then. It must not be sent straight after the
// tagged OK either: from that moment no command is in progress, and a client
// may already have sent its next sequence-numbered command. imaptest does
// exactly that — it sends the next command from the handler of the tagged
// reply, before it reads another line — and then reports the expunge as
// "Referenced message expunged". The follow-up FETCH here is sent the same
// way. The removal is owed to the first command that permits it, and must
// arrive there.
func TestExpungeUpdateWaitsForCommandCompletion(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{name: "FETCH", command: "FETCH 2 (FLAGS)"},
		{name: "STORE", command: "STORE 2 +FLAGS (\\Flagged)"},
		{name: "SEARCH", command: "SEARCH ALL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := New(&expungeOrderBackend{}, &Options{AllowInsecureAuth: true})
			serverSide, clientSide := net.Pipe()
			defer clientSide.Close()
			if err := clientSide.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- server.ServeConn(ctx, serverSide, nil) }()
			reader := bufio.NewReader(clientSide)
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}

			writeUpdateOrderCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
			readUpdateOrderTag(t, reader, "A1 OK ")
			writeUpdateOrderCommand(t, clientSide, "A2 SELECT INBOX\r\n")
			readUpdateOrderTag(t, reader, "A2 OK ")
			writeUpdateOrderCommand(t, clientSide, "A3 "+tt.command+"\r\n")
			readUpdateOrderTag(t, reader, "A3 OK ")
			writeUpdateOrderCommand(t, clientSide, "A4 FETCH 2 (FLAGS)\r\n")
			readUpdateOrderTag(t, reader, "A4 OK ")

			requireNoResponseBetweenCommands(t, clientSide, reader)
			requireExpungeDuringNoop(t, clientSide, reader, "A5")

			writeUpdateOrderCommand(t, clientSide, "A6 LOGOUT\r\n")
			readUpdateOrderTag(t, reader, "A6 OK ")
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// requireNoResponseBetweenCommands asserts that the server stays silent while
// no command is in progress. The wait is bounded, so a regression can only make
// this pass spuriously on a stalled host, never fail spuriously.
func requireNoResponseBetweenCommands(t *testing.T, conn net.Conn, reader *bufio.Reader) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	switch line, err := reader.ReadString('\n'); {
	case err == nil:
		t.Fatalf("server sent %q with no command in progress", strings.TrimSpace(line))
	case !isTimeout(err):
		t.Fatalf("reading between commands: %v", err)
	}
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
}

// requireExpungeDuringNoop sends NOOP and asserts that it carries the withheld
// removal before its tagged response. Holding the update forever would pass
// every assertion about where it must not appear, and would be a worse bug: the
// client would never learn the message was gone.
func requireExpungeDuringNoop(t *testing.T, conn net.Conn, reader *bufio.Reader, tag string) {
	t.Helper()
	writeUpdateOrderCommand(t, conn, tag+" NOOP\r\n")
	var expunges []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, tag+" ") {
			break
		}
		if strings.HasSuffix(line, " EXPUNGE\r\n") {
			expunges = append(expunges, line)
		}
	}
	if len(expunges) != 1 || expunges[0] != "* 1 EXPUNGE\r\n" {
		t.Fatalf("NOOP delivered %q, want the withheld removal exactly once", expunges)
	}
}

func writeUpdateOrderCommand(t *testing.T, conn net.Conn, command string) {
	t.Helper()
	if _, err := io.WriteString(conn, command); err != nil {
		t.Fatal(err)
	}
}

func readUpdateOrderTag(t *testing.T, reader *bufio.Reader, prefix string) {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, prefix) {
			return
		}
		if strings.HasSuffix(line, " EXPUNGE\r\n") {
			t.Fatalf("EXPUNGE preceded %q: %q", prefix, line)
		}
		if !strings.HasPrefix(line, "*") && !strings.HasPrefix(line, "+") {
			t.Fatalf("unexpected response while waiting for %q: %q", prefix, line)
		}
	}
}

// TestExpungeUpdateWaitsForPipelinedCommands is the pipelined half of the rule
// above.
//
// Dovecot's imaptest showed that deferring an expunge until a command's tagged
// completion is not enough for a client that pipelines: "after the tagged OK
// of command n" is simultaneously "while command n+1 is in flight", so the
// expunge left one window RFC 3501 §7.4.1 forbids and entered the next. The
// transcript is in docs/INTEROP.md, and imaptest's own diagnosis of the
// consequence was "Referenced message expunged seq=4 uid=0" — the loss of
// sequence-number synchronisation the rule exists to prevent.
//
// Both FETCHes go out in a single write, so the second is queued before the
// first finishes. Nothing untagged may appear until the last one completes.
func TestExpungeUpdateWaitsForPipelinedCommands(t *testing.T) {
	server := New(&expungeOrderBackend{}, &Options{AllowInsecureAuth: true})
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	if err := clientSide.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide, nil) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	writeUpdateOrderCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
	readUpdateOrderTag(t, reader, "A1 OK ")
	writeUpdateOrderCommand(t, clientSide, "A2 SELECT INBOX\r\n")
	readUpdateOrderTag(t, reader, "A2 OK ")

	// One write, two commands. This is the whole point: A4 is in the server's
	// queue before A3 has been answered, so the sequence numbers A4 carries
	// were computed by the client against the pre-expunge view.
	writeUpdateOrderCommand(t, clientSide, "A3 FETCH 2 (FLAGS)\r\nA4 FETCH 2 (FLAGS)\r\n")

	// Everything up to A4's tagged response must be free of EXPUNGE. Reading
	// line by line rather than waiting for a tag is deliberate: the failure is
	// an untagged response arriving in between, and skipping to the tag would
	// read straight past it.
	sawA3 := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "EXPUNGE") {
			where := "before A3 completed"
			if sawA3 {
				where = "after A3 completed but while A4 was still queued"
			}
			t.Fatalf("EXPUNGE delivered %s: %q", where, line)
		}
		if strings.HasPrefix(line, "A3 OK ") {
			sawA3 = true
			continue
		}
		if strings.HasPrefix(line, "A4 OK ") {
			break
		}
	}

	// A drained pipeline is not yet a moment to deliver it: after A4's tagged
	// response no command is in progress. See
	// TestExpungeUpdateWaitsForCommandCompletion.
	requireNoResponseBetweenCommands(t, clientSide, reader)
	requireExpungeDuringNoop(t, clientSide, reader, "A5")

	writeUpdateOrderCommand(t, clientSide, "A6 LOGOUT\r\n")
	readUpdateOrderTag(t, reader, "A6 OK ")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestDeferredCommandUpdateKeepsItsAccounting pins the second desynchronisation
// imaptest exposed after the pipelined-EXPUNGE fix. If an earlier unsolicited
// removal blocks the batch produced by EXPUNGE, the command has already sent
// that batch's response from its ExpungeWriter. Applying the batch during a
// later command without preserving revision order sends sequence numbers from
// two different mailbox views and leaves every following one too high.
func TestDeferredCommandUpdateKeepsItsAccounting(t *testing.T) {
	backend := &deferredAccountingBackend{selected: make(chan *deferredAccountingMailbox, 1)}
	server := New(backend, &Options{AllowInsecureAuth: true})
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	if err := clientSide.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide, nil) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	writeUpdateOrderCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
	readUpdateOrderTag(t, reader, "A1 OK ")
	writeUpdateOrderCommand(t, clientSide, "A2 SELECT INBOX\r\n")
	readUpdateOrderTag(t, reader, "A2 OK ")
	mailbox := <-backend.selected
	if err := mailbox.pushUnrelatedExpunge(); err != nil {
		t.Fatal(err)
	}

	// A4 keeps the unrelated removal deferred until A3's backend publishes its
	// own batch. The two removals must then be emitted in revision order, rather
	// than mapping A3's UID through the stale pre-removal view first.
	writeUpdateOrderCommand(t, clientSide, "A3 EXPUNGE\r\nA4 NOOP\r\n")
	var expunges []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(line, " EXPUNGE\r\n") {
			expunges = append(expunges, line)
		}
		if strings.HasPrefix(line, "A4 OK ") {
			break
		}
	}
	if len(expunges) != 2 || expunges[0] != "* 1 EXPUNGE\r\n" || expunges[1] != "* 2 EXPUNGE\r\n" {
		t.Fatalf("EXPUNGE responses = %q, want the queued revision prefix exactly once", expunges)
	}

	writeUpdateOrderCommand(t, clientSide, "A5 LOGOUT\r\n")
	readUpdateOrderTag(t, reader, "A5 OK ")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestExpungeAppliesQueuedAddsBeforeBackendCall pins the SERVERBUG imaptest
// stress hit once between-commands delivery stopped clearing the queue. A
// deferred unsolicited removal parks every ADD behind it; EXPUNGE with another
// command pipelined would otherwise ask the backend to remove a UID that is not
// yet in selected.uids and reject it from WriteExpunge.
func TestExpungeAppliesQueuedAddsBeforeBackendCall(t *testing.T) {
	backend := &staleAddExpungeBackend{selected: make(chan *staleAddExpungeMailbox, 1)}
	server := New(backend, &Options{AllowInsecureAuth: true})
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	if err := clientSide.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide, nil) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	writeUpdateOrderCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
	readUpdateOrderTag(t, reader, "A1 OK ")
	writeUpdateOrderCommand(t, clientSide, "A2 SELECT INBOX\r\n")
	readUpdateOrderTag(t, reader, "A2 OK ")
	mailbox := <-backend.selected
	if err := mailbox.pushUnrelatedExpungeThenAdd(); err != nil {
		t.Fatal(err)
	}

	writeUpdateOrderCommand(t, clientSide, "A3 EXPUNGE\r\nA4 NOOP\r\n")
	var sawA3OK bool
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "A3 NO ") {
			t.Fatalf("EXPUNGE failed against a UID only present behind a deferred removal: %q", line)
		}
		if strings.HasPrefix(line, "A3 OK ") {
			sawA3OK = true
		}
		if strings.HasPrefix(line, "A4 OK ") {
			break
		}
	}
	if !sawA3OK {
		t.Fatal("EXPUNGE never completed")
	}

	writeUpdateOrderCommand(t, clientSide, "A5 LOGOUT\r\n")
	readUpdateOrderTag(t, reader, "A5 OK ")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type staleAddExpungeBackend struct {
	selected chan *staleAddExpungeMailbox
}

func (b *staleAddExpungeBackend) Authenticate(context.Context, *ConnInfo, *Credentials, *AuthenticateOptions) (Session, error) {
	return &staleAddExpungeSession{backend: b}, nil
}

type staleAddExpungeSession struct {
	stubSession
	backend *staleAddExpungeBackend
}

func (s *staleAddExpungeSession) Select(_ context.Context, _ string, updater *Updater, _ *SelectOptions) (*SelectResult, error) {
	mailbox := &staleAddExpungeMailbox{updater: updater}
	s.backend.selected <- mailbox
	return &SelectResult{
		Mailbox: mailbox,
		Snapshot: SelectSnapshot{
			UIDs: []imap.UID{1, 2, 3},
			Status: imap.MailboxStatus{
				NumMessages: 3,
				UIDValidity: 1,
				UIDNext:     4,
				NoModSeq:    true,
			},
			Revision: "r1",
		},
	}, nil
}

type staleAddExpungeMailbox struct {
	stubSelectedMailbox
	updater *Updater
}

func (m *staleAddExpungeMailbox) pushUnrelatedExpungeThenAdd() error {
	if err := m.updater.Push(&UpdateBatch{
		Before:  "r1",
		After:   "r2",
		Changes: []Update{&UpdateExpunge{UID: 1}},
	}); err != nil {
		return err
	}
	return m.updater.Push(&UpdateBatch{
		Before:  "r2",
		After:   "r3",
		Changes: []Update{&UpdateAdd{UIDs: []imap.UID{4}}},
	})
}

func (m *staleAddExpungeMailbox) Expunge(ctx context.Context, writer *ExpungeWriter, _ *imap.UIDSet, options *ExpungeOptions) error {
	// UID 4 exists only via the ADD parked behind the deferred removal. Without
	// draining that prefix first, the old WriteExpunge check rejected it.
	if err := writer.WriteExpunge(ctx, 4, nil); err != nil {
		return err
	}
	var origin ChangeToken
	if options != nil {
		origin = options.Origin
	}
	return m.updater.Push(&UpdateBatch{
		Before:  "r3",
		After:   "r4",
		Origin:  origin,
		Changes: []Update{&UpdateExpunge{UID: 4}},
	})
}

// TestIdleReceivesUpdatesAfterPartialInput is the liveness half of the expunge
// deferral, and it covers a starvation the first version of that deferral had.
//
// inputPending is refreshed only when the reader parses another command line.
// The reader parks for the whole of a barrier command, and IDLE is a barrier —
// so an IDLE whose line arrived with trailing bytes that are not yet a complete
// line froze the flag at true for the entire IDLE, and every unsolicited update
// was withheld from the one client that is idling precisely to receive them.
// Nothing retries, and the client will not complete the line because it is
// waiting: it ends at teardown, with the queue meanwhile filling toward
// overflow.
//
// The trailing "DON" is the reproduction: a real client sending DONE in two
// writes, or any client whose framing lands mid-token.
func TestIdleReceivesUpdatesAfterPartialInput(t *testing.T) {
	backend := &expungeOrderBackend{selected: make(chan *expungeOrderMailbox, 1)}
	server := New(backend, &Options{AllowInsecureAuth: true})
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	if err := clientSide.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide, nil) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	writeUpdateOrderCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
	readUpdateOrderTag(t, reader, "A1 OK ")
	writeUpdateOrderCommand(t, clientSide, "A2 SELECT INBOX\r\n")
	readUpdateOrderTag(t, reader, "A2 OK ")

	var mailbox *expungeOrderMailbox
	select {
	case mailbox = <-backend.selected:
	case <-time.After(5 * time.Second):
		t.Fatal("the backend never handed out a selected mailbox")
	}

	// IDLE arrives with a trailing fragment: the client has begun DONE but the
	// line is not complete. This is what froze inputPending, because the reader
	// parks for the whole of a barrier command and never refreshes it.
	writeUpdateOrderCommand(t, clientSide, "A3 IDLE\r\nDON")
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "+ ") {
		t.Fatalf("IDLE continuation = %q", line)
	}

	// Publish while the client is idling. This is the whole point: an idling
	// client is idling in order to be told.
	if err := mailbox.pushExpunge(); err != nil {
		t.Fatal(err)
	}
	line, err = reader.ReadString('\n')
	if err != nil {
		t.Fatalf("no unsolicited update reached the idling client: %v", err)
	}
	if !strings.Contains(line, "EXPUNGE") {
		t.Fatalf("response during IDLE = %q, want EXPUNGE", line)
	}

	writeUpdateOrderCommand(t, clientSide, "E\r\n")
	readUpdateOrderTag(t, reader, "A3 OK ")
	writeUpdateOrderCommand(t, clientSide, "A4 LOGOUT\r\n")
	readUpdateOrderTag(t, reader, "A4 OK ")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestExpungeUpdateWaitsForACommandToBeInProgress is the clause the pipelined
// fix left open, and the one Dovecot's imaptest kept reporting after it landed.
//
// RFC 3501 §7.4.1 opens with "An EXPUNGE response MUST NOT be sent when no
// command is in progress", and the first version of this rule modelled only the
// half that follows it — not while responding to FETCH, STORE or SEARCH. So
// between commands the event loop delivered on its own update signal, and the
// three deferral conditions were all false because there was genuinely nothing
// to see: no queued command, and no buffered input, because the client's next
// command was still on the wire. inputPending cannot observe that, which is why
// "is a command in progress" has to be asked instead of inferred.
//
// Both halves are asserted. Holding the update forever would also pass an
// assertion that only checked the gap, and would be a worse bug than the one
// being fixed — the client would never learn the message was gone.
func TestExpungeUpdateWaitsForACommandToBeInProgress(t *testing.T) {
	backend := &expungeOrderBackend{selected: make(chan *expungeOrderMailbox, 1)}
	server := New(backend, &Options{AllowInsecureAuth: true})
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide, nil) }()
	reader := bufio.NewReader(clientSide)
	if err := clientSide.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	writeUpdateOrderCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
	readUpdateOrderTag(t, reader, "A1 OK ")
	writeUpdateOrderCommand(t, clientSide, "A2 SELECT INBOX\r\n")
	readUpdateOrderTag(t, reader, "A2 OK ")

	// Published from the test rather than from inside a command handler: that
	// is what makes this the unsolicited case, another session's expunge
	// arriving while this connection sits between commands.
	mailbox := <-backend.selected
	if err := mailbox.pushExpunge(); err != nil {
		t.Fatal(err)
	}

	if err := clientSide.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	switch line, err := reader.ReadString('\n'); {
	case err == nil:
		t.Fatalf("server sent %q with no command in progress", strings.TrimSpace(line))
	case !isTimeout(err):
		t.Fatalf("reading while idle: %v", err)
	}
	if err := clientSide.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}

	// NOOP carries no sequence numbers, so it is the first legal moment: the
	// client sees the EXPUNGE before the tagged response, and everything it
	// sends afterwards is numbered against the new view.
	writeUpdateOrderCommand(t, clientSide, "A3 NOOP\r\n")
	sawExpunge := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "EXPUNGE") {
			sawExpunge = true
			continue
		}
		if strings.HasPrefix(line, "A3 ") {
			break
		}
	}
	if !sawExpunge {
		t.Fatal("NOOP completed without the expunge the server had been holding; " +
			"deferring it forever loses the update instead of ordering it")
	}

	writeUpdateOrderCommand(t, clientSide, "A4 LOGOUT\r\n")
	readUpdateOrderTag(t, reader, "A4 OK ")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// startUpdateOrderSession serves one connection on a pipe, logs in and selects
// INBOX, and returns the client side with the mailbox the server selected.
func startUpdateOrderSession(t *testing.T) (net.Conn, *bufio.Reader, *expungeOrderMailbox, <-chan error) {
	t.Helper()
	backend := &expungeOrderBackend{selected: make(chan *expungeOrderMailbox, 1)}
	server := New(backend, &Options{AllowInsecureAuth: true})
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() { clientSide.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide, nil) }()
	if err := clientSide.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	writeUpdateOrderCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
	readUpdateOrderTag(t, reader, "A1 OK ")
	writeUpdateOrderCommand(t, clientSide, "A2 SELECT INBOX\r\n")
	readUpdateOrderTag(t, reader, "A2 OK ")
	return clientSide, reader, <-backend.selected, done
}

// TestExpungeUpdateDeliveredDuringUIDCommands pins the other side of #69's fix.
// RFC 9051 §7.5.1: "An EXPUNGE response MAY be sent during a UID command." The
// UID forms defer in their pre-command drain and, once the command is complete,
// in the drain after their tagged response too. Without a delivery point
// between the two, a client that only ever sends UID commands would never be
// told of the removal, nor of anything queued behind it, until its update queue
// overflowed.
func TestExpungeUpdateDeliveredDuringUIDCommands(t *testing.T) {
	for _, command := range []string{
		"UID FETCH 2 (FLAGS)",
		"UID STORE 2 +FLAGS (\\Flagged)",
		"UID SEARCH ALL",
	} {
		t.Run(command, func(t *testing.T) {
			conn, reader, mailbox, done := startUpdateOrderSession(t)
			if err := mailbox.pushExpunge(); err != nil {
				t.Fatal(err)
			}
			requireNoResponseBetweenCommands(t, conn, reader)

			writeUpdateOrderCommand(t, conn, "A3 "+command+"\r\n")
			var expunges []string
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(line, "A3 ") {
					break
				}
				if strings.HasSuffix(line, " EXPUNGE\r\n") {
					expunges = append(expunges, line)
				}
			}
			if len(expunges) != 1 || expunges[0] != "* 1 EXPUNGE\r\n" {
				t.Fatalf("%s delivered %q before its tagged response, want the withheld removal exactly once", command, expunges)
			}
			requireNoResponseBetweenCommands(t, conn, reader)

			writeUpdateOrderCommand(t, conn, "A4 LOGOUT\r\n")
			readUpdateOrderTag(t, reader, "A4 OK ")
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestIdleDeliversRemovalWithheldAcrossFetch pins the IDLE half of the review
// of #69's fix. A removal withheld across a FETCH is the common case now, and
// IDLE's pre-command drain may still defer it while the reader settles. The
// update signal has already been consumed, so IDLE must drain once it knows the
// reader is parked rather than wait for another update to arrive.
func TestIdleDeliversRemovalWithheldAcrossFetch(t *testing.T) {
	conn, reader, _, done := startUpdateOrderSession(t)
	writeUpdateOrderCommand(t, conn, "A3 FETCH 2 (FLAGS)\r\n")
	readUpdateOrderTag(t, reader, "A3 OK ")
	requireNoResponseBetweenCommands(t, conn, reader)

	writeUpdateOrderCommand(t, conn, "A4 IDLE\r\n")
	idling, expunged := false, false
	for !idling || !expunged {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("waiting for the withheld removal during IDLE: %v", err)
		}
		switch {
		case strings.HasPrefix(line, "+ "):
			idling = true
		case line == "* 1 EXPUNGE\r\n":
			expunged = true
		default:
			t.Fatalf("unexpected response during IDLE: %q", line)
		}
	}
	writeUpdateOrderCommand(t, conn, "DONE\r\n")
	readUpdateOrderTag(t, reader, "A4 OK ")

	writeUpdateOrderCommand(t, conn, "A5 LOGOUT\r\n")
	readUpdateOrderTag(t, reader, "A5 OK ")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
