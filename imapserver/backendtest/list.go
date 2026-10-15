package backendtest

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func runList(t *testing.T, harness *Harness) {
	// REMOTE expands the candidates; it must not exclude local descendants.
	// SPECIAL-USE restricts the final results, not which subscribed descendants
	// contribute CHILDINFO. Exercise both options together as well.
	for _, test := range []struct {
		name    string
		remote  bool
		special bool
	}{
		{name: "list-recursivematch"},
		{name: "list-recursivematch-remote", remote: true},
		{name: "list-recursivematch-special-use", special: true},
		{name: "list-recursivematch-remote-special-use", remote: true, special: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			instance, session := newSession(t, harness)
			defer closeSession(t, instance, session)
			if !supportsCapability(instance, session, "LIST-EXTENDED") {
				t.Skip(`backend does not witness "LIST-EXTENDED"; the framework evaluates RECURSIVEMATCH`)
			}
			if test.special && !supportsCapability(instance, session, "CREATE-SPECIAL-USE") {
				t.Skip("backend cannot create a special-use fixture")
			}
			ctx := context.Background()
			var create *imapserver.CreateOptions
			if test.special {
				create = &imapserver.CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrArchive}}
			}
			if err := session.Create(ctx, "recursive", create); err != nil {
				t.Fatal(err)
			}
			if err := session.Create(ctx, "recursive/child", nil); err != nil {
				t.Fatal(err)
			}
			if err := session.Subscribe(ctx, "recursive/child", nil); err != nil {
				t.Fatal(err)
			}
			options := &imapserver.ListOptions{
				Subscribed: true, SelectRemote: test.remote, SelectSpecialUse: test.special,
				ReturnSubscribed: true, ReturnSpecialUse: test.special,
			}
			if listed := listMailboxes(t, session, "", []string{"recursive"}, options); len(listed) != 0 {
				t.Fatalf("LIST without RECURSIVEMATCH returned the unsubscribed parent: %v", listed)
			}
			options.SelectRecursiveMatch = true
			listed := listMailboxes(t, session, "", []string{"recursive"}, options)
			parent := requireRecursiveParent(t, listed, "recursive")
			if imap.ContainsAttr(parent.Attrs, imap.MailboxAttrSubscribed) || imap.ContainsAttr(parent.Attrs, imap.MailboxAttrNonExistent) {
				t.Fatalf("existing unsubscribed parent has incorrect attributes: %q", parent.Attrs)
			}
			if test.special && !imap.ContainsAttr(parent.Attrs, imap.MailboxAttrArchive) {
				t.Fatalf("special-use parent lost its \\Archive attribute: %q", parent.Attrs)
			}
		})
	}

	t.Run("list-recursivematch-remote-descendant", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		if !supportsCapability(instance, session, "LIST-EXTENDED") {
			t.Skip(`backend does not witness "LIST-EXTENDED"`)
		}
		// There is no protocol operation that creates a remote mailbox. Discover
		// one seeded by the factory instead of adding a production API for it.
		all := listMailboxes(t, session, "", []string{"*"}, &imapserver.ListOptions{SelectRemote: true, ReturnSubscribed: true})
		names := make([]string, 0, len(all))
		for name := range all {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			data := all[name]
			if !imap.ContainsAttr(data.Attrs, imap.MailboxAttrRemote) || data.Delimiter == 0 {
				continue
			}
			at := strings.LastIndex(name, string(data.Delimiter))
			if at <= 0 {
				continue
			}
			parent := name[:at]
			// A literal parent pattern must not also select the child, or CHILDINFO
			// may legitimately be suppressed (RFC 5258 section 3.5).
			if strings.ContainsAny(parent, "*%") || all[parent] != nil && imap.ContainsAttr(all[parent].Attrs, imap.MailboxAttrSubscribed) {
				continue
			}
			if err := session.Subscribe(context.Background(), name, nil); err != nil {
				t.Fatalf("Subscribe remote fixture %q: %v", name, err)
			}
			listed := listMailboxes(t, session, "", []string{parent}, &imapserver.ListOptions{
				Subscribed: true, SelectRemote: true, SelectRecursiveMatch: true,
			})
			requireRecursiveParent(t, listed, parent)
			return
		}
		t.Skip("no remote child beneath an unsubscribed parent; seed one in Harness.New to exercise remote descendants")
	})

	t.Run("list-framework-recursivematch-discovery", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		if supportsCapability(instance, session, "LIST-EXTENDED") {
			t.Skip("backend evaluates RECURSIVEMATCH itself")
		}
		for _, name := range []string{"recursive", "recursive/child"} {
			if err := session.Create(context.Background(), name, nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := session.Subscribe(context.Background(), "recursive/child", nil); err != nil {
			t.Fatal(err)
		}
		// Only check the harness names. A backend may legitimately hide other
		// namespaces from "*"; it then evaluates RECURSIVEMATCH for those names
		// itself by witnessing LIST-EXTENDED, as ListOptions documents.
		for _, subscribed := range []bool{false, true} {
			options := &imapserver.ListOptions{Subscribed: subscribed}
			all := listMailboxes(t, session, "", []string{"*"}, options)
			for _, query := range []struct {
				reference string
				patterns  []string
			}{
				{patterns: []string{"recursive", "recursive/child"}},
				{reference: "recursive/", patterns: []string{"%"}},
			} {
				listed := listMailboxes(t, session, query.reference, query.patterns, options)
				if listed["recursive/child"] == nil {
					t.Fatalf("LIST %q %q omitted the harness child", query.reference, query.patterns)
				}
				for name := range listed {
					if all[name] == nil {
						t.Errorf(`LIST %q %q returns %q, but LIST "" "*" omits it (Subscribed=%v); witness LIST-EXTENDED to evaluate RECURSIVEMATCH for this namespace`, query.reference, query.patterns, name, subscribed)
					}
				}
			}
		}
	})
}

func listMailboxes(t *testing.T, session imapserver.Session, reference string, patterns []string, options *imapserver.ListOptions) map[string]*imap.ListData {
	t.Helper()
	listed := make(map[string]*imap.ListData)
	err := session.List(context.Background(), &imapserver.ListWriter{WriteFunc: func(_ context.Context, data *imap.ListData) error {
		if data == nil {
			t.Fatal("List wrote nil data")
		}
		listed[data.Mailbox] = data
		return nil
	}}, reference, patterns, options)
	if err != nil {
		t.Fatalf("List %q %q: %v", reference, patterns, err)
	}
	return listed
}

func requireRecursiveParent(t *testing.T, listed map[string]*imap.ListData, name string) *imap.ListData {
	t.Helper()
	parent := listed[name]
	if len(listed) != 1 || parent == nil {
		t.Fatalf("LIST with RECURSIVEMATCH returned %v, want only parent %q", listed, name)
	}
	if !slices.ContainsFunc(parent.ChildInfo, func(option string) bool { return strings.EqualFold(option, "SUBSCRIBED") }) {
		t.Fatalf("parent %q ChildInfo = %q, want SUBSCRIBED", name, parent.ChildInfo)
	}
	return parent
}
