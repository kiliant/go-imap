package imapclient

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

func TestFetchBodySectionStreamsAndParsesHeaderFields(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* OK ready\r\n"))
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		_ = line
		_, _ = serverConn.Write([]byte("* 1 FETCH (BODY[HEADER.FIELDS (From To)] {4}\r\nFrom FLAGS (\\Seen))\r\n"))
		_, _ = serverConn.Write([]byte(strings.Fields(line)[0] + " OK done\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state = StateSelected
	c.mu.Unlock()
	cmd := c.Fetch(imap.SeqSetNum(1), &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"From", "To"}, Peek: true}, imap.FetchItemFlags)
	data, err := cmd.Next(ctx)
	if err != nil {
		if e, ok := c.closeErr.(*imap.Error); ok {
			t.Fatalf("Next() = %v; wire error = %v", err, e.Err)
		}
		t.Fatal(err)
	}
	items := data.Items[imap.FetchDataKey("BODY[HEADER.FIELDS (From To)]")]
	if len(items) != 1 {
		t.Fatalf("body items = %#v", items)
	}
	section, ok := items[0].(*imap.FetchDataBodySection)
	if !ok || string(section.Specifier) != "HEADER.FIELDS" {
		t.Fatalf("body section = %#v", items[0])
	}
	if got, err := io.ReadAll(section.Literal); err != nil || string(got) != "From" {
		t.Fatalf("literal = %q, %v", got, err)
	}
	if err := cmd.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAppendSynchronisingLiteral(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* OK ready\r\n"))
		r := bufio.NewReader(serverConn)
		line, _ := r.ReadString('\n')
		if !strings.Contains(line, "APPEND INBOX {3}\r\n") {
			return
		}
		_, _ = serverConn.Write([]byte("+ go ahead\r\n"))
		body := make([]byte, 5)
		_, _ = io.ReadFull(r, body)
		if string(body) != "hey\r\n" {
			return
		}
		_, _ = serverConn.Write([]byte(strings.Fields(line)[0] + " OK appended\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state = StateAuthenticated
	c.mu.Unlock()
	data, err := c.Append(ctx, "INBOX", nil, 3, strings.NewReader("hey")).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if data == nil || data.UIDValidity != 0 || data.UID != 0 {
		t.Fatalf("APPEND data = %#v, want zero UIDPLUS data before parsing is enabled", data)
	}
}

func TestAppendContextCancelsSynchronisingLiteral(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	commandSeen := make(chan struct{})
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* OK ready\r\n"))
		_, _ = bufio.NewReader(serverConn).ReadString('\n')
		close(commandSeen)
		// Do not send the continuation: the context must unblock APPEND.
		_, _ = io.Copy(io.Discard, serverConn)
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state = StateAuthenticated
	c.mu.Unlock()
	commands := make(chan *AppendCommand, 1)
	go func() { commands <- c.Append(ctx, "INBOX", nil, 3, strings.NewReader("hey")) }()
	select {
	case <-commandSeen:
	case <-time.After(time.Second):
		t.Fatal("APPEND did not announce its literal")
	}
	cancel()
	var command *AppendCommand
	select {
	case command = <-commands:
	case <-time.After(time.Second):
		t.Fatal("APPEND did not return after context cancellation")
	}
	if _, err := command.Wait(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("APPEND cancellation = %v, want context.Canceled", err)
	}
}

func TestAppendReaderErrorIsReturnedByTypedWait(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* OK ready\r\n"))
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		if strings.Contains(line, "APPEND INBOX {3}\r\n") {
			_, _ = serverConn.Write([]byte("+ go ahead\r\n"))
		}
		_, _ = io.Copy(io.Discard, serverConn)
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state = StateAuthenticated
	c.mu.Unlock()
	want := errors.New("message reader failed")
	command := c.Append(ctx, "INBOX", nil, 3, errorReader{err: want})
	if _, err := command.Wait(ctx); !errors.Is(err, want) {
		t.Fatalf("APPEND reader error = %v, want %v", err, want)
	}
}

func TestCopyReturnsTypedData(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* PREAUTH ready\r\n"))
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[1] != "COPY" || fields[2] != "1" || fields[3] != "Archive" {
			return
		}
		_, _ = serverConn.Write([]byte(fields[0] + " OK copied\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state = StateSelected
	c.mu.Unlock()
	data, err := c.Copy(imap.SeqSetNum(1), "Archive").Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if data == nil || data.UIDValidity != 0 || data.SourceUIDs != nil || data.DestinationUIDs != nil {
		t.Fatalf("COPY data = %#v, want zero UIDPLUS data before parsing is enabled", data)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestUIDSearchEncodesCompoundCommand(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* PREAUTH ready\r\n"))
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[1] != "UID" || fields[2] != "SEARCH" || fields[3] != "ALL" {
			return
		}
		_, _ = serverConn.Write([]byte("* SEARCH 42\r\n" + fields[0] + " OK searched\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state = StateSelected
	c.mu.Unlock()
	got, err := c.SearchUID(imap.SearchAll, nil).AllUID(ctx)
	if err != nil {
		t.Fatalf("UID SEARCH: %v; wire error: %#v", err, c.closeErr)
	}
	if len(got) != 1 || got[0] != 42 {
		t.Fatalf("UID SEARCH = %v, want [42]", got)
	}
}
