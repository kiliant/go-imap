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
	wantItems := []imap.ListExtendedItem{{Tag: "CHILDINFO", Value: `("SUBSCRIBED")`}}
	if data[0].Mailbox != "Foo" || !reflect.DeepEqual(data[0].ChildInfo, []string{"SUBSCRIBED"}) || !reflect.DeepEqual(data[0].Extended, wantItems) {
		t.Fatalf("parent = %#v, want ChildInfo [SUBSCRIBED], kept raw too", data[0])
	}
	if data[1].ChildInfo != nil || data[1].Extended != nil {
		t.Fatalf("child carries extended data it was not sent: %#v", data[1])
	}
}

func TestReadListExtendedData(t *testing.T) {
	item := func(tag, value string) imap.ListExtendedItem { return imap.ListExtendedItem{Tag: tag, Value: value} }
	tests := []struct {
		name string
		wire string
		want ListData
	}{
		{"none", "\r\n", ListData{}},
		{
			"childinfo",
			` ("CHILDINFO" ("SUBSCRIBED" "x-opt"))` + "\r\n",
			ListData{
				ChildInfo: []string{"SUBSCRIBED", "X-OPT"},
				Extended:  []imap.ListExtendedItem{item("CHILDINFO", `("SUBSCRIBED" "x-opt")`)},
			},
		},
		{
			"childinfo as atoms",
			` (childinfo (subscribed))` + "\r\n",
			ListData{
				ChildInfo: []string{"SUBSCRIBED"},
				Extended:  []imap.ListExtendedItem{item("CHILDINFO", "(subscribed)")},
			},
		},
		{
			"childinfo option value keeps its case",
			` ("CHILDINFO" ("x-opt Value"))` + "\r\n",
			ListData{
				ChildInfo: []string{"X-OPT Value"},
				Extended:  []imap.ListExtendedItem{item("CHILDINFO", `("x-opt Value")`)},
			},
		},
		{
			"every item kept raw in order",
			` ("X-NUMBER" 12 "OLDNAME" ("Old") "X-NESTED" (a (b "c d") {3}` + "\r\nxyz) \"CHILDINFO\" (\"SUBSCRIBED\") x-atom foo)\r\n",
			ListData{
				OldName:   "Old",
				ChildInfo: []string{"SUBSCRIBED"},
				Extended: []imap.ListExtendedItem{
					item("X-NUMBER", "12"),
					item("OLDNAME", `("Old")`),
					item("X-NESTED", "(a (b \"c d\") {3}\r\nxyz)"),
					item("CHILDINFO", `("SUBSCRIBED")`),
					item("X-ATOM", "foo"),
				},
			},
		},
		{
			"oldname is decoded from modified UTF-7",
			` ("OLDNAME" ("P&AOQ-rent"))` + "\r\n",
			ListData{
				OldName:  "Pärent",
				Extended: []imap.ListExtendedItem{item("OLDNAME", `("P&AOQ-rent")`)},
			},
		},
		{
			"repeated items: the first that decodes is used, all are kept",
			` ("CHILDINFO" ("SUBSCRIBED") "OLDNAME" ("") "CHILDINFO" ("REMOTE") "OLDNAME" ("A") "OLDNAME" ("B"))` + "\r\n",
			ListData{
				OldName:   "A",
				ChildInfo: []string{"SUBSCRIBED"},
				Extended: []imap.ListExtendedItem{
					item("CHILDINFO", `("SUBSCRIBED")`),
					item("OLDNAME", `("")`),
					item("CHILDINFO", `("REMOTE")`),
					item("OLDNAME", `("A")`),
					item("OLDNAME", `("B")`),
				},
			},
		},
		{
			"malformed values are kept raw and decode to nothing",
			` ("CHILDINFO" "SUBSCRIBED" "CHILDINFO" () "CHILDINFO" (("SUBSCRIBED")) "CHILDINFO" ("") "OLDNAME" "Old" "OLDNAME" ("A" "B") "OLDNAME" () "X-B" 2)` + "\r\n",
			ListData{
				Extended: []imap.ListExtendedItem{
					item("CHILDINFO", `"SUBSCRIBED"`),
					item("CHILDINFO", "()"),
					item("CHILDINFO", `(("SUBSCRIBED"))`),
					item("CHILDINFO", `("")`),
					item("OLDNAME", `"Old"`),
					item("OLDNAME", `("A" "B")`),
					item("OLDNAME", "()"),
					item("X-B", "2"),
				},
			},
		},
		{
			"unparseable extended data costs all of it",
			` ("X-A" 1 "CHILDINFO" ("SUBSCRIBED") "X-BROKEN")` + "\r\n",
			ListData{},
		},
		{
			"an empty tag costs all of it",
			` ("X-A" 1 "" "")` + "\r\n",
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

// With UTF8=ACCEPT enabled a mailbox name is raw UTF-8, not modified UTF-7, and
// OLDNAME, decoded from the captured bytes, must follow the connection's mode.
func TestReadListExtendedDataOldNameUTF8(t *testing.T) {
	dec := imapwire.NewDecoderString(` ("OLDNAME" ("Pär&-ent"))`+"\r\n", &imapwire.Options{UTF8Accept: true})
	got, err := readListExtendedData(dec)
	if err != nil {
		t.Fatal(err)
	}
	if got.OldName != "Pär&-ent" {
		t.Fatalf("OldName = %q, want the raw UTF-8 name", got.OldName)
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
		Extended:  []imap.ListExtendedItem{{Tag: "X-BIG"}, {Tag: "CHILDINFO", Value: `("SUBSCRIBED")`}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}
