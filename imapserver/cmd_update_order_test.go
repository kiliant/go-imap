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
}

func (m *expungeOrderMailbox) pushExpunge() error {
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
