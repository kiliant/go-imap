package imapserver_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// TestLoopbackListRecursiveMatch runs RFC 5258 section 3.5's scenario end to
// end: a parent is returned because a descendant is subscribed, and says so
// with CHILDINFO, whether the parent is a mailbox or only a hierarchy level.
//
// It runs against memory, which evaluates RECURSIVEMATCH itself, and against
// a backend without the "LIST-EXTENDED" witness, for which the framework
// evaluates it. The two must answer alike.
func TestLoopbackListRecursiveMatch(t *testing.T) {
	for _, test := range []struct {
		name    string
		backend func(*testing.T, *memory.Backend) imapserver.Backend
	}{
		{"backend", func(_ *testing.T, b *memory.Backend) imapserver.Backend { return b }},
		{"framework", func(t *testing.T, b *memory.Backend) imapserver.Backend {
			return &withoutRecursiveMatchBackend{inner: b, t: t}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			testListRecursiveMatch(t, func(b *memory.Backend) imapserver.Backend { return test.backend(t, b) })
		})
	}
}

func testListRecursiveMatch(t *testing.T, backend func(*memory.Backend) imapserver.Backend) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	inner := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	server := imapserver.New(backend(inner), &imapserver.Options{AllowInsecureAuth: true})
	client, _ := openLoopbackClient(t, ctx, server, &imapclient.Options{AllowInsecureAuth: true})
	if err := client.Login(ctx, "alice", "secret", nil); err != nil {
		t.Fatal(err)
	}

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

	// The reference is prefixed to the pattern: "Deep/" and "%" select the
	// level below Deep, which is not a mailbox either.
	referenced, err := client.List("Deep/", "%", &imapclient.ListOptions{
		SelectionOptions: []imapclient.ListSelectOption{imapclient.ListSelectSubscribed, imapclient.ListSelectRecursiveMatch},
	}).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(referenced) != 1 || referenced[0].Mailbox != "Deep/Er" || !reflect.DeepEqual(referenced[0].ChildInfo, childInfo) || !imap.ContainsAttr(referenced[0].Attrs, imap.MailboxAttrNonExistent) {
		t.Errorf(`LIST "Deep/" "%%" = %#v, want only Deep/Er, \NonExistent with CHILDINFO (SUBSCRIBED)`, referenced)
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

// withoutRecursiveMatchBackend hides the memory backend's "LIST-EXTENDED"
// witness, so the framework evaluates RECURSIVEMATCH, and fails the test if
// the option reaches the session anyway. Names starting with hideFromStar, when
// set, are left out of LIST "" "*", as RFC 3501 section 6.3.8 lets a server
// do for some namespaces.
type withoutRecursiveMatchBackend struct {
	inner        *memory.Backend
	t            *testing.T
	hideFromStar string
}

func (b *withoutRecursiveMatchBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.inner.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &withoutRecursiveMatchSession{Session: session, t: b.t, hideFromStar: b.hideFromStar}, nil
}

func (b *withoutRecursiveMatchBackend) SupportsCapability(ctx context.Context, name string, options *imapserver.CapabilitySupportOptions) bool {
	return name != "LIST-EXTENDED" && b.inner.SupportsCapability(ctx, name, options)
}

type withoutRecursiveMatchSession struct {
	imapserver.Session
	t            *testing.T
	hideFromStar string
}

func (s *withoutRecursiveMatchSession) List(ctx context.Context, writer *imapserver.ListWriter, reference string, patterns []string, options *imapserver.ListOptions) error {
	if options != nil && options.SelectRecursiveMatch {
		s.t.Errorf("SelectRecursiveMatch reached a backend without the LIST-EXTENDED witness")
	}
	if s.hideFromStar == "" || reference != "" || len(patterns) != 1 || patterns[0] != "*" {
		return s.Session.List(ctx, writer, reference, patterns, options)
	}
	return s.Session.List(ctx, &imapserver.ListWriter{WriteFunc: func(ctx context.Context, data *imap.ListData) error {
		if strings.HasPrefix(data.Mailbox, s.hideFromStar) {
			return nil
		}
		return writer.WriteList(ctx, data)
	}}, reference, patterns, options)
}

// RFC 5258 section 6 makes SPECIAL-USE an independent option, which does not
// interact with RECURSIVEMATCH: a parent counts a subscribed descendant
// whether or not that descendant is special-use, and SPECIAL-USE then
// restricts the result like any other. Both evaluations must agree on that,
// and must not report an existing parent as \NonExistent.
func TestLoopbackListRecursiveMatchSpecialUse(t *testing.T) {
	for _, test := range []struct {
		name    string
		backend func(*testing.T, *memory.Backend) imapserver.Backend
	}{
		{"backend", func(_ *testing.T, b *memory.Backend) imapserver.Backend { return b }},
		{"framework", func(t *testing.T, b *memory.Backend) imapserver.Backend {
			return &withoutRecursiveMatchBackend{inner: b, t: t}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			inner := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
			server := imapserver.New(test.backend(t, inner), &imapserver.Options{AllowInsecureAuth: true})
			client, _ := openLoopbackClient(t, ctx, server, &imapclient.Options{AllowInsecureAuth: true})
			if err := client.Login(ctx, "alice", "secret", nil); err != nil {
				t.Fatal(err)
			}
			for mailbox, use := range map[string]imap.MailboxAttr{"Arch": imap.MailboxAttrArchive, "Sent": imap.MailboxAttrSent} {
				if err := client.Create(mailbox, &imapclient.CreateOptions{SpecialUse: []imap.MailboxAttr{use}}).Wait(ctx); err != nil {
					t.Fatalf("CREATE %s: %v", mailbox, err)
				}
			}
			for _, mailbox := range []string{"Arch/2024", "Pers", "Pers/Child"} {
				if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
					t.Fatalf("CREATE %s: %v", mailbox, err)
				}
			}
			for _, mailbox := range []string{"Arch/2024", "Sent", "Pers", "Pers/Child"} {
				if err := client.Subscribe(mailbox, nil).Wait(ctx); err != nil {
					t.Fatalf("SUBSCRIBE %s: %v", mailbox, err)
				}
			}
			data, err := client.List("", "%", &imapclient.ListOptions{
				SelectionOptions: []imapclient.ListSelectOption{imapclient.ListSelectSubscribed, imapclient.ListSelectSpecialUse, imapclient.ListSelectRecursiveMatch},
			}).Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			byName := make(map[string]*imapclient.ListData, len(data))
			for _, item := range data {
				byName[item.Mailbox] = item
			}
			if len(byName) != 2 || byName["Arch"] == nil || byName["Sent"] == nil {
				t.Fatalf(`LIST (SUBSCRIBED SPECIAL-USE RECURSIVEMATCH) "" "%%" = %v, want Arch and Sent`, keys(byName))
			}
			arch := byName["Arch"]
			if !reflect.DeepEqual(arch.ChildInfo, []string{"SUBSCRIBED"}) || !imap.ContainsAttr(arch.Attrs, imap.MailboxAttrArchive) ||
				imap.ContainsAttr(arch.Attrs, imap.MailboxAttrSubscribed) || imap.ContainsAttr(arch.Attrs, imap.MailboxAttrNonExistent) {
				t.Errorf("Arch = %#v, want \\Archive and CHILDINFO (SUBSCRIBED), not \\Subscribed or \\NonExistent", arch)
			}
			if sent := byName["Sent"]; sent.ChildInfo != nil || !imap.ContainsAttr(sent.Attrs, imap.MailboxAttrSubscribed) {
				t.Errorf("Sent = %#v, want \\Subscribed without CHILDINFO", sent)
			}
		})
	}
}

// RECURSIVEMATCH only adds parents (RFC 5258 section 3.1), so the framework's
// evaluation must never return less than the same command without it, even
// for names the backend leaves out of LIST "" "*".
func TestLoopbackListRecursiveMatchKeepsHiddenNames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	inner := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	server := imapserver.New(&withoutRecursiveMatchBackend{inner: inner, t: t, hideFromStar: "#shared"}, &imapserver.Options{AllowInsecureAuth: true})
	client, _ := openLoopbackClient(t, ctx, server, &imapclient.Options{AllowInsecureAuth: true})
	if err := client.Login(ctx, "alice", "secret", nil); err != nil {
		t.Fatal(err)
	}
	for _, mailbox := range []string{"#shared", "#shared/team"} {
		if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
			t.Fatalf("CREATE %s: %v", mailbox, err)
		}
	}
	if err := client.Subscribe("#shared/team", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	for _, selection := range [][]imapclient.ListSelectOption{
		{imapclient.ListSelectSubscribed},
		{imapclient.ListSelectSubscribed, imapclient.ListSelectRecursiveMatch},
	} {
		data, err := client.List("", "#shared/*", &imapclient.ListOptions{SelectionOptions: selection}).Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 1 || data[0].Mailbox != "#shared/team" {
			t.Errorf(`LIST %v "" "#shared/*" = %v, want #shared/team`, selection, listNames(data))
		}
	}
}
