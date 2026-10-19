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
	"github.com/kiliant/go-imap/internal/imapwire"
)

var invalidQResyncSets = []string{
	"*", "1:*", "*:1", "1,3:*",
	"(* 1)", "(1 *)", "(1:* 1:2)", "(1:2 1:*)",
	"(2:1 1:2)", "(1:2 2:1)", "(2,1 1:2)", "(1:2 2,1)",
	"(1,1 1:2)", "(1:2 1,1)", "(1:2,2:3 1:4)", "(1:4 1:2,2:3)",
	"(1:2 1)", "(1 1:2)",
	"(1:4294967295 1:4294967294)",
	"(1:4294967295,1:4294967295 1:4294967294)",
}

func TestQResyncRejectsInvalidSets(t *testing.T) {
	for _, sets := range invalidQResyncSets {
		t.Run(sets, func(t *testing.T) {
			dec := imapwire.NewDecoderString("(1 1 "+sets+")\r\n", nil)
			if params, err := parseQResyncParams(dec); err == nil {
				t.Fatalf("accepted invalid sets: %+v", params)
			}
		})
	}
}

func TestQResyncAcceptsStaticSets(t *testing.T) {
	for _, sets := range []string{
		"", "1:3", "3:1,7,5", "(1 3)", "1:9 (1:2,4 3,5:6)",
		"(1:4294967295 1:4294967295)",
		"(1:4294967294,4294967295 1,2:4294967295)",
		"(4294967295 4294967295)",
	} {
		t.Run(sets, func(t *testing.T) {
			input := "(1 1"
			if sets != "" {
				input += " " + sets
			}
			dec := imapwire.NewDecoderString(input+")\r\n", nil)
			if _, err := parseQResyncParams(dec); err != nil {
				t.Fatal(err)
			}
			if !dec.ExpectCRLF() {
				t.Fatal(dec.Err())
			}
		})
	}
}

type qResyncSetBackend struct {
	selects atomic.Int32
	resyncs atomic.Int32
}

func (b *qResyncSetBackend) Authenticate(context.Context, *ConnInfo, *Credentials, *AuthenticateOptions) (Session, error) {
	return &qResyncSetSession{backend: b}, nil
}

func (*qResyncSetBackend) SupportsCapability(_ context.Context, name string, _ *CapabilitySupportOptions) bool {
	return name == "CONDSTORE" || name == "QRESYNC"
}

type qResyncSetSession struct {
	stubSession
	backend *qResyncSetBackend
}

func (s *qResyncSetSession) Select(context.Context, string, *Updater, *SelectOptions) (*SelectResult, error) {
	s.backend.selects.Add(1)
	return &SelectResult{
		Mailbox: &qResyncSetMailbox{backend: s.backend},
		Snapshot: SelectSnapshot{
			UIDs: []imap.UID{1}, Revision: "r1",
			Status: imap.MailboxStatus{NumMessages: 1, UIDValidity: 1, UIDNext: 2, HighestModSeq: 1},
		},
	}, nil
}

type qResyncSetMailbox struct {
	stubSelectedMailbox
	backend *qResyncSetBackend
}

func (m *qResyncSetMailbox) Resync(context.Context, *QResyncSelect, *QResyncOptions) (*QResyncResult, error) {
	m.backend.resyncs.Add(1)
	return &QResyncResult{}, nil
}

func TestQResyncRejectsInvalidSetsBeforeBackend(t *testing.T) {
	backend := &qResyncSetBackend{}
	server := New(backend, &Options{AllowInsecureAuth: true})
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	if err := clientSide.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() { _ = server.ServeConn(ctx, serverSide, nil) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	request := func(tag, command, status string) {
		t.Helper()
		writeUpdateOrderCommand(t, clientSide, tag+" "+command+"\r\n")
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, tag+" ") {
				if !strings.HasPrefix(line, tag+" "+status+" ") {
					t.Fatalf("%s: %q, want %s", command, line, status)
				}
				return
			}
		}
	}
	request("L", "LOGIN alice secret", "OK")
	request("E", "ENABLE QRESYNC", "OK")
	request("S", "SELECT INBOX", "OK")
	for _, command := range []string{"SELECT", "EXAMINE"} {
		for _, validity := range []int{1, 99} {
			for i, sets := range invalidQResyncSets {
				request(fmt.Sprintf("Q%d", i), fmt.Sprintf("%s INBOX (QRESYNC (%d 1 %s))", command, validity, sets), "BAD")
			}
		}
	}
	if got := backend.selects.Load(); got != 1 {
		t.Fatalf("invalid requests reached Select: calls = %d, want 1", got)
	}
	if got := backend.resyncs.Load(); got != 0 {
		t.Fatalf("invalid requests reached Resync: calls = %d", got)
	}
	// Invalid syntax must also leave the existing selection usable.
	request("F", "FETCH 1 (FLAGS)", "OK")
	request("V", "SELECT INBOX (QRESYNC (1 1 1 (1 1)))", "OK")
	request("M", "SELECT INBOX (QRESYNC (99 1 1 (1 1)))", "OK")
	if selects, resyncs := backend.selects.Load(), backend.resyncs.Load(); selects != 3 || resyncs != 1 {
		t.Fatalf("valid requests: Select calls = %d, Resync calls = %d; want 3, 1", selects, resyncs)
	}
}
