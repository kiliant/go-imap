package imapserver

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

type noModSeqBackend struct{ calls atomic.Int32 }

func (b *noModSeqBackend) Authenticate(context.Context, *ConnInfo, *Credentials, *AuthenticateOptions) (Session, error) {
	return &noModSeqSession{backend: b}, nil
}

func (*noModSeqBackend) SupportsCapability(_ context.Context, name string, _ *CapabilitySupportOptions) bool {
	return name == "CONDSTORE" || name == "SORT" || name == "THREAD=ORDEREDSUBJECT"
}

type noModSeqSession struct {
	stubSession
	backend *noModSeqBackend
}

func (s *noModSeqSession) Select(_ context.Context, name string, _ *Updater, _ *SelectOptions) (*SelectResult, error) {
	snapshot := SelectSnapshot{
		UIDs: []imap.UID{7}, Revision: "r1",
		Status: imap.MailboxStatus{UIDValidity: 1, UIDNext: 8, NumMessages: 1},
	}
	if name == "tracked" {
		snapshot.Status.HighestModSeq = 9
	} else {
		snapshot.Status.NoModSeq = true
		if name == "stale-modseq" {
			snapshot.Status.HighestModSeq = 9
		}
	}
	return &SelectResult{Mailbox: &noModSeqMailbox{backend: s.backend}, Snapshot: snapshot}, nil
}

type noModSeqMailbox struct {
	stubSelectedMailbox
	backend *noModSeqBackend
}

func (m *noModSeqMailbox) Fetch(context.Context, *FetchWriter, imap.UIDSet, *FetchOptions) error {
	m.backend.calls.Add(1)
	return nil
}
func (m *noModSeqMailbox) Search(context.Context, *SearchQuery, *SearchOptions) (*SearchResult, error) {
	m.backend.calls.Add(1)
	return &SearchResult{}, nil
}
func (m *noModSeqMailbox) StoreCondStore(context.Context, *FetchWriter, imap.UIDSet, *StoreFlags, *StoreOptions) (*CondStoreResult, error) {
	m.backend.calls.Add(1)
	return &CondStoreResult{}, nil
}

func (m *noModSeqMailbox) Sort(context.Context, *SearchQuery, []imap.SortKeySpec, *SortOptions) ([]imap.UID, error) {
	m.backend.calls.Add(1)
	return nil, nil
}

func (m *noModSeqMailbox) Thread(context.Context, *SearchQuery, imap.ThreadAlgorithm, *ThreadOptions) ([]imap.ThreadNode, error) {
	m.backend.calls.Add(1)
	return nil, nil
}

func TestNoModSeqRejectsCommandsBeforeBackend(t *testing.T) {
	for _, mailbox := range []string{"status", "stale-modseq"} {
		t.Run(mailbox, func(t *testing.T) {
			backend := &noModSeqBackend{}
			server := New(backend, &Options{AllowInsecureAuth: true})
			serverSide, clientSide := net.Pipe()
			defer clientSide.Close()
			_ = clientSide.SetDeadline(time.Now().Add(15 * time.Second))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { _ = server.ServeConn(ctx, serverSide, nil) }()
			reader := bufio.NewReader(clientSide)
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			request := func(tag, command, want string) []string {
				t.Helper()
				writeUpdateOrderCommand(t, clientSide, tag+" "+command+"\r\n")
				var lines []string
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						t.Fatal(err)
					}
					if strings.HasPrefix(line, tag+" ") {
						if !strings.HasPrefix(line, tag+" "+want+" ") {
							t.Errorf("%s: %q, want %s", command, line, want)
						}
						return lines
					}
					lines = append(lines, line)
				}
			}
			request("L", "LOGIN alice secret", "OK")
			lines := request("S", "SELECT "+mailbox+" (CONDSTORE)", "OK")
			if !strings.Contains(strings.Join(lines, ""), "NOMODSEQ") {
				t.Fatalf("missing NOMODSEQ: %q", lines)
			}
			for i, command := range []string{
				"FETCH 1 (MODSEQ)", "UID FETCH 7 (MODSEQ)",
				"FETCH 1 (FLAGS) (CHANGEDSINCE 1)", "UID FETCH 7 (FLAGS) (CHANGEDSINCE 1)",
				"SEARCH MODSEQ 1", "UID SEARCH NOT NOT MODSEQ 1",
				"STORE 1 (UNCHANGEDSINCE 0) +FLAGS (\\Seen)", "UID STORE 7 (UNCHANGEDSINCE 1) +FLAGS (\\Seen)",
				"SORT (ARRIVAL) US-ASCII MODSEQ 1", "UID SORT (ARRIVAL) US-ASCII MODSEQ 1",
				"THREAD ORDEREDSUBJECT US-ASCII MODSEQ 1", "UID THREAD ORDEREDSUBJECT US-ASCII MODSEQ 1",
			} {
				request(fmt.Sprintf("Q%d", i), command, "BAD")
			}
			if calls := backend.calls.Load(); calls != 0 {
				t.Errorf("rejected commands reached backend %d times", calls)
			}
			request("F", "FETCH 1 (FLAGS)", "OK")
			request("T", "SELECT tracked (CONDSTORE)", "OK")
			request("M", "FETCH 1 (MODSEQ)", "OK")
			if calls := backend.calls.Load(); calls != 2 {
				t.Errorf("base FETCH and tracked FETCH reached backend %d times, want 2", calls)
			}
		})
	}
}
