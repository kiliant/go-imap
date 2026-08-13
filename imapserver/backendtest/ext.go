package backendtest

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// Conformance for the optional interfaces T23 added.
//
// Every subtest here skips when the backend does not implement the interface it
// covers. That is the whole point of an optional interface: not implementing it
// is a valid choice, and a suite that failed for it would push backend authors
// towards stub implementations that always error — which is worse than an
// honest absence, because the framework would then advertise the capability.
//
// What is *not* optional is that a backend implementing one of these obeys the
// invariant the framework relies on. Those invariants are what this file pins.

func runExtensions(t *testing.T, harness *Harness) {
	t.Run("capability-witness-is-stable", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		witness, ok := backendWitness(instance, session)
		if !ok {
			t.Skip("backend does not implement CapabilitySupport")
		}
		// The framework asks the same question at greeting, after STARTTLS and
		// after authentication. An answer that changed between those would make
		// the advertised capability set depend on when it was sampled.
		for _, name := range []string{"CONDSTORE", "QRESYNC", "SORT", "CHILDREN"} {
			first := witness.SupportsCapability(name)
			if second := witness.SupportsCapability(name); first != second {
				t.Errorf("SupportsCapability(%q) is not stable: %v then %v", name, first, second)
			}
		}
		// An unrecognised token must be declined, not accepted by default. A
		// backend that returns true for anything would have every capability
		// this library knows advertised on its behalf.
		if witness.SupportsCapability("NO-SUCH-CAPABILITY-9999") {
			t.Error("SupportsCapability returned true for an unknown token")
		}
	})

	t.Run("condstore-conditional-store", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		mailbox := populate(t, session, "condstore")
		result := selectMailbox(t, session, mailbox, discardUpdater())
		condStore, ok := result.Mailbox.(imapserver.CondStoreMailbox)
		if !ok {
			t.Skip("backend does not implement CondStoreMailbox")
		}
		uids := result.Snapshot.UIDs
		if len(uids) < 2 {
			t.Fatalf("expected at least two seeded messages, got %d", len(uids))
		}

		// Raise the first message's modification sequence, then attempt a
		// conditional store bounded below it.
		bump, err := condStore.StoreCondStore(context.Background(), discardFetchWriter(),
			imap.UIDSetNum(uids[0]), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}},
			&imapserver.StoreOptions{})
		if err != nil {
			t.Fatalf("StoreCondStore: %v", err)
		}
		if bump == nil || bump.HighestModSeq == 0 {
			t.Fatalf("StoreCondStore reported no modification sequence: %#v", bump)
		}
		conditional, err := condStore.StoreCondStore(context.Background(), discardFetchWriter(),
			imap.UIDSetNum(uids[0]), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagAnswered}},
			&imapserver.StoreOptions{UnchangedSince: 1})
		// A rejection is a successful command, not an error: RFC 7162
		// section 3.1.3. Returning an error here would discard the messages
		// that did change.
		if err != nil {
			t.Fatalf("a rejected conditional store must not error: %v", err)
		}
		if conditional == nil || conditional.Modified.IsEmpty() {
			t.Errorf("conditional store did not report the rejected message: %#v", conditional)
		}
		if err := result.Mailbox.Unselect(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("qresync-vanished-excludes-present-messages", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		mailbox := populate(t, session, "qresync")
		result := selectMailbox(t, session, mailbox, discardUpdater())
		resync, ok := result.Mailbox.(imapserver.QResyncMailbox)
		if !ok {
			t.Skip("backend does not implement QResyncMailbox")
		}
		present := slices.Clone(result.Snapshot.UIDs)
		// Resynchronising from the beginning reports everything that changed,
		// but a message still in the mailbox is not vanished. Reporting one
		// would make a client delete a message it still has.
		report, err := resync.Resync(context.Background(), &imapserver.QResyncSelect{
			UIDValidity: result.Snapshot.Status.UIDValidity,
		})
		if err != nil {
			t.Fatalf("Resync: %v", err)
		}
		if report != nil {
			for _, uid := range present {
				if report.Vanished.Contains(uid) {
					t.Errorf("UID %d is present but was reported vanished", uid)
				}
			}
		}
		if err := result.Mailbox.Unselect(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("replace-is-atomic", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		mailbox := populate(t, session, "replace")
		result := selectMailbox(t, session, mailbox, discardUpdater())
		replacer, ok := result.Mailbox.(imapserver.ReplaceMailbox)
		if !ok {
			t.Skip("backend does not implement ReplaceMailbox")
		}
		before := len(result.Snapshot.UIDs)
		if before == 0 {
			t.Fatal("expected seeded messages")
		}
		replaced := result.Snapshot.UIDs[0]
		data, err := replacer.Replace(context.Background(), replaced, mailbox,
			messageReader("replacement"), &imapserver.ReplaceOptions{})
		if err != nil {
			t.Fatalf("Replace: %v", err)
		}
		if data == nil || !data.HasUID || data.UID == 0 {
			t.Fatalf("Replace reported no new UID: %#v", data)
		}
		if data.UID == replaced {
			t.Error("the replacement reused the replaced UID, so the two are indistinguishable")
		}
		if err := result.Mailbox.Unselect(context.Background()); err != nil {
			t.Fatal(err)
		}
		// One message out, one in: the count is the invariant that separates a
		// replace from an append.
		after := selectMailbox(t, session, mailbox, discardUpdater())
		if got := len(after.Snapshot.UIDs); got != before {
			t.Errorf("message count changed across REPLACE: %d then %d", before, got)
		}
		if slices.Contains(after.Snapshot.UIDs, replaced) {
			t.Errorf("UID %d survived the replace", replaced)
		}
		if !slices.Contains(after.Snapshot.UIDs, data.UID) {
			t.Errorf("the replacement UID %d is not present", data.UID)
		}
		if err := after.Mailbox.Unselect(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("sort-returns-a-permutation", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		mailbox := populate(t, session, "sort")
		result := selectMailbox(t, session, mailbox, discardUpdater())
		sorter, ok := result.Mailbox.(imapserver.SortMailbox)
		if !ok {
			t.Skip("backend does not implement SortMailbox")
		}
		sorted, err := sorter.Sort(context.Background(), nil,
			[]imap.SortKeySpec{{Key: imap.SortKeyArrival}}, &imapserver.SortOptions{})
		if err != nil {
			t.Fatalf("Sort: %v", err)
		}
		// SORT reorders the result; it must not add, drop or duplicate. A
		// duplicate would make the client show one message twice.
		expected := slices.Clone(result.Snapshot.UIDs)
		got := slices.Clone(sorted)
		slices.Sort(expected)
		slices.Sort(got)
		if !slices.Equal(expected, got) {
			t.Errorf("Sort is not a permutation of the mailbox: got %v, want the same set as %v", got, expected)
		}
		if err := result.Mailbox.Unselect(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("thread-reports-each-message-once", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		mailbox := populate(t, session, "thread")
		result := selectMailbox(t, session, mailbox, discardUpdater())
		threader, ok := result.Mailbox.(imapserver.ThreadMailbox)
		if !ok {
			t.Skip("backend does not implement ThreadMailbox")
		}
		roots, err := threader.Thread(context.Background(), nil, imap.ThreadOrderedSubject, &imapserver.ThreadOptions{})
		if err != nil {
			// Refusing an algorithm is allowed; answering it wrongly is not.
			t.Skipf("backend declined ORDEREDSUBJECT: %v", err)
		}
		seen := make(map[uint32]int)
		var walk func(nodes []imap.ThreadNode)
		walk = func(nodes []imap.ThreadNode) {
			for i := range nodes {
				if nodes[i].Num != 0 {
					seen[nodes[i].Num]++
				}
				walk(nodes[i].Children)
			}
		}
		walk(roots)
		for uid, count := range seen {
			if count != 1 {
				t.Errorf("UID %d appears %d times in the thread forest", uid, count)
			}
		}
		if err := result.Mailbox.Unselect(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("quota-roots-are-answerable", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		quota, ok := session.(imapserver.QuotaSession)
		if !ok {
			t.Skip("backend does not implement QuotaSession")
		}
		mailbox := populate(t, session, "quota")
		roots, err := quota.QuotaRoots(context.Background(), mailbox, nil)
		if err != nil {
			t.Fatalf("QuotaRoots: %v", err)
		}
		// Every root named here must be one GetQuota can answer, or the
		// GETQUOTAROOT response points at something the client cannot query.
		for _, root := range roots {
			data, err := quota.GetQuota(context.Background(), root, nil)
			if err != nil {
				t.Errorf("QuotaRoots named %q but GetQuota rejects it: %v", root, err)
				continue
			}
			if data == nil {
				t.Errorf("GetQuota(%q) returned no data", root)
				continue
			}
			if data.Root != root {
				t.Errorf("GetQuota(%q) reported root %q", root, data.Root)
			}
		}
	})

	t.Run("acl-round-trips", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		acl, ok := session.(imapserver.ACLSession)
		if !ok {
			t.Skip("backend does not implement ACLSession")
		}
		mailbox := populate(t, session, "acl")
		setter, canSet := session.(imapserver.ACLSetSession)
		if !canSet {
			t.Skip("backend does not implement ACLSetSession")
		}
		if err := setter.SetACL(context.Background(), mailbox, "backendtest-user", "lr", nil); err != nil {
			t.Fatalf("SetACL: %v", err)
		}
		data, err := acl.GetACL(context.Background(), mailbox, nil)
		if err != nil {
			t.Fatalf("GetACL: %v", err)
		}
		if !hasACLEntry(data, "backendtest-user") {
			t.Fatalf("GetACL does not report the entry just set: %#v", data)
		}
		// Deleting an entry removes it; it does not leave one with no rights.
		// A client distinguishes "no explicit entry" from "explicitly nothing".
		if err := setter.DeleteACL(context.Background(), mailbox, "backendtest-user", nil); err != nil {
			t.Fatalf("DeleteACL: %v", err)
		}
		data, err = acl.GetACL(context.Background(), mailbox, nil)
		if err != nil {
			t.Fatalf("GetACL: %v", err)
		}
		if hasACLEntry(data, "backendtest-user") {
			t.Error("DeleteACL left the entry in place")
		}
	})

	t.Run("metadata-distinguishes-nil-from-empty", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		metadata, ok := session.(imapserver.MetadataSession)
		if !ok {
			t.Skip("backend does not implement MetadataSession")
		}
		mailbox := populate(t, session, "metadata")
		const entry imap.MetadataEntryName = "/private/backendtest"
		empty := ""
		if err := metadata.SetMetadata(context.Background(), mailbox,
			[]imap.MetadataEntry{{Name: entry, Value: &empty}}, nil); err != nil {
			t.Fatalf("SetMetadata: %v", err)
		}
		data, err := metadata.GetMetadata(context.Background(), mailbox, []imap.MetadataEntryName{entry}, nil)
		if err != nil {
			t.Fatalf("GetMetadata: %v", err)
		}
		if !hasMetadataEntry(data, entry) {
			t.Error("an empty value is a present value and must be stored")
		}
		// A nil value removes the entry. RFC 5464 section 4.3 makes removal and
		// blanking different operations, and a backend that conflates them
		// cannot express either.
		if err := metadata.SetMetadata(context.Background(), mailbox,
			[]imap.MetadataEntry{{Name: entry, Value: nil}}, nil); err != nil {
			t.Fatalf("SetMetadata(nil): %v", err)
		}
		data, err = metadata.GetMetadata(context.Background(), mailbox, []imap.MetadataEntryName{entry}, nil)
		if err != nil {
			t.Fatalf("GetMetadata: %v", err)
		}
		if hasMetadataEntry(data, entry) {
			t.Error("a nil value must remove the entry, not blank it")
		}
	})

	t.Run("multisearch-results-carry-their-mailbox", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		searcher, ok := session.(imapserver.MultiSearchSession)
		if !ok {
			t.Skip("backend does not implement MultiSearchSession")
		}
		mailbox := populate(t, session, "multisearch")
		results, err := searcher.MultiSearch(context.Background(), []string{mailbox}, imap.SearchAll, nil)
		if err != nil {
			t.Fatalf("MultiSearch: %v", err)
		}
		for _, result := range results {
			// A UID without its mailbox and UIDVALIDITY is unusable: the client
			// cannot tell which mailbox it belongs to, which is the entire
			// difference between MULTISEARCH and SEARCH.
			if result.Mailbox == "" {
				t.Errorf("result names no mailbox: %#v", result)
			}
			if len(result.UIDs) != 0 && result.UIDValidity == 0 {
				t.Errorf("result for %q carries UIDs with no UIDVALIDITY", result.Mailbox)
			}
		}
	})

	t.Run("scram-credentials-are-derivations-not-passwords", func(t *testing.T) {
		instance := harness.New()
		if instance == nil || instance.Backend == nil {
			t.Fatal("backendtest: factory returned nil instance or backend")
		}
		store, ok := instance.Backend.(imapserver.SCRAMCredentials)
		if !ok {
			t.Skip("backend does not implement SCRAMCredentials")
		}
		stored, err := store.SCRAMCredentials(context.Background(), "SCRAM-SHA-256", instance.Credentials.Username)
		if err != nil {
			t.Fatalf("SCRAMCredentials: %v", err)
		}
		if stored == nil {
			t.Fatal("SCRAMCredentials returned nothing for a valid user")
		}
		if len(stored.Salt) == 0 || len(stored.StoredKey) == 0 || len(stored.ServerKey) == 0 {
			t.Errorf("incomplete derivation: %#v", stored)
		}
		// RFC 7677 section 4 sets the floor. A backend below it is offering
		// SCRAM that is not worth the round trips.
		if stored.Iterations < 4096 {
			t.Errorf("iteration count %d is below the RFC 7677 minimum", stored.Iterations)
		}
		// The derivation must be stable, or every login would need a fresh one
		// and no client could ever authenticate twice.
		again, err := store.SCRAMCredentials(context.Background(), "SCRAM-SHA-256", instance.Credentials.Username)
		if err != nil || again == nil {
			t.Fatalf("second SCRAMCredentials call: %v", err)
		}
		if string(again.StoredKey) != string(stored.StoredKey) {
			t.Error("the stored key changed between calls")
		}
		// An unknown user must not be distinguishable by a different error
		// shape, so this only asserts it does not succeed.
		if unknown, err := store.SCRAMCredentials(context.Background(), "SCRAM-SHA-256", "no-such-user-9999"); err == nil && unknown != nil {
			t.Error("SCRAMCredentials answered for an unknown user")
		}
	})

	t.Run("urlauth-refuses-a-forged-token", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		urlAuth, ok := session.(imapserver.URLAuthSession)
		if !ok {
			t.Skip("backend does not implement URLAuthSession")
		}
		mailbox := populate(t, session, "urlauth")
		url := "imap://" + instance.Credentials.Username + "@example.com/" + mailbox + "/;UID=1"
		authorized, err := urlAuth.GenerateURLAuth(context.Background(), url, "INTERNAL", nil)
		if err != nil {
			t.Fatalf("GenerateURLAuth: %v", err)
		}
		if authorized == url {
			t.Fatal("GenerateURLAuth returned the URL unchanged, so it carries no token")
		}
		// The security property: tampering with the token must not resolve.
		// Everything else about URLAUTH is formatting.
		forged := authorized[:len(authorized)-1] + "x"
		if content, _ := urlAuth.FetchURLAuth(context.Background(), forged, nil); len(content) != 0 {
			t.Error("a forged authorization token was honoured")
		}
		if err := urlAuth.ResetURLAuthKey(context.Background(), "", nil); err != nil {
			t.Fatalf("ResetURLAuthKey: %v", err)
		}
		if content, _ := urlAuth.FetchURLAuth(context.Background(), authorized, nil); len(content) != 0 {
			t.Error("a reset key did not revoke an outstanding URL")
		}
	})

	t.Run("language-reports-what-it-adopts", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		languages, ok := session.(imapserver.LanguageSession)
		if !ok {
			t.Skip("backend does not implement LanguageSession")
		}
		available, err := languages.Languages(context.Background(), nil)
		if err != nil {
			t.Fatalf("Languages: %v", err)
		}
		if len(available) == 0 {
			t.Fatal("a backend implementing LanguageSession must offer at least one language")
		}
		// Every advertised tag must be selectable, or the list is a promise the
		// backend does not keep.
		for _, tag := range available {
			adopted, err := languages.SetLanguage(context.Background(), tag, nil)
			if err != nil || adopted == "" {
				t.Errorf("advertised language %q cannot be selected: %v", tag, err)
			}
		}
		if adopted, _ := languages.SetLanguage(context.Background(), "zz-nonexistent", nil); adopted != "" {
			t.Error("an unavailable language was adopted")
		}
	})

	t.Run("namespace-reports-a-personal-namespace", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		namespaces, ok := session.(imapserver.NamespaceSession)
		if !ok {
			t.Skip("backend does not implement NamespaceSession")
		}
		data, err := namespaces.Namespace(context.Background(), nil)
		if err != nil {
			t.Fatalf("Namespace: %v", err)
		}
		// A session that can select mailboxes has somewhere to select them
		// from, so a personal namespace must exist.
		if data == nil || len(data.Personal) == 0 {
			t.Errorf("no personal namespace reported: %#v", data)
		}
	})
}

func backendWitness(instance *Instance, session imapserver.Session) (imapserver.CapabilitySupport, bool) {
	if witness, ok := session.(imapserver.CapabilitySupport); ok {
		return witness, true
	}
	witness, ok := instance.Backend.(imapserver.CapabilitySupport)
	return witness, ok
}

func hasACLEntry(data *imap.ACLData, identifier string) bool {
	if data == nil {
		return false
	}
	return slices.ContainsFunc(data.Entries, func(entry imap.ACLEntry) bool {
		return entry.Identifier == identifier
	})
}

func hasMetadataEntry(data *imap.MailboxMetadata, name imap.MetadataEntryName) bool {
	if data == nil {
		return false
	}
	return slices.ContainsFunc(data.Entries, func(entry imap.MetadataEntry) bool {
		return entry.Name == name
	})
}

// messageReader builds a minimal well-formed message for the extension
// subtests, matching the shape appendMessage uses.
func messageReader(subject string) io.Reader {
	return strings.NewReader(fmt.Sprintf(
		"From: sender@example.com\r\nTo: receiver@example.com\r\nSubject: %s\r\n\r\nbody %s\r\n",
		subject, subject))
}

func discardUpdater() *imapserver.Updater {
	return &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { return nil }}
}

func discardFetchWriter() *imapserver.FetchWriter {
	return &imapserver.FetchWriter{
		WriteFunc: func(context.Context, *imap.FetchMessageData) error { return nil },
	}
}
