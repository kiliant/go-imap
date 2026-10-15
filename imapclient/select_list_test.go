package imapclient

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// TestSelectForwardsListOnRev1 pins that SELECT claims no LIST on an
// IMAP4rev1 session. Only RFC 9051 section 6.3.2 makes a server send a LIST
// during SELECT, so on rev1 one naming the selected mailbox is an event, such
// as a NOTIFY SubscriptionChange (RFC 5465 section 5.5), and must reach the
// List handler like any other.
func TestSelectForwardsListOnRev1(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	lists := make(chan *ListData, 4)
	go func() {
		r := bufio.NewReader(serverConn)
		_, _ = serverConn.Write([]byte("* PREAUTH ready\r\n"))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("* 3 EXISTS\r\n" +
			"* LIST (\\Subscribed) \"/\" INBOX\r\n" +
			"* LIST () \"/\" Elsewhere\r\n" +
			"* OK [UIDVALIDITY 7] ok\r\n" +
			tag + " OK [READ-WRITE] selected\r\n"))
	}()
	c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
		List: func(data *ListData) { lists <- data },
	}})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	status, err := c.Select("inbox", nil).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.NumMessages != 3 || status.UIDValidity != 7 {
		t.Fatalf("status = %#v", status)
	}
	close(lists)
	var got []string
	for data := range lists {
		got = append(got, data.Mailbox)
	}
	if len(got) != 2 || got[0] != "INBOX" || got[1] != "Elsewhere" {
		t.Fatalf("List handler got %q, want both events in order", got)
	}
}

// TestSelectClaimsListAfterEnable drives IMAP4rev2 through a real ENABLE
// exchange rather than seeding the client's state, so the claim depends on
// what ENABLED actually records.
func TestSelectClaimsListAfterEnable(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	lists := make(chan *ListData, 4)
	go func() {
		r := bufio.NewReader(serverConn)
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IMAP4rev1 IMAP4rev2 ENABLE] ready\r\n"))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		_, _ = serverConn.Write([]byte("* ENABLED IMAP4rev2\r\n" + strings.Fields(line)[0] + " OK enabled\r\n"))
		if line, err = r.ReadString('\n'); err != nil {
			return
		}
		_, _ = serverConn.Write([]byte("* 0 EXISTS\r\n" +
			"* LIST () \"/\" INBOX\r\n" +
			"* OK [UIDVALIDITY 7] ok\r\n" +
			strings.Fields(line)[0] + " OK [READ-WRITE] selected\r\n"))
	}()
	c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
		List: func(data *ListData) { lists <- data },
	}})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Enable(nil, "IMAP4rev2").Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	close(lists)
	for data := range lists {
		t.Fatalf("the SELECT's own LIST reached the List handler: %#v", data)
	}
}
