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
			},
			NoModSeq: true,
			Revision: "r1",
		},
	}, nil
}

type expungeOrderMailbox struct {
	stubSelectedMailbox
	updater *Updater
	pushed  bool
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

			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if line != "* 1 EXPUNGE\r\n" {
				t.Fatalf("response after completion = %q, want EXPUNGE", line)
			}

			writeUpdateOrderCommand(t, clientSide, "A4 LOGOUT\r\n")
			readUpdateOrderTag(t, reader, "A4 OK ")
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
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
// above, and the reason the rule above was not enough.
//
// Deferring an expunge past a command's tagged completion is correct for a
// client that waits. Dovecot's imaptest showed it is not correct for one that
// pipelines: "after the tagged OK of command n" is simultaneously "while
// command n+1 is in flight", so the expunge left one window RFC 3501 §7.4.1
// forbids and entered the next. The transcript is in docs/INTEROP.md, and
// imaptest's own diagnosis of the consequence was "Referenced message expunged
// seq=4 uid=0" — the loss of sequence-number synchronisation the rule exists to
// prevent.
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

	// Once the pipeline has drained the update is owed, and must arrive.
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "* 1 EXPUNGE\r\n" {
		t.Fatalf("response after the pipeline drained = %q, want EXPUNGE", line)
	}

	writeUpdateOrderCommand(t, clientSide, "A5 LOGOUT\r\n")
	readUpdateOrderTag(t, reader, "A5 OK ")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
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

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
