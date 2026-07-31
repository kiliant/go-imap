package imapclient

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
	if err := c.Append("INBOX", nil, 3, strings.NewReader("hey")).Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
