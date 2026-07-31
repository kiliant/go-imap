package imapclient

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func mailboxTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestSelectCollectsStatusAndDetectsUIDValidityChange(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* PREAUTH ready\r\n"))
		r := bufio.NewReader(serverConn)
		line, _ := r.ReadString('\n')
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("* FLAGS (\\Answered \\Seen custom)\r\n" +
			"* 4 EXISTS\r\n* 1 RECENT\r\n" +
			"* OK [PERMANENTFLAGS (\\Answered \\Seen \\*)] flags\r\n" +
			"* OK [UIDNEXT 19] next\r\n* OK [UIDVALIDITY 101] valid\r\n" +
			"* OK [UNSEEN 3] unseen\r\n* OK [HIGHESTMODSEQ 18446744073709551615] modseq\r\n" +
			tag + " OK [READ-WRITE] selected\r\n"))

		line, _ = r.ReadString('\n') // CLOSE
		tag = strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte(tag + " OK closed\r\n"))
		line, _ = r.ReadString('\n')
		tag = strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("* OK [UIDVALIDITY 202] recreated\r\n" + tag + " OK [READ-ONLY] selected\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx := mailboxTestContext(t)
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.Select("INBOX", nil).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if data.NumMessages != 4 || data.NumRecent != 1 || data.UIDNext != 19 || data.UIDValidity != 101 || data.Unseen != 3 || data.HighestModSeq != ^uint64(0) {
		t.Fatalf("SELECT data = %#v", data)
	}
	if len(data.Flags) != 3 || !imap.ContainsFlag(data.PermanentFlags, imap.FlagWildcard) {
		t.Fatalf("SELECT flags = %#v permanent=%#v", data.Flags, data.PermanentFlags)
	}
	if c.State() != StateSelected {
		t.Fatalf("State() after SELECT = %q", c.State())
	}
	if err := c.CloseMailbox().Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if c.State() != StateAuthenticated {
		t.Fatalf("State() after CLOSE = %q", c.State())
	}
	recreated, err := c.Examine("INBOX", nil).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !recreated.UIDValidityChanged || !recreated.ReadOnly {
		t.Fatalf("recreated SELECT data = %#v", recreated)
	}
}

func TestStatusUsesOpenStatusItems(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* PREAUTH ready\r\n"))
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("* STATUS Archive (MESSAGES 3 UIDNEXT 9 UIDVALIDITY 100 UNSEEN 2 HIGHESTMODSEQ 18446744073709551615 X-VENDOR 7)\r\n" + tag + " OK status\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx := mailboxTestContext(t)
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.Status("Archive", &StatusOptions{Items: []imap.StatusItem{
		imap.StatusItemMessages, imap.StatusItemHighestModSeq, imap.StatusItemKeyword("X-VENDOR"),
	}}).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if data.NumMessages != 3 || data.UIDNext != 9 || data.UIDValidity != 100 || data.NumUnseen != 2 || data.HighestModSeq != ^uint64(0) || data.Values["X-VENDOR"] != uint64(7) {
		t.Fatalf("STATUS data = %#v", data)
	}
}

func TestListExtendedOptionsAndLsubMapping(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	requests := make(chan string, 2)
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IMAP4rev1 LIST-EXTENDED] ready\r\n"))
		r := bufio.NewReader(serverConn)
		for range 2 {
			line, _ := r.ReadString('\n')
			if line == "" {
				return
			}
			requests <- line
			tag := strings.Fields(line)[0]
			_, _ = serverConn.Write([]byte("* LIST (\\HasChildren) \"/\" Projects\r\n" + tag + " OK listed\r\n"))
		}
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx := mailboxTestContext(t)
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.List("", "Projects", &ListOptions{
		Patterns:         []string{"Archive"},
		SelectionOptions: []ListSelectOption{ListSelectSubscribed},
		ReturnOptions:    []ListReturnOption{ListReturnChildren},
	}).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 1 || data[0].Delimiter != '/' || data[0].Mailbox != "Projects" || !imap.ContainsAttr(data[0].Attrs, imap.MailboxAttrHasChildren) {
		t.Fatalf("LIST data = %#v", data)
	}
	if request := <-requests; !strings.Contains(request, "LIST (SUBSCRIBED) \"\" (Projects Archive) RETURN (CHILDREN)") {
		t.Fatalf("LIST request = %q", request)
	}
	if _, err := c.Lsub("", "*", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; !strings.Contains(request, "LIST (SUBSCRIBED) \"\" *") {
		t.Fatalf("mapped LSUB request = %q", request)
	}
}

func TestNamespaceAndMailboxNameEncoding(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	requests := make(chan string, 2)
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* PREAUTH ready\r\n"))
		r := bufio.NewReader(serverConn)
		line, _ := r.ReadString('\n')
		requests <- line
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("* NAMESPACE ((\"\" \"/\")) NIL ((\"Shared.\" \".\"))\r\n" + tag + " OK namespace\r\n"))
		line, _ = r.ReadString('\n')
		requests <- line
		tag = strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte(tag + " OK created\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx := mailboxTestContext(t)
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	ns, err := c.Namespace().Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ns.Personal) != 1 || ns.Personal[0].Delimiter != '/' || len(ns.Shared) != 1 || ns.Shared[0].Delimiter != '.' {
		t.Fatalf("NAMESPACE data = %#v", ns)
	}
	if request := <-requests; !strings.Contains(request, " NAMESPACE\r\n") {
		t.Fatalf("NAMESPACE request = %q", request)
	}
	if err := c.Create("旅行").Wait(ctx); err != nil {
		t.Fatal(err)
	}
	encoded, err := imapwire.EncodeMailboxName("旅行")
	if err != nil {
		t.Fatal(err)
	}
	if request := <-requests; !strings.Contains(request, encoded) || strings.Contains(request, "旅行") {
		t.Fatalf("CREATE request = %q", request)
	}
}
