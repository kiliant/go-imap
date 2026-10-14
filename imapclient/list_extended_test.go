package imapclient

import (
	"bufio"
	"context"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// TestListRecursiveMatchChildInfo runs RFC 5258 section 3.5's example: an
// unsubscribed parent is returned because a child is subscribed, and says so
// with CHILDINFO.
func TestListRecursiveMatchChildInfo(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	commands := make(chan string, 1)
	go func() {
		r := bufio.NewReader(serverConn)
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IMAP4rev1 LIST-EXTENDED] ready\r\n"))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		commands <- line
		_, _ = serverConn.Write([]byte("* LIST () \"/\" \"Foo\" (\"CHILDINFO\" (\"SUBSCRIBED\"))\r\n" +
			"* LIST (\\Subscribed) \"/\" \"Foo/Bar\"\r\n" +
			strings.Fields(line)[0] + " OK done\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	data, err := c.List("", "*", &ListOptions{
		SelectionOptions: []ListSelectOption{ListSelectSubscribed, ListSelectRecursiveMatch},
	}).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := <-commands; !strings.Contains(got, `LIST (SUBSCRIBED RECURSIVEMATCH) "" *`) {
		t.Fatalf("command = %q", got)
	}
	if len(data) != 2 {
		t.Fatalf("got %d mailboxes, want 2", len(data))
	}
	if data[0].Mailbox != "Foo" || !reflect.DeepEqual(data[0].ChildInfo, []string{"SUBSCRIBED"}) || data[0].Extended != nil {
		t.Fatalf("parent = %#v, want ChildInfo [SUBSCRIBED]", data[0])
	}
	if data[1].ChildInfo != nil || data[1].Extended != nil {
		t.Fatalf("child carries extended data it was not sent: %#v", data[1])
	}
}

func TestReadListExtendedData(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want ListData
	}{
		{"none", "\r\n", ListData{}},
		{
			"childinfo",
			` ("CHILDINFO" ("SUBSCRIBED" "x-opt"))` + "\r\n",
			ListData{ChildInfo: []string{"SUBSCRIBED", "X-OPT"}},
		},
		{
			"childinfo as atoms",
			` (CHILDINFO (subscribed))` + "\r\n",
			ListData{ChildInfo: []string{"SUBSCRIBED"}},
		},
		{
			"unknown items kept raw in order",
			` ("X-NUMBER" 12 "OLDNAME" ("Old") "X-NESTED" (a (b "c d") {3}` + "\r\nxyz) \"CHILDINFO\" (\"SUBSCRIBED\") x-atom foo)\r\n",
			ListData{
				OldName:   "Old",
				ChildInfo: []string{"SUBSCRIBED"},
				Extended: []imap.ListExtendedItem{
					{Tag: "X-NUMBER", Value: "12"},
					{Tag: "X-NESTED", Value: "(a (b \"c d\") {3}\r\nxyz)"},
					{Tag: "X-ATOM", Value: "foo"},
				},
			},
		},
		{
			"repeated childinfo keeps the first",
			` ("CHILDINFO" ("SUBSCRIBED") "CHILDINFO" ("REMOTE"))` + "\r\n",
			ListData{ChildInfo: []string{"SUBSCRIBED"}},
		},
		{
			"malformed childinfo is skipped, the rest kept",
			` ("CHILDINFO" "SUBSCRIBED" "X-A" 1 "CHILDINFO" () "CHILDINFO" (("SUBSCRIBED")) "CHILDINFO" ("") "CHILDINFO" ("REMOTE") "X-B" 2)` + "\r\n",
			ListData{
				ChildInfo: []string{"REMOTE"},
				Extended: []imap.ListExtendedItem{
					{Tag: "X-A", Value: "1"},
					{Tag: "X-B", Value: "2"},
				},
			},
		},
		{
			"unparseable extended data costs all of it",
			` ("X-A" 1 "CHILDINFO" ("SUBSCRIBED") "X-BROKEN")` + "\r\n",
			ListData{},
		},
		{
			"not a list",
			` "CHILDINFO" ("SUBSCRIBED")` + "\r\n",
			ListData{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dec := imapwire.NewDecoderString(test.wire, nil)
			got, err := readListExtendedData(dec)
			if err != nil {
				t.Fatalf("readListExtendedData() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %#v\nwant %#v", got, test.want)
			}
			if !dec.AtEOF() {
				t.Fatalf("decoder not at the end of the response: %v", dec.Err())
			}
		})
	}
}

// An unknown item too large to hold is consumed, so the response and the items
// after it survive, and is kept with an empty Value.
func TestReadListExtendedDataOversizedItem(t *testing.T) {
	wire := ` ("X-BIG" {20}` + "\r\n" + strings.Repeat("x", 20) + ` "CHILDINFO" ("SUBSCRIBED"))` + "\r\n"
	dec := imapwire.NewDecoderString(wire, &imapwire.Options{MaxBufferedLiteralSize: 16})
	got, err := readListExtendedData(dec)
	if err != nil {
		t.Fatal(err)
	}
	want := ListData{
		ChildInfo: []string{"SUBSCRIBED"},
		Extended:  []imap.ListExtendedItem{{Tag: "X-BIG"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}
