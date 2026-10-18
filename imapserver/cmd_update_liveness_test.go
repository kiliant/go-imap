package imapserver

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

type updateLivenessBackend struct{ add bool }

func (b *updateLivenessBackend) SupportsCapability(_ context.Context, name string, _ *CapabilitySupportOptions) bool {
	return name == "CONDSTORE" || name == "QRESYNC"
}

func (b *updateLivenessBackend) Authenticate(context.Context, *ConnInfo, *Credentials, *AuthenticateOptions) (Session, error) {
	return &updateLivenessSession{backend: b}, nil
}

type updateLivenessSession struct {
	stubSession
	backend *updateLivenessBackend
}

func (s *updateLivenessSession) Select(_ context.Context, _ string, updater *Updater, _ *SelectOptions) (*SelectResult, error) {
	return &SelectResult{
		Mailbox: &updateLivenessMailbox{updater: updater, add: s.backend.add, revision: 1},
		Snapshot: SelectSnapshot{
			UIDs: []imap.UID{1, 2, 3}, Revision: "r1", HighestModSeq: 10,
			Status: imap.MailboxStatus{NumMessages: 3, UIDValidity: 1, UIDNext: 4, HighestModSeq: 10},
		},
	}, nil
}

type updateLivenessMailbox struct {
	stubSelectedMailbox
	updater  *Updater
	add      bool
	revision int
}

func (m *updateLivenessMailbox) push(origin ChangeToken, changes ...Update) error {
	before := fmt.Sprintf("r%d", m.revision)
	m.revision++
	return m.updater.Push(&UpdateBatch{Before: MailboxRevision(before), After: MailboxRevision(fmt.Sprintf("r%d", m.revision)), Origin: origin, Changes: changes})
}

func (m *updateLivenessMailbox) Fetch(ctx context.Context, writer *FetchWriter, uids imap.UIDSet, _ *FetchOptions) error {
	if m.revision == 1 {
		if err := m.push(0, &UpdateExpunge{UID: 1}); err != nil {
			return err
		}
		if m.add {
			if err := m.push(0, &UpdateAdd{UIDs: []imap.UID{4}}, &UpdateFlags{UID: 3, Flags: []imap.Flag{imap.FlagFlagged}, ModSeq: 20}); err != nil {
				return err
			}
		}
	}
	for _, uid := range []imap.UID{2, 3} {
		if uids.Contains(uid) {
			if err := writer.WriteMessage(ctx, &imap.FetchMessageData{Items: map[imap.FetchDataKey][]imap.FetchData{
				"UID": {imap.FetchDataUID(uid)}, "FLAGS": {imap.FetchDataFlags{}}, "MODSEQ": {imap.FetchDataModSeq(20 + uint64(m.revision))},
			}}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *updateLivenessMailbox) Store(_ context.Context, _ *FetchWriter, _ imap.UIDSet, _ *StoreFlags, options *StoreOptions) error {
	return m.push(options.Origin, &UpdateFlags{UID: 2, Flags: []imap.Flag{imap.FlagSeen}, ModSeq: 20 + uint64(m.revision)})
}

func (m *updateLivenessMailbox) Search(context.Context, *SearchQuery, *SearchOptions) (*SearchResult, error) {
	uids := []imap.UID{2, 3}
	if m.add {
		uids = append(uids, 4)
	}
	return &SearchResult{UIDs: uids}, nil
}

func readLivenessTag(t *testing.T, reader *bufio.Reader, tag string, allowRemoval bool) string {
	t.Helper()
	var wire strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		wire.WriteString(line)
		if !allowRemoval && (strings.Contains(line, " EXPUNGE") || strings.Contains(line, " VANISHED")) {
			t.Fatalf("premature removal: %s", wire.String())
		}
		if strings.HasPrefix(line, tag+" ") {
			if !strings.HasPrefix(line, tag+" OK") {
				t.Fatalf("command failed: %s", wire.String())
			}
			return wire.String()
		}
	}
}

func TestUpdatesPassWithheldRemoval(t *testing.T) {
	for _, qresync := range []bool{false, true} {
		t.Run(fmt.Sprint(qresync), func(t *testing.T) {
			connection, reader := openLivenessSession(t, true, qresync)
			writeUpdateOrderCommand(t, connection, "F FETCH 2 (FLAGS)\r\n")
			wire := readLivenessTag(t, reader, "F", false)
			if !strings.Contains(wire, "* 4 EXISTS\r\n") || !strings.Contains(wire, "* 3 FETCH (FLAGS (\\Flagged)") {
				t.Fatalf("updates stalled behind removal: %s", wire)
			}
			if qresync && (!strings.Contains(wire, "MODSEQ (20)") || !strings.Contains(wire, "[HIGHESTMODSEQ 10]")) {
				t.Fatalf("unsafe resync checkpoint: %s", wire)
			}
			writeUpdateOrderCommand(t, connection, "S SEARCH ALL\r\n")
			if wire := readLivenessTag(t, reader, "S", false); !strings.Contains(wire, "* SEARCH 2 3 4\r\n") {
				t.Fatalf("client sequence view changed: %s", wire)
			}
			writeUpdateOrderCommand(t, connection, "N NOOP\r\n")
			wire = readLivenessTag(t, reader, "N", true)
			want := "* 1 EXPUNGE\r\n"
			if qresync {
				want = "* VANISHED 1\r\n"
			}
			if strings.Count(wire, want) != 1 || strings.Contains(wire, "removal announcements pending") {
				t.Fatalf("NOOP removal delivery: %s", wire)
			}
			writeUpdateOrderCommand(t, connection, "A SEARCH ALL\r\n")
			if wire := readLivenessTag(t, reader, "A", false); !strings.Contains(wire, "* SEARCH 1 2 3\r\n") {
				t.Fatalf("view did not catch up: %s", wire)
			}
		})
	}
}

func TestOwnStoreBatchesDoNotAccumulateBehindRemoval(t *testing.T) {
	connection, reader := openLivenessSession(t, false, false)
	writeUpdateOrderCommand(t, connection, "F FETCH 2 (FLAGS)\r\n")
	readLivenessTag(t, reader, "F", false)
	for i := 0; i < 300; i++ {
		tag := fmt.Sprintf("S%d", i)
		writeUpdateOrderCommand(t, connection, tag+" STORE 2 +FLAGS.SILENT (\\Seen)\r\n")
		wire := readLivenessTag(t, reader, tag, false)
		if strings.Contains(wire, " FETCH ") {
			t.Fatalf("own STORE update was duplicated: %s", wire)
		}
	}
	writeUpdateOrderCommand(t, connection, "N NOOP\r\n")
	if wire := readLivenessTag(t, reader, "N", true); strings.Count(wire, "* 1 EXPUNGE\r\n") != 1 || strings.Contains(wire, " FETCH ") {
		t.Fatalf("stale own updates replayed: %s", wire)
	}
}

func TestPipelinedCommandsKeepViewWhileUpdatesAdvance(t *testing.T) {
	connection, reader := openLivenessSession(t, true, false)
	writeUpdateOrderCommand(t, connection, "F FETCH 2 (FLAGS)\r\nG FETCH 3 (FLAGS)\r\nS SEARCH ALL\r\n")
	readLivenessTag(t, reader, "F", false)
	if wire := readLivenessTag(t, reader, "G", false); !strings.Contains(wire, "* 3 FETCH ") {
		t.Fatalf("pipelined FETCH was renumbered: %s", wire)
	}
	if wire := readLivenessTag(t, reader, "S", false); !strings.Contains(wire, "* SEARCH 2 3 4\r\n") {
		t.Fatalf("pipelined SEARCH lost additions or changed numbering: %s", wire)
	}
	writeUpdateOrderCommand(t, connection, "N NOOP\r\n")
	if wire := readLivenessTag(t, reader, "N", true); strings.Count(wire, "* 1 EXPUNGE\r\n") != 1 {
		t.Fatalf("withheld removal lost: %s", wire)
	}
}

func openLivenessSession(t *testing.T, add, qresync bool) (net.Conn, *bufio.Reader) {
	t.Helper()
	server := New(&updateLivenessBackend{add: add}, &Options{AllowInsecureAuth: true, Limits: Limits{MaxQueuedUpdates: 4}})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	client, peer := net.Pipe()
	t.Cleanup(func() { client.Close(); cancel(); server.Close(context.Background(), nil) })
	if err := client.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.ServeConn(ctx, peer, nil) }()
	reader := bufio.NewReader(client)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	writeUpdateOrderCommand(t, client, "L LOGIN alice secret\r\n")
	readLivenessTag(t, reader, "L", false)
	if qresync {
		writeUpdateOrderCommand(t, client, "E ENABLE QRESYNC\r\n")
		readLivenessTag(t, reader, "E", false)
	}
	writeUpdateOrderCommand(t, client, "S SELECT INBOX\r\n")
	readLivenessTag(t, reader, "S", false)
	return client, reader
}
