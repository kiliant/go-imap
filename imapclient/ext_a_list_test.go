package imapclient

import (
	"errors"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestHasChildren(t *testing.T) {
	for _, tc := range []struct {
		name       string
		attrs      []imap.MailboxAttr
		wantHas    bool
		wantKnown  bool
		wantReason string
	}{
		{name: "has children", attrs: []imap.MailboxAttr{imap.MailboxAttrHasChildren}, wantHas: true, wantKnown: true},
		{name: "has no children", attrs: []imap.MailboxAttr{imap.MailboxAttrHasNoChildren}, wantKnown: true},
		{name: "case insensitive", attrs: []imap.MailboxAttr{"\\haschildren"}, wantHas: true, wantKnown: true},
		// RFC 3348 section 3 says \HasNoChildren is redundant under
		// \Noinferiors and should be omitted.
		{name: "noinferiors", attrs: []imap.MailboxAttr{imap.MailboxAttrNoInferiors}, wantKnown: true},
		// A server may omit both, and a client must not assume either way.
		{name: "silent", attrs: []imap.MailboxAttr{imap.MailboxAttrUnmarked}},
		{name: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			has, known := HasChildren(tc.attrs)
			if has != tc.wantHas || known != tc.wantKnown {
				t.Fatalf("HasChildren(%v) = (%t, %t), want (%t, %t)", tc.attrs, has, known, tc.wantHas, tc.wantKnown)
			}
		})
	}
}

func TestCreateMailboxSendsUseParameter(t *testing.T) {
	var sent string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 CREATE-SPECIAL-USE] ready", func(s *extAServer) {
		tag, rest := s.command()
		sent = rest
		s.ok(tag)
	})
	err := c.CreateMailbox("MySpecial", &CreateOptions{
		SpecialUse: []imap.MailboxAttr{imap.MailboxAttrDrafts, imap.MailboxAttrSent},
	}).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sent != `CREATE MySpecial (USE (\Drafts \Sent))` {
		t.Fatalf("CREATE wire form = %q", sent)
	}
}

func TestCreateMailboxWithoutOptionsMatchesCreate(t *testing.T) {
	var sent string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1] ready", func(s *extAServer) {
		tag, rest := s.command()
		sent = rest
		s.ok(tag)
	})
	if err := c.CreateMailbox("Plain", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if sent != "CREATE Plain" {
		t.Fatalf("CREATE wire form = %q", sent)
	}
}

func TestCreateMailboxGatesOnCreateSpecialUse(t *testing.T) {
	sawCommand := false
	// SPECIAL-USE alone is not enough: RFC 6154 section 3 makes setting the
	// attributes a separate capability from reporting them.
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 SPECIAL-USE] ready", func(s *extAServer) {
		if tag, _ := s.command(); tag != "" {
			sawCommand = true
			s.ok(tag)
		}
	})
	err := c.CreateMailbox("Drafts", &CreateOptions{SpecialUse: []imap.MailboxAttr{imap.MailboxAttrDrafts}}).Wait(ctx)
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("CreateMailbox without CREATE-SPECIAL-USE error = %v", err)
	}
	if sawCommand {
		t.Fatal("a CREATE reached the wire although CREATE-SPECIAL-USE is absent")
	}
}

func TestCreateMailboxRejectsMalformedAttribute(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 CREATE-SPECIAL-USE] ready", func(s *extAServer) {})
	for _, attr := range []imap.MailboxAttr{"Drafts", "\\Bad Attr", "\\"} {
		err := c.CreateMailbox("X", &CreateOptions{SpecialUse: []imap.MailboxAttr{attr}}).Wait(ctx)
		if err == nil {
			t.Fatalf("CreateMailbox accepted the attribute %q", attr)
		}
	}
}

func TestListMailboxesUsesListExtendedWhenAvailable(t *testing.T) {
	var sent string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 LIST-EXTENDED CHILDREN] ready", func(s *extAServer) {
		tag, rest := s.command()
		sent = rest
		s.reply(`* LIST (\HasChildren) "." "work"`, tag+" OK listed")
	})
	data, err := c.ListMailboxes(ctx, "", "%", &ListOptions{
		Patterns:      []string{"work.%"},
		ReturnOptions: []ListReturnOption{ListReturnChildren},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent != `LIST "" (% work.%) RETURN (CHILDREN)` {
		t.Fatalf("LIST-EXTENDED wire form = %q", sent)
	}
	if len(data) != 1 {
		t.Fatalf("data = %#v", data)
	}
	if has, known := HasChildren(data[0].Attrs); !has || !known {
		t.Fatalf("CHILDREN attributes lost: %#v", data[0].Attrs)
	}
}

func TestListMailboxesEmulatesMultiplePatterns(t *testing.T) {
	var commands []string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1] ready", func(s *extAServer) {
		tag, rest := s.command()
		commands = append(commands, rest)
		s.reply(`* LIST () "." "INBOX"`, `* LIST () "." "work"`, tag+" OK listed")
		tag, rest = s.command()
		commands = append(commands, rest)
		// "work" matches both patterns and must not be duplicated.
		s.reply(`* LIST () "." "work"`, `* LIST () "." "work.reports"`, tag+" OK listed")
	})
	data, err := c.ListMailboxes(ctx, "", "%", &ListOptions{Patterns: []string{"work.%"}})
	if err != nil {
		t.Fatal(err)
	}
	assertCommands(t, commands, []string{`LIST "" %`, `LIST "" work.%`})
	var names []string
	for _, item := range data {
		names = append(names, item.Mailbox)
	}
	assertCommands(t, names, []string{"INBOX", "work", "work.reports"})
}

func TestListMailboxesEmulatesSubscribedWithLsub(t *testing.T) {
	var commands []string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1] ready", func(s *extAServer) {
		tag, rest := s.command()
		commands = append(commands, rest)
		s.reply(`* LSUB () "." "work"`, tag+" OK listed")
	})
	data, err := c.ListMailboxes(ctx, "", "*", &ListOptions{
		SelectionOptions: []ListSelectOption{ListSelectSubscribed},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCommands(t, commands, []string{`LSUB "" *`})
	if len(data) != 1 || !imap.ContainsAttr(data[0].Attrs, imap.MailboxAttrSubscribed) {
		t.Fatalf("LSUB fallback did not report \\Subscribed: %#v", data)
	}
}

func TestListMailboxesRefusesInexpressibleOptions(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1] ready", func(s *extAServer) {
		if tag, _ := s.command(); tag != "" {
			s.ok(tag)
		}
	})
	if _, err := c.ListMailboxes(ctx, "", "*", &ListOptions{
		ReturnOptions: []ListReturnOption{ListReturnChildren},
	}); !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("return options without LIST-EXTENDED error = %v", err)
	}
	if _, err := c.ListMailboxes(ctx, "", "*", &ListOptions{
		SelectionOptions: []ListSelectOption{ListSelectRecursiveMatch},
	}); !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("selection options without LIST-EXTENDED error = %v", err)
	}
	if _, err := c.ListMailboxes(nil, "", "*", nil); err == nil { //nolint:staticcheck // nil ctx is the case under test
		t.Fatal("a nil context was accepted")
	}
}

func TestSpecialUseFromServer(t *testing.T) {
	var sent string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 LIST-EXTENDED SPECIAL-USE] ready", func(s *extAServer) {
		tag, rest := s.command()
		sent = rest
		s.reply(
			`* LIST (\Sent) "/" "Sent Mail"`,
			`* LIST (\Drafts) "/" "Drafts"`,
			`* LIST (\Junk \Flagged) "/" "Spam"`,
			`* LIST (\Vendor-Unknown) "/" "Other"`,
			tag+" OK listed")
	})
	data, err := c.SpecialUse(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sent != `LIST (SPECIAL-USE) "" * RETURN (SPECIAL-USE)` {
		t.Fatalf("SPECIAL-USE wire form = %q", sent)
	}
	if data.Source != SpecialUseSourceServer || data.Guessed() {
		t.Fatalf("source = %q guessed=%t", data.Source, data.Guessed())
	}
	if name, ok := data.Mailbox(imap.MailboxAttrSent); !ok || name != "Sent Mail" {
		t.Fatalf("\\Sent = %q %t", name, ok)
	}
	if name, ok := data.Mailbox(imap.MailboxAttrJunk); !ok || name != "Spam" {
		t.Fatalf("\\Junk = %q %t", name, ok)
	}
	if name, ok := data.Mailbox(imap.MailboxAttrFlagged); !ok || name != "Spam" {
		t.Fatalf("\\Flagged = %q %t", name, ok)
	}
	if _, ok := data.Mailbox("\\Vendor-Unknown"); ok {
		t.Fatal("an attribute outside the special-use set was collected")
	}
	if _, ok := data.Mailbox(imap.MailboxAttrArchive); ok {
		t.Fatal("an absent attribute was reported present")
	}
}

func TestSpecialUseFallsBackToXList(t *testing.T) {
	var sent string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 XLIST] ready", func(s *extAServer) {
		tag, rest := s.command()
		sent = rest
		s.reply(
			`* XLIST (\Inbox \HasNoChildren) "/" "INBOX"`,
			`* XLIST (\Spam) "/" "[Gmail]/Spam"`,
			`* XLIST (\Starred) "/" "[Gmail]/Starred"`,
			`* XLIST (\Trash) "/" "[Gmail]/Bin"`,
			tag+" OK listed")
	})
	data, err := c.SpecialUse(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sent != `XLIST "" *` {
		t.Fatalf("XLIST wire form = %q", sent)
	}
	if data.Source != SpecialUseSourceXList || data.Guessed() {
		t.Fatalf("source = %q", data.Source)
	}
	if name, ok := data.Mailbox(imap.MailboxAttrJunk); !ok || name != "[Gmail]/Spam" {
		t.Fatalf("\\Spam was not translated to \\Junk: %q %t", name, ok)
	}
	if name, ok := data.Mailbox(imap.MailboxAttrFlagged); !ok || name != "[Gmail]/Starred" {
		t.Fatalf("\\Starred was not translated to \\Flagged: %q %t", name, ok)
	}
	if name, ok := data.Mailbox(imap.MailboxAttrTrash); !ok || name != "[Gmail]/Bin" {
		t.Fatalf("\\Trash = %q %t", name, ok)
	}
	// \Inbox has no RFC 6154 equivalent and must not be invented.
	if _, ok := data.Mailbox("\\Inbox"); ok {
		t.Fatal("\\Inbox was collected as a special use")
	}
}

func TestSpecialUseNameHeuristicIsOptInAndMarked(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1] ready", func(s *extAServer) {
		tag, _ := s.command()
		s.reply(
			`* LIST () "/" "INBOX"`,
			`* LIST () "/" "Sent Items"`,
			`* LIST () "/" "Personal/Deleted Items"`,
			`* LIST (\Noselect) "/" "Junk"`,
			`* LIST () "/" "Notes"`,
			tag+" OK listed")
		tag, _ = s.command()
		s.ok(tag)
	})
	if _, err := c.SpecialUse(ctx, nil); !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("SpecialUse without an opt-in error = %v", err)
	}
	data, err := c.SpecialUse(ctx, &SpecialUseOptions{AllowNameHeuristic: true})
	if err != nil {
		t.Fatal(err)
	}
	if !data.Guessed() || data.Source != SpecialUseSourceNameHeuristic {
		t.Fatalf("a name guess was not reported as one: %#v", data)
	}
	if name, ok := data.Mailbox(imap.MailboxAttrSent); !ok || name != "Sent Items" {
		t.Fatalf("guessed \\Sent = %q %t", name, ok)
	}
	// The leaf name is what is matched, not the full path.
	if name, ok := data.Mailbox(imap.MailboxAttrTrash); !ok || name != "Personal/Deleted Items" {
		t.Fatalf("guessed \\Trash = %q %t", name, ok)
	}
	// A \Noselect mailbox cannot hold messages, so it is never a special use.
	if _, ok := data.Mailbox(imap.MailboxAttrJunk); ok {
		t.Fatal("a \\Noselect mailbox was guessed as \\Junk")
	}
	if _, ok := (*SpecialUseData)(nil).Mailbox(imap.MailboxAttrSent); ok {
		t.Fatal("nil SpecialUseData returned a mailbox")
	}
	if (*SpecialUseData)(nil).Guessed() {
		t.Fatal("nil SpecialUseData reported a guess")
	}
}
