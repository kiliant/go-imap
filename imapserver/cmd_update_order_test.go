package imapserver

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

type expungeOrderBackend struct{}

func (*expungeOrderBackend) Authenticate(context.Context, *ConnInfo, *Credentials, *AuthenticateOptions) (Session, error) {
	return &expungeOrderSession{}, nil
}

type expungeOrderSession struct {
	stubSession
}

func (s *expungeOrderSession) Select(_ context.Context, _ string, updater *Updater, _ *SelectOptions) (*SelectResult, error) {
	return &SelectResult{
		Mailbox: &expungeOrderMailbox{updater: updater},
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
			go func() { done <- server.ServeConn(ctx, serverSide) }()
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
	go func() { done <- server.ServeConn(ctx, serverSide) }()
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
