//go:build interop

package imapclient_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

// The group A capabilities, in the order docs/RFC-COVERAGE.md lists them. The
// matrix test below reports which servers advertise which, because a coverage
// row may only move to "verified" on the evidence of two independent servers
// actually exercising the code — not on a profile's claim.
var t08Capabilities = []string{
	"UIDPLUS", "MOVE", "ESEARCH", "SEARCHRES", "LIST-EXTENDED",
	"LIST-STATUS", "SPECIAL-USE", "CREATE-SPECIAL-USE", "CHILDREN", "WITHIN",
}

func TestExtACapabilityMatrix(t *testing.T) {
	for _, server := range harness.RunningServers() {
		caps := harness.CapabilitiesFor(server.Profile.Name)
		harness.AssertProfile(t, server.Profile, caps)
		var present, absent []string
		for _, capability := range t08Capabilities {
			if caps[capability] {
				present = append(present, capability)
			} else {
				absent = append(absent, capability)
			}
		}
		t.Logf("%s advertises %s; falls back for %s", server.Profile.Name,
			strings.Join(present, " "), strings.Join(absent, " "))
	}
}

func TestExtAUIDPlus(t *testing.T) {
	t08ForEachServer(t, func(t *testing.T, ctx context.Context, server *harness.Server, caps map[string]bool, client *imapclient.Client) {
		mailbox := t08Mailbox(t, ctx, server, client, "uidplus")
		message := "From: sender@example.test\r\nTo: interop@example.test\r\nSubject: one\r\n\r\none\r\n"
		appended, err := client.Append(ctx, mailbox, nil, int64(len(message)), strings.NewReader(message)).Wait(ctx)
		if err != nil {
			t08Fail(t, server, client, "APPEND", err)
		}
		t08Append(t, ctx, client, mailbox, "two")
		t08Append(t, ctx, client, mailbox, "three")
		if _, err := client.Select(mailbox, nil).Wait(ctx); err != nil {
			t08Fail(t, server, client, "SELECT", err)
		}
		uids := t08AllUIDs(t, ctx, server, client)
		if len(uids) != 3 {
			t08Fail(t, server, client, fmt.Sprintf("expected 3 UIDs, got %v", uids), nil)
		}
		if caps["UIDPLUS"] {
			if appended == nil || appended.UIDValidity == 0 || appended.UID == 0 {
				t08Fail(t, server, client, fmt.Sprintf("APPENDUID missing: %#v", appended), nil)
			}
			if appended.UID != uids[0] {
				t08Fail(t, server, client, fmt.Sprintf("APPENDUID UID %d != first mailbox UID %d", appended.UID, uids[0]), nil)
			}
			dest := t08Mailbox(t, ctx, server, client, "uidplus-copy")
			var one imap.UIDSet
			one.AddNum(uids[0])
			copied, err := client.CopyUID(one, dest).Wait(ctx)
			if err != nil {
				t08Fail(t, server, client, "UID COPY", err)
			}
			if copied == nil || !copied.Received() || copied.UIDValidity == 0 ||
				!copied.SourceUIDs.Equal(one) || copied.DestinationUIDs.IsEmpty() {
				t08Fail(t, server, client, fmt.Sprintf("COPYUID missing: %#v", copied), nil)
			}
		}

		var all imap.UIDSet
		for _, uid := range uids {
			all.AddNum(uid)
		}
		if err := client.StoreUID(all, []imap.Flag{imap.FlagDeleted}, &imapclient.StoreOptions{
			Op:     imapclient.StoreFlagsAdd,
			Silent: true,
		}).Wait(ctx); err != nil {
			t08Fail(t, server, client, "UID STORE \\Deleted", err)
		}

		var first imap.UIDSet
		first.AddNum(uids[0])
		err = client.UIDExpunge(first, nil).Wait(ctx)
		if !caps["UIDPLUS"] {
			// No emulation exists on purpose: plain EXPUNGE would remove the
			// other two messages as well. See Client.UIDExpunge.
			if !errors.Is(err, imapclient.ErrCapabilityNotAdvertised) {
				t08Fail(t, server, client, "UID EXPUNGE without UIDPLUS must be refused locally", err)
			}
			t.Log("UIDPLUS absent: UID EXPUNGE refused without writing to the connection")
			return
		}
		if err != nil {
			t08Fail(t, server, client, "UID EXPUNGE", err)
		}
		remaining := t08AllUIDs(t, ctx, server, client)
		if len(remaining) != 2 || remaining[0] != uids[1] || remaining[1] != uids[2] {
			t08Fail(t, server, client, fmt.Sprintf("UID EXPUNGE removed %v, want only %d", uids, uids[0]), nil)
		}
	})
}

func TestExtAMove(t *testing.T) {
	t08ForEachServer(t, func(t *testing.T, ctx context.Context, server *harness.Server, caps map[string]bool, client *imapclient.Client) {
		source := t08Mailbox(t, ctx, server, client, "move-source")
		destination := t08Mailbox(t, ctx, server, client, "move-destination")
		t08Append(t, ctx, client, source, "moved")
		if _, err := client.Select(source, nil).Wait(ctx); err != nil {
			t08Fail(t, server, client, "SELECT source", err)
		}
		uids := t08AllUIDs(t, ctx, server, client)
		if len(uids) != 1 {
			t08Fail(t, server, client, fmt.Sprintf("expected 1 UID before the move, got %v", uids), nil)
		}
		var set imap.UIDSet
		set.AddNum(uids[0])

		var options *imapclient.MoveOptions
		if !caps["MOVE"] {
			// The emulation is not atomic, so it is opt-in. Without the opt-in
			// nothing may reach the wire.
			if _, err := client.MoveUID(ctx, set, destination, nil); !errors.Is(err, imapclient.ErrCapabilityNotAdvertised) {
				t08Fail(t, server, client, "UID MOVE without MOVE and without the opt-in must be refused locally", err)
			}
			options = &imapclient.MoveOptions{AllowNonAtomicFallback: true}
		}
		data, err := client.MoveUID(ctx, set, destination, options)
		if err != nil {
			t08Fail(t, server, client, "UID MOVE", err)
		}
		if data.Emulated != !caps["MOVE"] {
			t08Fail(t, server, client, fmt.Sprintf("MOVE emulated=%t with MOVE advertised=%t", data.Emulated, caps["MOVE"]), nil)
		}
		if data.Emulated {
			t.Logf("MOVE absent: emulated with COPY + STORE \\Deleted + %s",
				map[bool]string{true: "UID EXPUNGE", false: "EXPUNGE (every \\Deleted message)"}[!data.ExpungedEveryDeletedMessage])
		} else if data.UIDPlus.Received() {
			t.Logf("MOVE returned COPYUID %d %s -> %s", data.UIDPlus.UIDValidity, data.UIDPlus.SourceUIDs, data.UIDPlus.DestinationUIDs)
		}

		if left := t08AllUIDs(t, ctx, server, client); len(left) != 0 {
			t08Fail(t, server, client, fmt.Sprintf("the move left %v in the source mailbox", left), nil)
		}
		if _, err := client.Select(destination, nil).Wait(ctx); err != nil {
			t08Fail(t, server, client, "SELECT destination", err)
		}
		if arrived := t08AllUIDs(t, ctx, server, client); len(arrived) != 1 {
			t08Fail(t, server, client, fmt.Sprintf("the destination holds %v, want one message", arrived), nil)
		}
	})
}

func TestExtAESearch(t *testing.T) {
	t08ForEachServer(t, func(t *testing.T, ctx context.Context, server *harness.Server, caps map[string]bool, client *imapclient.Client) {
		mailbox := t08Mailbox(t, ctx, server, client, "esearch")
		for _, subject := range []string{"one", "two", "three"} {
			t08Append(t, ctx, client, mailbox, subject)
		}
		if _, err := client.Select(mailbox, nil).Wait(ctx); err != nil {
			t08Fail(t, server, client, "SELECT", err)
		}

		data, err := client.SearchExtended(imap.SearchAll, &imapclient.ESearchOptions{
			ReturnOptions: []imapclient.SearchReturnOption{
				imapclient.SearchReturnMin,
				imapclient.SearchReturnMax,
				imapclient.SearchReturnCount,
			},
		}).Wait(ctx)
		if err != nil {
			t08Fail(t, server, client, "SEARCH RETURN (MIN MAX COUNT)", err)
		}
		if data.Emulated != !caps["ESEARCH"] {
			t08Fail(t, server, client, fmt.Sprintf("ESEARCH emulated=%t with ESEARCH advertised=%t", data.Emulated, caps["ESEARCH"]), nil)
		}
		if !data.HasMin || !data.HasMax || !data.HasCount {
			t08Fail(t, server, client, fmt.Sprintf("incomplete ESEARCH data %#v", data), nil)
		}
		if data.Count != 3 || data.Min != 1 || data.Max != 3 {
			t08Fail(t, server, client, fmt.Sprintf("MIN=%d MAX=%d COUNT=%d, want 1/3/3", data.Min, data.Max, data.Count), nil)
		}
		if data.Emulated {
			t.Log("ESEARCH absent: MIN, MAX and COUNT computed client-side from a plain SEARCH")
		}

		// An empty result must omit MIN and MAX while still reporting COUNT 0.
		empty, err := client.SearchExtended(imap.SearchHeaderField{Field: "Subject", Value: "no such subject"}, &imapclient.ESearchOptions{
			ReturnOptions: []imapclient.SearchReturnOption{
				imapclient.SearchReturnMin,
				imapclient.SearchReturnCount,
			},
		}).Wait(ctx)
		if err != nil {
			t08Fail(t, server, client, "SEARCH for nothing", err)
		}
		if empty.HasMin || !empty.HasCount || empty.Count != 0 {
			t08Fail(t, server, client, fmt.Sprintf("empty ESEARCH data %#v", empty), nil)
		}
	})
}

func TestExtASearchRes(t *testing.T) {
	t08ForEachServer(t, func(t *testing.T, ctx context.Context, server *harness.Server, caps map[string]bool, client *imapclient.Client) {
		if !caps["SEARCHRES"] {
			// "$" is server-side state; no client-side computation creates it.
			mailbox := t08Mailbox(t, ctx, server, client, "searchres-absent")
			t08Append(t, ctx, client, mailbox, "saved")
			if _, err := client.Select(mailbox, nil).Wait(ctx); err != nil {
				t08Fail(t, server, client, "SELECT", err)
			}
			_, err := client.SearchExtendedUID(imap.SearchAll, &imapclient.ESearchOptions{
				ReturnOptions: []imapclient.SearchReturnOption{imapclient.SearchReturnSave},
			}).Wait(ctx)
			if !errors.Is(err, imapclient.ErrCapabilityNotAdvertised) {
				t08Fail(t, server, client, "RETURN (SAVE) without SEARCHRES must be refused locally", err)
			}
			t.Skip("server does not advertise SEARCHRES")
		}
		if !caps["MOVE"] && !caps["UIDPLUS"] {
			t.Skip("no command on this server consumes the saved result")
		}

		source := t08Mailbox(t, ctx, server, client, "searchres-source")
		destination := t08Mailbox(t, ctx, server, client, "searchres-destination")
		t08Append(t, ctx, client, source, "saved one")
		t08Append(t, ctx, client, source, "saved two")
		if _, err := client.Select(source, nil).Wait(ctx); err != nil {
			t08Fail(t, server, client, "SELECT", err)
		}

		command := client.SearchExtendedUID(imap.SearchAll, &imapclient.ESearchOptions{
			ReturnOptions: []imapclient.SearchReturnOption{imapclient.SearchReturnSave},
		})
		if _, err := command.Wait(ctx); err != nil {
			t08Fail(t, server, client, "UID SEARCH RETURN (SAVE)", err)
		}
		saved := command.SavedResult()
		if saved == nil || !saved.Valid() {
			t08Fail(t, server, client, "RETURN (SAVE) produced no usable saved result", nil)
		}

		if caps["MOVE"] {
			if _, err := client.MoveUID(ctx, nil, destination, &imapclient.MoveOptions{SavedSearchResult: saved}); err != nil {
				t08Fail(t, server, client, "UID MOVE $", err)
			}
			if left := t08AllUIDs(t, ctx, server, client); len(left) != 0 {
				t08Fail(t, server, client, fmt.Sprintf("UID MOVE $ left %v behind", left), nil)
			}
			return
		}

		if err := client.StoreUID(imap.UIDSetRange(1, 0), []imap.Flag{imap.FlagDeleted}, &imapclient.StoreOptions{
			Op:     imapclient.StoreFlagsAdd,
			Silent: true,
		}).Wait(ctx); err != nil {
			t08Fail(t, server, client, "UID STORE \\Deleted", err)
		}
		if err := client.UIDExpunge(nil, &imapclient.UIDExpungeOptions{SavedSearchResult: saved}).Wait(ctx); err != nil {
			t08Fail(t, server, client, "UID EXPUNGE $", err)
		}
		if left := t08AllUIDs(t, ctx, server, client); len(left) != 0 {
			t08Fail(t, server, client, fmt.Sprintf("UID EXPUNGE $ left %v behind", left), nil)
		}
	})
}

func TestExtAListExtendedAndChildren(t *testing.T) {
	t08ForEachServer(t, func(t *testing.T, ctx context.Context, server *harness.Server, caps map[string]bool, client *imapclient.Client) {
		parent := t08Mailbox(t, ctx, server, client, "listext")
		delimiter := t08Delimiter(t, ctx, server, client, parent)
		if delimiter == 0 {
			t.Skip("server reports no hierarchy delimiter, so it has no child mailboxes")
		}
		child := parent + string(delimiter) + "child"
		if err := client.Create(child).Wait(ctx); err != nil {
			t08Fail(t, server, client, "CREATE child mailbox", err)
		}
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = client.Delete(child).Wait(cleanupCtx)
		})

		options := &imapclient.ListOptions{Patterns: []string{parent + string(delimiter) + "%"}}
		if caps["LIST-EXTENDED"] {
			options.ReturnOptions = []imapclient.ListReturnOption{imapclient.ListReturnChildren}
		}
		data, err := client.ListMailboxes(ctx, "", parent, options)
		if err != nil {
			t08Fail(t, server, client, "LIST with several patterns", err)
		}
		if !caps["LIST-EXTENDED"] {
			t.Log("LIST-EXTENDED absent: emulated with one plain LIST per pattern")
		}
		names := t08Names(data)
		if !t08Contains(names, parent) || !t08Contains(names, child) {
			t08Fail(t, server, client, fmt.Sprintf("LIST returned %v, want both %q and %q", names, parent, child), nil)
		}

		if !caps["CHILDREN"] {
			t.Log("CHILDREN absent: \\HasChildren not asserted")
			return
		}
		for _, item := range data {
			if item.Mailbox != parent {
				continue
			}
			has, known := imapclient.HasChildren(item.Attrs)
			if !known || !has {
				t08Fail(t, server, client, fmt.Sprintf("CHILDREN attributes on %q = %v", parent, item.Attrs), nil)
			}
		}
	})
}

func TestExtAListStatus(t *testing.T) {
	t08ForEachServer(t, func(t *testing.T, ctx context.Context, server *harness.Server, caps map[string]bool, client *imapclient.Client) {
		mailbox := t08Mailbox(t, ctx, server, client, "liststatus")
		t08Append(t, ctx, client, mailbox, "one")
		t08Append(t, ctx, client, mailbox, "two")

		var statuses []*imapclient.StatusData
		data, err := client.ListMailboxes(ctx, "", mailbox, &imapclient.ListOptions{
			ReturnOptions: []imapclient.ListReturnOption{&imapclient.ListReturnStatus{
				Items: []imap.StatusItem{
					imap.StatusItemMessages,
					imap.StatusItemUIDNext,
					imap.StatusItemUIDValidity,
					imap.StatusItemUnseen,
				},
				Handler: func(status *imapclient.StatusData) { statuses = append(statuses, status) },
			}},
		})
		if err != nil {
			t08Fail(t, server, client, "LIST RETURN (STATUS)", err)
		}
		if names := t08Names(data); !t08Contains(names, mailbox) {
			t08Fail(t, server, client, fmt.Sprintf("LIST returned %v, want %q", names, mailbox), nil)
		}
		if !caps["LIST-STATUS"] {
			t.Log("LIST-STATUS absent: emulated with one STATUS per selectable mailbox")
		}

		var found *imapclient.StatusData
		for _, status := range statuses {
			if status.Mailbox == mailbox {
				found = status
			}
		}
		if found == nil {
			t08Fail(t, server, client, fmt.Sprintf("no STATUS for %q among %d responses", mailbox, len(statuses)), nil)
		}
		if found.NumMessages != 2 || found.UIDValidity == 0 || found.UIDNext == 0 {
			t08Fail(t, server, client, fmt.Sprintf("STATUS for %q = %#v", mailbox, found), nil)
		}
	})
}

func TestExtASpecialUse(t *testing.T) {
	t08ForEachServer(t, func(t *testing.T, ctx context.Context, server *harness.Server, caps map[string]bool, client *imapclient.Client) {
		switch {
		case caps["SPECIAL-USE"] || caps["XLIST"]:
			data, err := client.SpecialUse(ctx, nil)
			if err != nil {
				t08Fail(t, server, client, "SPECIAL-USE lookup", err)
			}
			want := imapclient.SpecialUseSourceServer
			if !caps["SPECIAL-USE"] {
				want = imapclient.SpecialUseSourceXList
			}
			if data.Source != want || data.Guessed() {
				t08Fail(t, server, client, fmt.Sprintf("special-use source = %q guessed=%t", data.Source, data.Guessed()), nil)
			}
			t.Logf("special use from %s: %s", data.Source, t08SpecialUseSummary(data))

		default:
			// Without a server-side answer the result is a guess, and a guess
			// the caller did not ask for is indistinguishable from a fact.
			if _, err := client.SpecialUse(ctx, nil); !errors.Is(err, imapclient.ErrCapabilityNotAdvertised) {
				t08Fail(t, server, client, "a special-use guess was returned without the opt-in", err)
			}
			data, err := client.SpecialUse(ctx, &imapclient.SpecialUseOptions{AllowNameHeuristic: true})
			if err != nil {
				t08Fail(t, server, client, "special-use name heuristic", err)
			}
			if !data.Guessed() {
				t08Fail(t, server, client, "the name heuristic did not mark its result as a guess", nil)
			}
			// The seeded fixture mailboxes are named Sent and Archive.
			if _, ok := data.Mailbox(imap.MailboxAttrSent); !ok {
				t08Fail(t, server, client, fmt.Sprintf("the name heuristic found no \\Sent: %s", t08SpecialUseSummary(data)), nil)
			}
			t.Logf("SPECIAL-USE and XLIST absent: guessed %s", t08SpecialUseSummary(data))
		}
	})
}

func TestExtACreateSpecialUse(t *testing.T) {
	t08ForEachServer(t, func(t *testing.T, ctx context.Context, server *harness.Server, caps map[string]bool, client *imapclient.Client) {
		mailbox := server.Profile.MailboxPrefix + harness.UniqueMailbox("t08-create-special-use")
		if !caps["CREATE-SPECIAL-USE"] {
			err := client.CreateMailbox(mailbox, &imapclient.CreateOptions{
				SpecialUse: []imap.MailboxAttr{imap.MailboxAttrArchive},
			}).Wait(ctx)
			if !errors.Is(err, imapclient.ErrCapabilityNotAdvertised) {
				t08Fail(t, server, client, "CREATE with USE must be refused locally without CREATE-SPECIAL-USE", err)
			}
			t.Skip("server does not advertise CREATE-SPECIAL-USE")
		}

		if err := client.CreateMailbox(mailbox, &imapclient.CreateOptions{
			SpecialUse: []imap.MailboxAttr{imap.MailboxAttrArchive},
		}).Wait(ctx); err != nil {
			t08Fail(t, server, client, "CREATE ... USE (\\Archive)", err)
		}
		t.Cleanup(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = client.Delete(mailbox).Wait(cleanupCtx)
		})

		if !caps["SPECIAL-USE"] {
			t.Log("CREATE-SPECIAL-USE without SPECIAL-USE: the attribute cannot be read back")
			return
		}
		// The servers disagree about where the attribute becomes visible.
		// RFC 6154 section 6 adds the use attributes to mbx-list-oflag, which
		// reads as "any LIST response", and Cyrus 3.x does report \Archive on a
		// plain LIST. Stalwart 0.11.8 reports it only when the LIST asks with
		// RETURN (SPECIAL-USE). Asking explicitly satisfies both.
		options := &imapclient.ListOptions{}
		if caps["LIST-EXTENDED"] {
			options.ReturnOptions = []imapclient.ListReturnOption{imapclient.ListReturnSpecialUse}
		}
		data, err := client.ListMailboxes(ctx, "", mailbox, options)
		if err != nil {
			t08Fail(t, server, client, "LIST the created mailbox", err)
		}
		if len(data) != 1 || !imap.ContainsAttr(data[0].Attrs, imap.MailboxAttrArchive) {
			t08Fail(t, server, client, fmt.Sprintf("the created mailbox does not carry \\Archive: %v", data), nil)
		}
	})
}

func TestExtAWithin(t *testing.T) {
	t08ForEachServer(t, func(t *testing.T, ctx context.Context, server *harness.Server, caps map[string]bool, client *imapclient.Client) {
		mailbox := t08Mailbox(t, ctx, server, client, "within")
		t08Append(t, ctx, client, mailbox, "recent")
		if _, err := client.Select(mailbox, nil).Wait(ctx); err != nil {
			t08Fail(t, server, client, "SELECT", err)
		}

		younger := imap.SearchWithin{Key: imap.SearchWithinKeyYounger, Seconds: 3600}
		if !caps["WITHIN"] {
			// BEFORE and SINCE compare whole dates, so there is no faithful
			// fallback and the criterion must not reach the wire.
			_, err := client.SearchExtended(younger, nil).Wait(ctx)
			if !errors.Is(err, imapclient.ErrCapabilityNotAdvertised) {
				t08Fail(t, server, client, "OLDER/YOUNGER without WITHIN must be refused locally", err)
			}
			t.Skip("server does not advertise WITHIN")
		}

		count := func(criteria imap.SearchCriteria) uint32 {
			t.Helper()
			data, err := client.SearchExtended(criteria, &imapclient.ESearchOptions{
				ReturnOptions: []imapclient.SearchReturnOption{imapclient.SearchReturnCount},
			}).Wait(ctx)
			if err != nil {
				t08Fail(t, server, client, "SEARCH WITHIN", err)
			}
			return data.Count
		}
		if got := count(younger); got != 1 {
			t08Fail(t, server, client, fmt.Sprintf("YOUNGER 3600 matched %d messages, want 1", got), nil)
		}
		older := imap.SearchWithin{Key: imap.SearchWithinKeyOlder, Seconds: 3600}
		if got := count(older); got != 0 {
			t08Fail(t, server, client, fmt.Sprintf("OLDER 3600 matched %d messages, want 0", got), nil)
		}
	})
}

// t08ForEachServer runs body against every running server in its own subtest,
// with a logged-in client and a bounded context.
func t08ForEachServer(t *testing.T, body func(*testing.T, context.Context, *harness.Server, map[string]bool, *imapclient.Client)) {
	t.Helper()
	servers := harness.RunningServers()
	if len(servers) == 0 {
		t.Skip("no interoperability server is running")
	}
	for _, server := range servers {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			caps := harness.CapabilitiesFor(server.Profile.Name)
			harness.AssertProfile(t, server.Profile, caps)
			body(t, ctx, server, caps, t08Dial(t, ctx, server))
		})
	}
}

func t08Dial(t *testing.T, ctx context.Context, server *harness.Server) *imapclient.Client {
	t.Helper()
	client, err := imapclient.Dial(ctx, server.Address, &imapclient.Options{AllowInsecureAuth: true})
	if err == nil {
		err = client.Login(ctx, authInteropUsername, authInteropPassword)
	}
	if err != nil {
		if client != nil {
			_ = client.Close()
		}
		server.LogDiagnostics(context.Background(), t, nil)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// t08Mailbox creates a uniquely named mailbox in the server's personal
// namespace and arranges for its removal.
func t08Mailbox(t *testing.T, ctx context.Context, server *harness.Server, client *imapclient.Client, name string) string {
	t.Helper()
	mailbox := server.Profile.MailboxPrefix + harness.UniqueMailbox("t08-"+name)
	if err := client.Create(mailbox).Wait(ctx); err != nil {
		t08Fail(t, server, client, "CREATE "+mailbox, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if client.State() == imapclient.StateSelected {
			if client.Capabilities()["UNSELECT"] {
				_ = client.Unselect().Wait(cleanupCtx)
			} else {
				_ = client.CloseMailbox().Wait(cleanupCtx)
			}
		}
		_ = client.Delete(mailbox).Wait(cleanupCtx)
	})
	return mailbox
}

func t08Append(t *testing.T, ctx context.Context, client *imapclient.Client, mailbox, subject string) {
	t.Helper()
	message := "From: sender@example.test\r\nTo: interop@example.test\r\nSubject: " + subject + "\r\n\r\n" + subject + "\r\n"
	if _, err := client.Append(ctx, mailbox, nil, int64(len(message)), strings.NewReader(message)).Wait(ctx); err != nil {
		t.Fatalf("APPEND %q to %q: %v", subject, mailbox, err)
	}
}

func t08AllUIDs(t *testing.T, ctx context.Context, server *harness.Server, client *imapclient.Client) []imap.UID {
	t.Helper()
	uids, err := client.SearchUID(imap.SearchAll, nil).AllUID(ctx)
	if err != nil {
		t08Fail(t, server, client, "UID SEARCH ALL", err)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	return uids
}

// t08Delimiter reports the hierarchy delimiter the server assigns to mailbox.
// It is never assumed: RFC 3501 section 7.2.2 makes it server-defined.
func t08Delimiter(t *testing.T, ctx context.Context, server *harness.Server, client *imapclient.Client, mailbox string) rune {
	t.Helper()
	data, err := client.List("", mailbox, nil).Wait(ctx)
	if err != nil {
		t08Fail(t, server, client, "LIST for the hierarchy delimiter", err)
	}
	for _, item := range data {
		if item.Mailbox == mailbox {
			return item.Delimiter
		}
	}
	t08Fail(t, server, client, "LIST did not return "+mailbox, nil)
	return 0
}

func t08Names(data []*imapclient.ListData) []string {
	names := make([]string, 0, len(data))
	for _, item := range data {
		names = append(names, item.Mailbox)
	}
	return names
}

func t08Contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func t08SpecialUseSummary(data *imapclient.SpecialUseData) string {
	entries := make([]string, 0, len(data.Mailboxes))
	for attr, names := range data.Mailboxes {
		entries = append(entries, fmt.Sprintf("%s=%s", attr, strings.Join(names, ",")))
	}
	sort.Strings(entries)
	if len(entries) == 0 {
		return "(none)"
	}
	return strings.Join(entries, " ")
}

func t08Fail(t *testing.T, server *harness.Server, client *imapclient.Client, operation string, err error) {
	t.Helper()
	server.LogDiagnostics(context.Background(), t, nil)
	if err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
	t.Fatal(operation)
}
