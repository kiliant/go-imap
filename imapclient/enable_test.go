package imapclient

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestEnableTracksServerSubset(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	requests := make(chan string, 1)
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IMAP4rev1 ENABLE IMAP4rev2] ready\r\n"))
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		requests <- line
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("* ENABLED IMAP4rev2 UTF8=ACCEPT\r\n" + tag + " OK enabled\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	enabled, err := c.Enable("IMAP4rev2", "CONDSTORE", "UTF8=ACCEPT").Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := <-requests; !strings.Contains(got, "ENABLE IMAP4REV2 CONDSTORE UTF8=ACCEPT") {
		t.Fatalf("ENABLE request = %q", got)
	}
	if strings.Join(enabled, ",") != "IMAP4REV2,UTF8=ACCEPT" {
		t.Fatalf("enabled = %#v", enabled)
	}
	if got := c.EnabledCapabilities(); !got["IMAP4REV2"] || !got["UTF8=ACCEPT"] || got["CONDSTORE"] {
		t.Fatalf("tracked enabled capabilities = %#v", got)
	}
	if !c.rev2Enabled() {
		t.Fatal("IMAP4rev2 was not activated")
	}
	if !c.Supports("MOVE") || !c.Supports("LIST-EXTENDED") || !c.Supports("NAMESPACE") || c.Capabilities()["MOVE"] {
		t.Fatalf("rev2 mandatory capability bridge is incorrect: supports MOVE=%t advertised=%#v", c.Supports("MOVE"), c.Capabilities())
	}
}

func TestEnableRequiresAuthenticatedUnselectedState(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() { _, _ = serverConn.Write([]byte("* OK ready\r\n")) }()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Enable("CONDSTORE").Wait(ctx); err == nil {
		t.Fatal("ENABLE succeeded before authentication")
	}
	c.mu.Lock()
	c.state = StateSelected
	c.mu.Unlock()
	if _, err := c.Enable("CONDSTORE").Wait(ctx); err == nil {
		t.Fatal("ENABLE succeeded while selected")
	}
}

func TestEnableAndSelectCannotBePipelined(t *testing.T) {
	t.Run("enable then select", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		request := make(chan string, 1)
		allowCompletion := make(chan struct{})
		go func() {
			_, _ = serverConn.Write([]byte("* PREAUTH ready\r\n"))
			line, _ := bufio.NewReader(serverConn).ReadString('\n')
			request <- line
			<-allowCompletion
			tag := strings.Fields(line)[0]
			_, _ = serverConn.Write([]byte("* ENABLED CONDSTORE\r\n" + tag + " OK enabled\r\n"))
		}()
		c := NewClient(clientConn, nil)
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.WaitGreeting(ctx); err != nil {
			t.Fatal(err)
		}
		enable := c.Enable("CONDSTORE")
		got := <-request
		if _, err := c.Select("INBOX", nil).Wait(ctx); err == nil {
			t.Fatal("SELECT was accepted while ENABLE was in flight")
		}
		if !strings.Contains(got, " ENABLE CONDSTORE\r\n") {
			t.Fatalf("only ENABLE should reach the server, got %q", got)
		}
		close(allowCompletion)
		if _, err := enable.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("select then enable", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		request := make(chan string, 1)
		allowCompletion := make(chan struct{})
		go func() {
			_, _ = serverConn.Write([]byte("* PREAUTH ready\r\n"))
			line, _ := bufio.NewReader(serverConn).ReadString('\n')
			request <- line
			<-allowCompletion
			tag := strings.Fields(line)[0]
			_, _ = serverConn.Write([]byte(tag + " OK selected\r\n"))
		}()
		c := NewClient(clientConn, nil)
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.WaitGreeting(ctx); err != nil {
			t.Fatal(err)
		}
		selectCmd := c.Select("INBOX", nil)
		got := <-request
		if _, err := c.Enable("CONDSTORE").Wait(ctx); err == nil {
			t.Fatal("ENABLE was accepted while SELECT was in flight")
		}
		if !strings.Contains(got, " SELECT INBOX\r\n") {
			t.Fatalf("only SELECT should reach the server, got %q", got)
		}
		close(allowCompletion)
		if _, err := selectCmd.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

func TestEnableWriteFailureDoesNotMaskClosedError(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() { _, _ = serverConn.Write([]byte("* PREAUTH ready\r\n")) }()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	// Space is forbidden in an IMAP atom, so Encoder.Flush fails after the
	// command has allocated its tag but before a tagged completion can arrive.
	_, enableErr := c.Enable("invalid capability").Wait(ctx)
	if enableErr == nil {
		t.Fatal("invalid ENABLE capability succeeded")
	}
	if _, err := c.Select("INBOX", nil).Wait(ctx); err == nil || strings.Contains(err.Error(), "pipelined") || !strings.Contains(err.Error(), "atom") {
		t.Fatalf("SELECT after ENABLE write failure = %v, want terminal write error (ENABLE reported %v)", err, enableErr)
	}
}

func TestEnableAndSelectOnClosedClientReturnCloseError(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	c := NewClient(clientConn, nil)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.Enable("CONDSTORE").Wait(ctx); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("ENABLE after Close = %v, want closed error", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(ctx); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("SELECT after Close = %v, want closed error", err)
	}
}

func TestEnableUTF8AcceptUpdatesMailboxDecoder(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY ENABLE UTF8=ACCEPT] ready\r\n"))
		r := bufio.NewReader(serverConn)
		line, _ := r.ReadString('\n')
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("* ENABLED UTF8=ACCEPT\r\n" + tag + " OK enabled\r\n"))
		line, _ = r.ReadString('\n')
		tag = strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("* LIST () \"/\" 旅行\r\n" + tag + " OK listed\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Enable("UTF8=ACCEPT").Wait(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.List("", "*", nil).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 1 || data[0].Mailbox != "旅行" {
		t.Fatalf("LIST mailbox after ENABLE UTF8=ACCEPT = %#v", data)
	}
}
