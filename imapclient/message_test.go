package imapclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
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

// TestFetchTwoHundredMegabyteBodyStreamsWithFlatAllocation exercises the
// public Client/FETCH path, rather than only the wire decoder. In particular,
// the FETCH collector must hand the literal to its caller before it continues
// parsing the rest of the response.
func TestFetchTwoHundredMegabyteBodyStreamsWithFlatAllocation(t *testing.T) {
	if testing.Short() {
		t.Skip("200 MiB streaming regression")
	}
	const size = int64(200 << 20)
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	serverDone := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		if _, err := io.WriteString(serverConn, "* OK ready\r\n"); err != nil {
			serverDone <- err
			return
		}
		line, err := bufio.NewReader(serverConn).ReadString('\n')
		if err != nil {
			serverDone <- err
			return
		}
		tag := strings.Fields(line)[0]
		if _, err := io.WriteString(serverConn, fmt.Sprintf("* 1 FETCH (BODY[] {%d}\r\n", size)); err != nil {
			serverDone <- err
			return
		}
		if _, err := io.Copy(serverConn, io.LimitReader(repeatedMessageByte('x'), size)); err != nil {
			serverDone <- err
			return
		}
		_, err = io.WriteString(serverConn, ")\r\n"+tag+" OK done\r\n")
		serverDone <- err
	}()

	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state = StateSelected
	c.mu.Unlock()

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	cmd := c.Fetch(imap.SeqSetNum(1), &imap.FetchItemBodySection{Peek: true})
	data, err := cmd.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	items := data.Items[imap.FetchDataKey("BODY[]")]
	if len(items) != 1 {
		t.Fatalf("BODY[] items = %#v", items)
	}
	section, ok := items[0].(*imap.FetchDataBodySection)
	if !ok {
		t.Fatalf("BODY[] value = %T, want *imap.FetchDataBodySection", items[0])
	}
	n, err := io.Copy(io.Discard, section.Literal)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if n != size {
		t.Fatalf("streamed %d bytes, want %d", n, size)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
		t.Fatalf("FETCH allocated %d bytes for a %d-byte literal; want <= %d (streaming)", allocated, size, 2<<20)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

type repeatedMessageByte byte

func (r repeatedMessageByte) Read(dst []byte) (int, error) {
	for i := range dst {
		dst[i] = byte(r)
	}
	return len(dst), nil
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
		_, _ = serverConn.Write([]byte(strings.Fields(line)[0] + " OK [APPENDUID 38505 3955] appended\r\n"))
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
	if data == nil || data.UIDValidity != 38505 || data.UID != 3955 {
		t.Fatalf("APPEND data = %#v, want APPENDUID 38505 3955", data)
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
		_, _ = serverConn.Write([]byte(fields[0] + " OK [COPYUID 38505 304 3956] copied\r\n"))
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
	if data == nil || !data.Received() || data.UIDValidity != 38505 ||
		!data.SourceUIDs.Equal(imap.UIDSetNum(304)) ||
		!data.DestinationUIDs.Equal(imap.UIDSetNum(3956)) {
		t.Fatalf("COPY data = %#v, want COPYUID 38505 304 3956", data)
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
