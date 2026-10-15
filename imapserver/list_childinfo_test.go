package imapserver_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

// TestLoopbackListRecursiveMatch runs RFC 5258 section 3.5's scenario end to
// end: a parent is returned because a descendant is subscribed, and says so
// with CHILDINFO, whether the parent is a mailbox or only a hierarchy level.
func TestLoopbackListRecursiveMatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, _ := newGroupAClient(t, ctx)

	// Deep and Deep/Er are never created, so they exist only as levels.
	for _, mailbox := range []string{"Foo", "Foo/Bar", "Moo", "Deep/Er/Child", "Subbed", "Subbed/Too"} {
		if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
			t.Fatalf("CREATE %s: %v", mailbox, err)
		}
	}
	for _, mailbox := range []string{"Foo/Bar", "Deep/Er/Child", "Subbed", "Subbed/Too"} {
		if err := client.Subscribe(mailbox, nil).Wait(ctx); err != nil {
			t.Fatalf("SUBSCRIBE %s: %v", mailbox, err)
		}
	}

	list := func(pattern string) map[string]*imapclient.ListData {
		t.Helper()
		data, err := client.List("", pattern, &imapclient.ListOptions{
			SelectionOptions: []imapclient.ListSelectOption{imapclient.ListSelectSubscribed, imapclient.ListSelectRecursiveMatch},
		}).Wait(ctx)
		if err != nil {
			t.Fatalf("LIST (SUBSCRIBED RECURSIVEMATCH) %q: %v", pattern, err)
		}
		byName := make(map[string]*imapclient.ListData, len(data))
		for _, item := range data {
			byName[item.Mailbox] = item
		}
		return byName
	}
	childInfo := []string{"SUBSCRIBED"}

	top := list("%")
	if len(top) != 3 || top["Foo"] == nil || top["Deep"] == nil || top["Subbed"] == nil {
		t.Fatalf(`LIST "%%" returned %v, want Foo, Deep and Subbed`, keys(top))
	}
	if got := top["Foo"]; !reflect.DeepEqual(got.ChildInfo, childInfo) || imap.ContainsAttr(got.Attrs, imap.MailboxAttrSubscribed) {
		t.Errorf("unsubscribed parent Foo = %#v, want CHILDINFO (SUBSCRIBED) and no \\Subscribed", got)
	}
	if got := top["Deep"]; !reflect.DeepEqual(got.ChildInfo, childInfo) || !imap.ContainsAttr(got.Attrs, imap.MailboxAttrNonExistent) {
		t.Errorf("hierarchy level Deep = %#v, want \\NonExistent with CHILDINFO (SUBSCRIBED)", got)
	}
	if got := top["Subbed"]; !reflect.DeepEqual(got.ChildInfo, childInfo) || !imap.ContainsAttr(got.Attrs, imap.MailboxAttrSubscribed) {
		t.Errorf("subscribed parent Subbed = %#v, want \\Subscribed with CHILDINFO (SUBSCRIBED)", got)
	}

	all := list("*")
	for _, name := range []string{"Foo", "Foo/Bar", "Deep", "Deep/Er", "Deep/Er/Child", "Subbed", "Subbed/Too"} {
		if all[name] == nil {
			t.Errorf(`LIST "*" did not return %s: %v`, name, keys(all))
		}
	}
	for _, leaf := range []string{"Foo/Bar", "Deep/Er/Child", "Subbed/Too"} {
		if got := all[leaf]; got != nil && got.ChildInfo != nil {
			t.Errorf("%s has no subscribed descendant but carries CHILDINFO %q", leaf, got.ChildInfo)
		}
	}
	if all["Moo"] != nil || all["INBOX"] != nil {
		t.Errorf(`LIST "*" returned a mailbox with nothing subscribed in it: %v`, keys(all))
	}

	plain, err := client.List("", "*", &imapclient.ListOptions{
		SelectionOptions: []imapclient.ListSelectOption{imapclient.ListSelectSubscribed},
	}).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 4 {
		t.Errorf("LIST (SUBSCRIBED) returned %d mailboxes, want the 4 subscribed ones", len(plain))
	}
	for _, item := range plain {
		if item.ChildInfo != nil {
			t.Errorf("LIST without RECURSIVEMATCH carried CHILDINFO: %#v", item)
		}
	}
}

// CHILDINFO is written only for LIST with RECURSIVEMATCH, and RECURSIVEMATCH
// is accepted only beside a base selection option. REMOTE and SPECIAL-USE are
// independent options, so neither qualifies (RFC 5258 section 6; RFC 6154's
// formal syntax), and RFC 5258 section 3 has the server answer BAD.
func TestLoopbackListRecursiveMatchWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, conn, reader := newGroupARawSession(t, ctx)

	for i, command := range []string{"CREATE Foo/Bar", "SUBSCRIBE Foo/Bar"} {
		tag := "B" + string(rune('a'+i))
		writeRawCommand(t, conn, tag+" "+command+"\r\n")
		if _, tagged := collectUntilTag(t, reader, tag+" "); !strings.HasPrefix(tagged, tag+" OK") {
			t.Fatalf("%s failed: %q", command, tagged)
		}
	}

	writeRawCommand(t, conn, "C1 LIST (SUBSCRIBED RECURSIVEMATCH) \"\" \"%\"\r\n")
	untagged, tagged := collectUntilTag(t, reader, "C1 ")
	if !strings.HasPrefix(tagged, "C1 OK") {
		t.Fatalf("LIST (SUBSCRIBED RECURSIVEMATCH) failed: %q", tagged)
	}
	// Foo was never created: RFC 5258 section 5, example 9's "qux2".
	if want := `* LIST (\NonExistent) "/" Foo ("CHILDINFO" ("SUBSCRIBED"))`; len(untagged) != 1 || untagged[0] != want {
		t.Fatalf("LIST (SUBSCRIBED RECURSIVEMATCH) = %q, want %q", untagged, want)
	}

	for _, command := range []string{
		`LIST (SUBSCRIBED) "" "*"`,
		`LIST "" "*"`,
		`LSUB "" "*"`,
	} {
		writeRawCommand(t, conn, "C2 "+command+"\r\n")
		untagged, tagged := collectUntilTag(t, reader, "C2 ")
		if !strings.HasPrefix(tagged, "C2 OK") {
			t.Fatalf("%s failed: %q", command, tagged)
		}
		for _, line := range untagged {
			if strings.Contains(line, "CHILDINFO") {
				t.Errorf("%s wrote CHILDINFO without RECURSIVEMATCH: %q", command, line)
			}
		}
	}

	for _, selection := range []string{"(RECURSIVEMATCH)", "(REMOTE RECURSIVEMATCH)", "(SPECIAL-USE RECURSIVEMATCH)"} {
		writeRawCommand(t, conn, "C3 LIST "+selection+" \"\" \"*\"\r\n")
		if _, tagged := collectUntilTag(t, reader, "C3 "); !strings.HasPrefix(tagged, "C3 BAD") {
			t.Errorf("LIST %s = %q, want BAD", selection, tagged)
		}
	}
	writeRawCommand(t, conn, "C4 LIST (REMOTE SUBSCRIBED RECURSIVEMATCH) \"\" \"%\"\r\n")
	if _, tagged := collectUntilTag(t, reader, "C4 "); !strings.HasPrefix(tagged, "C4 OK") {
		t.Errorf("LIST (REMOTE SUBSCRIBED RECURSIVEMATCH) = %q, want OK", tagged)
	}
}

func keys(m map[string]*imapclient.ListData) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	return names
}
