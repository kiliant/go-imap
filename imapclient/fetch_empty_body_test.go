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

// TestFetchEmptyBodyIsDeliveredComplete pins that a FETCH response whose body
// section is NIL or a zero-length literal is delivered only once complete.
// There is nothing to stream in either case. Before, the response was handed
// over at the body while the reader goroutine went on appending the items after
// it — here UID and FLAGS — to the map the caller was already reading: a data
// race, and a fatal concurrent map access at worst.
func TestFetchEmptyBodyIsDeliveredComplete(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		if _, err := io.WriteString(serverConn, "* OK ready\r\n"); err != nil {
			return
		}
		line, err := bufio.NewReader(serverConn).ReadString('\n')
		if err != nil {
			return
		}
		tag := strings.Fields(line)[0]
		_, _ = io.WriteString(serverConn, "* 1 FETCH (BODY[] {0}\r\n UID 5 FLAGS (\\Seen))\r\n"+
			"* 2 FETCH (BODY[] NIL UID 6 FLAGS ())\r\n"+
			tag+" OK done\r\n")
	}()

	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state = StateSelected
	c.mu.Unlock()

	cmd := c.FetchUID(imap.UIDSetNum(5, 6), nil, imap.FetchItemUID, imap.FetchItemFlags, &imap.FetchItemBodySection{Peek: true})
	for _, want := range []imap.UID{5, 6} {
		data, err := cmd.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var uid imap.UID
		var body *imap.FetchDataBodySection
		for _, values := range data.Items {
			for _, v := range values {
				switch v := v.(type) {
				case imap.FetchDataUID:
					uid = imap.UID(v)
				case *imap.FetchDataBodySection:
					body = v
				}
			}
		}
		if uid != want {
			t.Fatalf("response delivered without the UID after its empty body: UID = %d, want %d", uid, want)
		}
		if body == nil {
			t.Fatalf("UID %d: no BODY[] item", want)
		}
		if n, err := io.Copy(io.Discard, body.Literal); n != 0 || err != nil {
			t.Fatalf("UID %d: empty body read %d octets, %v", want, n, err)
		}
		if closer, ok := body.Literal.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := cmd.Next(ctx); err != io.EOF {
		t.Fatalf("after both responses: %v, want EOF", err)
	}
}
