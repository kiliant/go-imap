package imapserver_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func TestLoopbackBaseCommandSet(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	backend := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})
	exists := make(chan uint32, 16)
	client, done := openLoopbackClient(t, ctx, server, &imapclient.Options{
		AllowInsecureAuth: true,
		UnilateralData: &imapclient.UnilateralDataHandler{
			Exists: func(n uint32) { exists <- n },
		},
	})
	if err := client.Login(ctx, "alice", "secret", nil); err != nil {
		t.Fatal(err)
	}
	for _, mailbox := range []string{"Archive", "Moved"} {
		if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
			t.Fatalf("CREATE %s: %v", mailbox, err)
		}
	}
	if err := client.Subscribe("Archive", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Create("Temporary", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Rename("Temporary", "Renamed", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Subscribe("Renamed", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Unsubscribe("Renamed", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Delete("Renamed", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	listed, err := client.List("", "*", nil).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !containsMailbox(listed, "INBOX") || !containsMailbox(listed, "Archive") {
		t.Fatalf("LIST = %#v", listed)
	}
	subscribed, err := client.Lsub("", "*", nil).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(subscribed) != 1 || subscribed[0].Mailbox != "Archive" {
		t.Fatalf("LSUB = %#v", subscribed)
	}
	status, err := client.Status("INBOX", nil).Wait(ctx)
	if err != nil || status.NumMessages != 0 || status.UIDValidity == 0 || status.UIDNext == 0 {
		t.Fatalf("STATUS = %#v, %v", status, err)
	}

	raw := []byte("From: sender@example.com\r\nTo: alice@example.com\r\nSubject: loopback\r\n\r\nhello\r\n")
	appended, err := client.Append(ctx, "INBOX", nil, int64(len(raw)), bytes.NewReader(raw)).Wait(ctx)
	if err != nil || appended == nil || !appended.HasUID {
		t.Fatalf("APPEND = %#v, %v", appended, err)
	}
	selected, err := client.Select("INBOX", nil).Wait(ctx)
	if err != nil || selected.NumMessages != 1 || selected.UIDValidity == 0 || !selected.NoModSeq {
		t.Fatalf("SELECT = %#v, %v", selected, err)
	}

	fetch := client.FetchUID(imap.UIDSetNum(appended.UID), nil,
		imap.FetchItemUID, imap.FetchItemFlags, &imap.FetchItemBodySection{Peek: true})
	var fetchedBody []byte
	for {
		message, err := fetch.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if message.SeqNum != 1 {
			t.Fatalf("FETCH sequence number = %d", message.SeqNum)
		}
		values := message.Items[imap.FetchDataKey("BODY[]")]
		if len(values) != 1 {
			t.Fatalf("FETCH BODY[] values = %#v", values)
		}
		section, ok := values[0].(*imap.FetchDataBodySection)
		if !ok {
			t.Fatalf("FETCH BODY[] value = %T", values[0])
		}
		fetchedBody, err = io.ReadAll(section.Literal)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(fetchedBody, raw) {
		t.Fatalf("FETCH BODY[] = %q", fetchedBody)
	}
	seqMatches, err := client.Search(imap.SearchAll, nil).All(ctx)
	if err != nil || !slices.Equal(seqMatches, []imap.SeqNum{1}) {
		t.Fatalf("SEARCH = %v, %v", seqMatches, err)
	}
	charsetMatches, err := client.Search(imap.SearchString{Key: imap.SearchKeySubject, Value: "loopback"}, &imapclient.SearchOptions{Charset: "UTF-8"}).All(ctx)
	if err != nil || !slices.Equal(charsetMatches, []imap.SeqNum{1}) {
		t.Fatalf("SEARCH CHARSET = %v, %v", charsetMatches, err)
	}
	uidMatches, err := client.SearchUID(imap.SearchSeqNum{Set: imap.SeqSetNum(1)}, nil).AllUID(ctx)
	if err != nil || !slices.Equal(uidMatches, []imap.UID{appended.UID}) {
		t.Fatalf("UID SEARCH = %v, %v", uidMatches, err)
	}
	if err := client.StoreUID(imap.UIDSetNum(appended.UID), []imap.Flag{imap.FlagFlagged}, &imapclient.StoreOptions{Op: imapclient.StoreFlagsAdd}).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	copied, err := client.CopyUID(imap.UIDSetNum(appended.UID), "Archive", nil).Wait(ctx)
	if err != nil || copied == nil || !copied.HasUIDs {
		t.Fatalf("UID COPY = %#v, %v", copied, err)
	}
	moved, err := client.MoveUID(ctx, imap.UIDSetNum(appended.UID), "Moved", nil)
	if err != nil || moved == nil || !moved.UIDPlus.HasUIDs {
		t.Fatalf("UID MOVE = %#v, %v", moved, err)
	}

	second, err := client.Append(ctx, "INBOX", nil, int64(len(raw)), bytes.NewReader(raw)).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StoreUID(imap.UIDSetNum(second.UID), []imap.Flag{imap.FlagDeleted}, &imapclient.StoreOptions{Op: imapclient.StoreFlagsAdd, Silent: true}).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Expunge(nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}

	for len(exists) != 0 {
		<-exists
	}
	idle := client.Idle(nil)
	if err := idle.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	writer, writerDone := openLoopbackClient(t, ctx, server, &imapclient.Options{AllowInsecureAuth: true})
	if err := writer.Authenticate(ctx, "alice", "secret", &imapclient.AuthenticateOptions{Mechanism: "PLAIN"}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Append(ctx, "INBOX", nil, int64(len(raw)), bytes.NewReader(raw)).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case count := <-exists:
		if count != 1 {
			t.Fatalf("IDLE EXISTS = %d, want 1", count)
		}
	case <-ctx.Done():
		t.Fatal("IDLE did not deliver concurrent APPEND")
	}
	if err := idle.Done(); err != nil {
		t.Fatal(err)
	}
	if err := idle.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := writer.Logout(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := client.CloseMailbox(nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	readOnly, err := client.Examine("Moved", nil).Wait(ctx)
	if err != nil || !readOnly.ReadOnly || readOnly.NumMessages != 1 {
		t.Fatalf("EXAMINE = %#v, %v", readOnly, err)
	}
	err = client.Store(imap.SeqSetNum(1), []imap.Flag{imap.FlagFlagged}, &imapclient.StoreOptions{Op: imapclient.StoreFlagsAdd}).Wait(ctx)
	var protocolErr *imap.Error
	if !errors.As(err, &protocolErr) || protocolErr.Code != imap.CodeReadOnly {
		t.Fatalf("read-only STORE = %v", err)
	}
	if err := client.Expunge(nil).Wait(ctx); !errors.As(err, &protocolErr) || protocolErr.Code != imap.CodeReadOnly {
		t.Fatalf("read-only EXPUNGE = %v", err)
	}
	if err := client.CloseMailbox(nil).Wait(ctx); err != nil {
		t.Fatalf("read-only CLOSE: %v", err)
	}
	if err := client.Logout(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMoveRefusedWithoutCapabilityWitnessOrSelectedInterface(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	inner := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	server := imapserver.New(&withoutMoveBackend{inner: inner}, &imapserver.Options{AllowInsecureAuth: true})
	serverSide, clientSide := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide) }()
	reader := bufio.NewReader(clientSide)
	greeting, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(greeting, " MOVE") {
		t.Fatalf("MOVE advertised without witness: %q", greeting)
	}
	writeRawCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
	readUntilTag(t, reader, "A1 OK ")
	writeRawCommand(t, clientSide, "A2 SELECT INBOX\r\n")
	readUntilTag(t, reader, "A2 OK ")
	writeRawCommand(t, clientSide, "A3 MOVE 1 Archive\r\n")
	line := readUntilTag(t, reader, "A3 ")
	if !strings.HasPrefix(line, "A3 NO ") || !strings.Contains(line, "[CANNOT]") {
		t.Fatalf("MOVE refusal = %q", line)
	}
	writeRawCommand(t, clientSide, "A4 LOGOUT\r\n")
	readUntilTag(t, reader, "A4 OK ")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type withoutMoveBackend struct{ inner *memory.Backend }

func (b *withoutMoveBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.inner.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &withoutMoveSession{Session: session}, nil
}

type withoutMoveSession struct{ imapserver.Session }

func (s *withoutMoveSession) Select(ctx context.Context, mailbox string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, mailbox, updater, options)
	if err != nil {
		return nil, err
	}
	copyResult := *result
	copyResult.Mailbox = &withoutMoveMailbox{SelectedMailbox: result.Mailbox}
	return &copyResult, nil
}

type withoutMoveMailbox struct{ imapserver.SelectedMailbox }

func openLoopbackClient(t *testing.T, ctx context.Context, server *imapserver.Server, options *imapclient.Options) (*imapclient.Client, <-chan error) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide) }()
	client := imapclient.NewClient(clientSide, options)
	if err := client.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	return client, done
}

func containsMailbox(data []*imap.ListData, name string) bool {
	for _, item := range data {
		if item != nil && item.Mailbox == name {
			return true
		}
	}
	return false
}

func writeRawCommand(t *testing.T, conn net.Conn, command string) {
	t.Helper()
	if _, err := io.WriteString(conn, command); err != nil {
		t.Fatal(err)
	}
}

func readUntilTag(t *testing.T, reader *bufio.Reader, prefix string) string {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, prefix) {
			return line
		}
		if strings.HasPrefix(line, "*") || strings.HasPrefix(line, "+") {
			continue
		}
		t.Fatalf("unexpected response while waiting for %q: %q", prefix, line)
	}
}
