package backendtest

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// Conformance for the optional interfaces T23 added.
//
// Every subtest here skips when the backend does not implement the interface it
// covers, or explicitly filters its capability. Not implementing an optional
// interface is a valid choice, and failing for it would push backend authors
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
			first := witness.SupportsCapability(context.Background(), name, nil)
			if second := witness.SupportsCapability(context.Background(), name, nil); first != second {
				t.Errorf("SupportsCapability(%q) is not stable: %v then %v", name, first, second)
			}
		}
		// An unrecognised token must be declined, not accepted by default. A
		// backend that returns true for anything would have every capability
		// this library knows advertised on its behalf.
		if witness.SupportsCapability(context.Background(), "NO-SUCH-CAPABILITY-9999", nil) {
			t.Error("SupportsCapability returned true for an unknown token")
		}
	})

	t.Run("uidplus-assigned-results", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		if !supportsCapability(instance, session, "UIDPLUS") {
			t.Skip("backend does not witness UIDPLUS; APPEND and COPY results are optional")
		}
		// UIDPLUS is the promise that APPEND and COPY report assigned UIDs,
		// which RFC 4315's APPENDUID and COPYUID are made of.
		mailbox := populate(t, session, "uidplus")
		raw := "Subject: uidplus\r\n\r\nbody\r\n"
		data, err := session.Append(context.Background(), mailbox, strings.NewReader(raw), nil)
		if err != nil || data == nil || !data.HasUID || data.UID == 0 || data.UIDValidity == 0 {
			t.Fatalf("Append data = %#v, %v", data, err)
		}
		destination := mailbox + "-copy"
		if err := session.Create(context.Background(), destination, nil); err != nil {
			t.Fatal(err)
		}
		result := selectMailbox(t, session, mailbox, discardUpdater())
		defer result.Mailbox.Unselect(context.Background(), nil)
		// The assigned UID names the appended message, the last one.
		if uids := result.Snapshot.UIDs; data.UIDValidity != result.Snapshot.Status.UIDValidity || len(uids) == 0 || uids[len(uids)-1] != data.UID {
			t.Fatalf("Append data = %#v, but the mailbox has UIDVALIDITY %d and UIDs %v", data, result.Snapshot.Status.UIDValidity, uids)
		}
		copied, err := result.Mailbox.Copy(context.Background(), imap.UIDSetNum(data.UID), destination, nil)
		if err != nil || copied == nil || !copied.HasUIDs || !copied.SourceUIDs.Contains(data.UID) || copied.DestinationUIDs.IsEmpty() {
			t.Fatalf("Copy data = %#v, %v", copied, err)
		}
		checkReportedCopy(t, instance, "COPY", copied, []imap.UID{data.UID}, destination)
	})

	t.Run("condstore-conditional-store", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireWitness(t, instance, session, "CONDSTORE")
		mailbox := populate(t, session, "condstore")
		result := selectMailbox(t, session, mailbox, discardUpdater())
		condStore, ok := result.Mailbox.(imapserver.CondStoreMailbox)
		if !ok {
			t.Fatal("CONDSTORE is witnessed but the selected mailbox does not implement CondStoreMailbox")
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
		// HighestModSeq is optional (zero when unreported), so only the result
		// itself is required here.
		if bump == nil {
			t.Fatal("StoreCondStore returned no result")
		}
		conditional, err := condStore.StoreCondStore(context.Background(), discardFetchWriter(),
			imap.UIDSetNum(uids[0]), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagAnswered}},
			&imapserver.StoreOptions{UnchangedSince: 1, HasUnchangedSince: true})
		// A rejection is a successful command, not an error: RFC 7162
		// section 3.1.3. Returning an error here would discard the messages
		// that did change.
		if err != nil {
			t.Fatalf("a rejected conditional store must not error: %v", err)
		}
		if conditional == nil || conditional.Modified.IsEmpty() {
			t.Errorf("conditional store did not report the rejected message: %#v", conditional)
		}
		if err := result.Mailbox.Unselect(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("qresync-vanished-excludes-present-messages", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireWitness(t, instance, session, "QRESYNC")
		requireAllowedCapabilities(t, instance, session, "CONDSTORE")
		mailbox := populate(t, session, "qresync")
		result := selectMailbox(t, session, mailbox, discardUpdater())
		resync, ok := result.Mailbox.(imapserver.QResyncMailbox)
		if !ok {
			t.Fatal("QRESYNC is witnessed but the selected mailbox does not implement QResyncMailbox")
		}
		present := slices.Clone(result.Snapshot.UIDs)
		// Resynchronising from the beginning reports everything that changed,
		// but a message still in the mailbox is not vanished. Reporting one
		// would make a client delete a message it still has.
		report, err := resync.Resync(context.Background(), &imapserver.QResyncSelect{
			UIDValidity: result.Snapshot.Status.UIDValidity,
		}, nil)
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
		if err := result.Mailbox.Unselect(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("replace-is-atomic", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireWitness(t, instance, session, "REPLACE")
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
		// The new UID is required only from a UIDPLUS witness, as for APPEND.
		reported := data != nil && data.HasUID
		if supportsCapability(instance, session, "UIDPLUS") && (!reported || data.UID == 0) {
			t.Fatalf("Replace reported no new UID: %#v", data)
		}
		if reported && data.UID == replaced {
			t.Error("the replacement reused the replaced UID, so the two are indistinguishable")
		}
		if err := result.Mailbox.Unselect(context.Background(), nil); err != nil {
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
		if reported && !slices.Contains(after.Snapshot.UIDs, data.UID) {
			t.Errorf("the replacement UID %d is not present", data.UID)
		}
		if err := after.Mailbox.Unselect(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("sort-returns-a-permutation", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireWitness(t, instance, session, "SORT")
		mailbox := populate(t, session, "sort")
		result := selectMailbox(t, session, mailbox, discardUpdater())
		sorter, ok := result.Mailbox.(imapserver.SortMailbox)
		if !ok {
			t.Skip("backend does not implement SortMailbox")
		}
		sorted, err := sorter.Sort(context.Background(), searchQuery(t, imap.SearchAll),
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
		if err := result.Mailbox.Unselect(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("thread-reports-each-message-once", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireWitness(t, instance, session, "THREAD=ORDEREDSUBJECT")
		mailbox := populate(t, session, "thread")
		result := selectMailbox(t, session, mailbox, discardUpdater())
		threader, ok := result.Mailbox.(imapserver.ThreadMailbox)
		if !ok {
			t.Skip("backend does not implement ThreadMailbox")
		}
		roots, err := threader.Thread(context.Background(), searchQuery(t, imap.SearchAll), imap.ThreadOrderedSubject, &imapserver.ThreadOptions{})
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
		if err := result.Mailbox.Unselect(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("quota-roots-are-answerable", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireAllowedCapabilities(t, instance, session, "QUOTA")
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
		requireAllowedCapabilities(t, instance, session, "ACL")
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
		mailbox := ""
		if capabilityAllowed(instance, session, "METADATA") {
			mailbox = populate(t, session, "metadata")
		} else {
			requireAllowedCapabilities(t, instance, session, "METADATA-SERVER")
		}
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
		requireAllowedCapabilities(t, instance, session, "MULTISEARCH")
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
		instance, err := harness.New(t.Context(), nil)
		if err != nil {
			t.Fatalf("backendtest: harness could not construct an instance: %v", err)
		}
		if instance == nil {
			t.Fatal("backendtest: harness returned no instance and no error")
		}
		if instance == nil || instance.Backend == nil {
			t.Fatal("backendtest: factory returned nil instance or backend")
		}
		store, ok := instance.Backend.(imapserver.SCRAMCredentials)
		if !ok {
			t.Skip("backend does not implement SCRAMCredentials")
		}
		// Each mechanism family is a separate witness, so only one the backend
		// witnesses (bound or unbound) and does not filter is checked.
		mechanism := ""
		for _, family := range []string{"SCRAM-SHA-256", "SCRAM-SHA-1"} {
			for _, token := range []string{"AUTH=" + family, "AUTH=" + family + "-PLUS"} {
				if mechanism == "" && supportsCapability(instance, nil, token) {
					mechanism = family
				}
			}
		}
		if mechanism == "" {
			t.Skip("backend witnesses no SCRAM mechanism")
		}
		stored, err := store.SCRAMCredentials(context.Background(), mechanism, instance.Credentials.Username, nil)
		if err != nil {
			t.Fatalf("SCRAMCredentials: %v", err)
		}
		if stored == nil {
			t.Fatal("SCRAMCredentials returned nothing for a valid user")
		}
		if len(stored.Salt) == 0 || len(stored.StoredKey) == 0 || len(stored.ServerKey) == 0 {
			t.Errorf("incomplete derivation: %#v", stored)
		}
		// RFC 7677 section 4 (RFC 5802 for SHA-1) sets the floor. A backend below it is offering
		// SCRAM that is not worth the round trips.
		if stored.Iterations < 4096 {
			t.Errorf("iteration count %d is below the RFC 7677 minimum", stored.Iterations)
		}
		// The derivation must be stable, or every login would need a fresh one
		// and no client could ever authenticate twice.
		again, err := store.SCRAMCredentials(context.Background(), mechanism, instance.Credentials.Username, nil)
		if err != nil || again == nil {
			t.Fatalf("second SCRAMCredentials call: %v", err)
		}
		if string(again.StoredKey) != string(stored.StoredKey) {
			t.Error("the stored key changed between calls")
		}
		// An unknown user must not be distinguishable by a different error
		// shape, so this only asserts it does not succeed.
		if unknown, err := store.SCRAMCredentials(context.Background(), mechanism, "no-such-user-9999", nil); err == nil && unknown != nil {
			t.Error("SCRAMCredentials answered for an unknown user")
		}
	})

	t.Run("urlauth-refuses-a-forged-token", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireAllowedCapabilities(t, instance, session, "URLAUTH")
		urlAuth, ok := session.(imapserver.URLAuthSession)
		if !ok {
			t.Skip("backend does not implement URLAuthSession")
		}
		mailbox := populate(t, session, "urlauth")
		// UIDs are the backend's to assign; take one from the mailbox.
		fixture := selectMailbox(t, session, mailbox, discardUpdater())
		uid := fixture.Snapshot.UIDs[0]
		if err := fixture.Mailbox.Unselect(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		url := "imap://" + url.User(instance.Credentials.Username).String() + "@example.com/" + url.PathEscape(mailbox) + fmt.Sprintf("/;UID=%d;URLAUTH=anonymous", uid)
		authorized, err := urlAuth.GenerateURLAuth(context.Background(), url, "INTERNAL", nil)
		if err != nil {
			t.Fatalf("GenerateURLAuth: %v", err)
		}
		if authorized == url {
			t.Fatal("GenerateURLAuth returned the URL unchanged, so it carries no token")
		}
		content, err := urlAuth.FetchURLAuth(context.Background(), authorized, nil)
		if err != nil || content == nil || content.Body == nil {
			if content != nil && content.Body != nil {
				_ = content.Body.Close()
			}
			t.Fatalf("valid URLFETCH returned no body: %#v, %v", content, err)
		}
		raw, readErr := io.ReadAll(content.Body)
		closeErr := content.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("valid URLFETCH body failed: %v, %v", readErr, closeErr)
		}
		if content.HasSize && (content.Size < 0 || content.Size != int64(len(raw))) {
			t.Fatalf("URLFETCH known size %d differs from %d bytes", content.Size, len(raw))
		}
		// The security property: tampering with the token must not resolve.
		// URL shape, owner/access restrictions and expiry need separate checks.
		forged := authorized[:len(authorized)-1] + "x"
		if content, _ := urlAuth.FetchURLAuth(context.Background(), forged, nil); content != nil {
			if content.Body != nil {
				_ = content.Body.Close()
			}
			t.Error("a forged authorization token was honoured")
		}
		if err := urlAuth.ResetURLAuthKey(context.Background(), "", nil); err != nil {
			t.Fatalf("ResetURLAuthKey: %v", err)
		}
		if content, _ := urlAuth.FetchURLAuth(context.Background(), authorized, nil); content != nil {
			if content.Body != nil {
				_ = content.Body.Close()
			}
			t.Error("a reset key did not revoke an outstanding URL")
		}
	})

	t.Run("language-reports-what-it-adopts", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireAllowedCapabilities(t, instance, session, "LANGUAGE")
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
			if err != nil || adopted == nil || adopted.Tag == "" {
				t.Errorf("advertised language %q cannot be selected: %v", tag, err)
			}
		}
		if adopted, _ := languages.SetLanguage(context.Background(), "zz-nonexistent", nil); adopted != nil && adopted.Tag != "" {
			t.Error("an unavailable language was adopted")
		}
	})

	t.Run("namespace-reports-a-personal-namespace", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireAllowedCapabilities(t, instance, session, "NAMESPACE")
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

	t.Run("quota-set-is-readable-afterwards", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireAllowedCapabilities(t, instance, session, "QUOTASET", "QUOTA")
		setter, ok := session.(imapserver.QuotaSetSession)
		if !ok {
			t.Skip("backend does not implement QuotaSetSession")
		}
		reader, ok := session.(imapserver.QuotaSession)
		if !ok {
			t.Skip("backend implements QuotaSetSession without QuotaSession")
		}
		mailbox := populate(t, session, "quotaset")
		roots, err := reader.QuotaRoots(context.Background(), mailbox, nil)
		if err != nil || len(roots) == 0 {
			t.Skipf("no quota root to write to: %v", err)
		}
		root := roots[0]
		before, err := reader.GetQuota(context.Background(), root, nil)
		if err != nil || before == nil {
			t.Fatalf("GetQuota(%q): %v", root, err)
		}
		if len(before.Resources) == 0 {
			t.Skip("quota root reports no resources to set")
		}
		// SETQUOTA's whole contract is that the limit it accepted is the limit
		// GETQUOTA subsequently reports. A backend that accepts the write and
		// keeps the old value leaves the client believing it changed something.
		resource := before.Resources[0]
		want := resource.Limit + 512
		limits := []imap.QuotaResourceLimit{{Name: resource.Name, Limit: want}}
		if err := setter.SetQuota(context.Background(), root, limits, nil); err != nil {
			t.Skipf("backend declined SetQuota, which is a valid policy: %v", err)
		}
		after, err := reader.GetQuota(context.Background(), root, nil)
		if err != nil || after == nil {
			t.Fatalf("GetQuota after SetQuota: %v", err)
		}
		for _, got := range after.Resources {
			if got.Name != resource.Name {
				continue
			}
			if got.Limit != want {
				t.Errorf("SetQuota(%q, %d) succeeded but GetQuota reports %d",
					resource.Name, want, got.Limit)
			}
			return
		}
		t.Errorf("resource %q disappeared from the quota root after SetQuota", resource.Name)
	})

	t.Run("unauthenticate-returns-to-a-usable-state", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireAllowedCapabilities(t, instance, session, "UNAUTHENTICATE")
		unauth, ok := session.(imapserver.UnauthenticateSession)
		if !ok {
			t.Skip("backend does not implement UnauthenticateSession")
		}
		populate(t, session, "unauth")
		if err := unauth.Unauthenticate(context.Background(), nil); err != nil {
			t.Fatalf("Unauthenticate: %v", err)
		}
		// The framework abandons any selection, calls Unauthenticate, then closes
		// the session and drops it — see handleUnauthenticate. So a backend does
		// not have to scrub the previous identity's data itself: the object never
		// serves another command, and re-authenticating builds a fresh session.
		//
		// What it must not do is leave the session unclosable. Unauthenticate
		// releasing something Close still needs would turn every UNAUTHENTICATE
		// into a failed command *after* the point of no return, where the
		// framework has already abandoned the selection.
		//
		// The deferred closeSession is that assertion. Calling Close here as well
		// would require it to be idempotent, and nothing promises that — the
		// framework calls it exactly once, then drops the session. A backend that
		// releases its handle and errors on a second Close is correct against the
		// documented interface, and an earlier draft of this subtest would have
		// failed it.
	})

	t.Run("catenate-url-resolves-or-is-refused", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireAllowedCapabilities(t, instance, session, "CATENATE")
		catenate, ok := session.(imapserver.CatenateSession)
		if !ok {
			t.Skip("backend does not implement CatenateSession")
		}
		mailbox := populate(t, session, "catenate")
		result := selectMailbox(t, session, mailbox, discardUpdater())
		if len(result.Snapshot.UIDs) == 0 {
			t.Skip("no message to build a URL for")
		}
		url := fmt.Sprintf("imap://localhost/%s/;UID=%d", mailbox, result.Snapshot.UIDs[0])
		reader, err := catenate.ResolveCatenateURL(context.Background(), url, nil)
		if err != nil || reader == nil {
			// Refusing a URL is valid — the backend decides what it can
			// resolve, and this one names a made-up authority. A nil reader is
			// the documented unresolved result, which fails the APPEND with
			// BADURL exactly as an error does.
			t.Skipf("backend did not resolve the fixture URL: %v", err)
		}
		defer reader.Close()
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("reading resolved CATENATE URL: %v", err)
		}
		if len(content) == 0 {
			t.Error("a resolved CATENATE URL yielded no bytes, which would append an empty part")
		}
		// A URL naming a message that does not exist must fail rather than
		// resolve to nothing.
		bogus := fmt.Sprintf("imap://localhost/%s/;UID=4294967295", mailbox)
		if bogusReader, err := catenate.ResolveCatenateURL(context.Background(), bogus, nil); err == nil {
			if bogusReader != nil {
				defer bogusReader.Close()
				if data, _ := io.ReadAll(bogusReader); len(data) == 0 {
					t.Error("a URL for a nonexistent message resolved to an empty part instead of failing")
				}
			}
		}
	})

	t.Run("filter-lookup-distinguishes-unknown-from-empty", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireAllowedCapabilities(t, instance, session, "FILTERS")
		filters, ok := session.(imapserver.FilterSession)
		if !ok {
			t.Skip("backend does not implement FilterSession")
		}
		// The framework turns a nil result into UNDEFINED-FILTER, and criteria
		// into a substituted tree. Those are the only two outcomes it can tell
		// apart, so a backend must not signal "no such filter" any other way —
		// in particular not by returning criteria that match nothing, which the
		// client would read as a successful search.
		criteria, err := filters.Filter(context.Background(), "no-such-filter-9999", nil)
		if err != nil {
			// An explicit error is also unambiguous, so it conforms.
			return
		}
		if criteria != nil {
			t.Errorf("an undefined filter name returned criteria %#v; return nil so the framework can report UNDEFINED-FILTER", criteria)
		}
	})

	t.Run("comparator-reports-an-active-comparator", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireAllowedCapabilities(t, instance, session, "I18NLEVEL=2", "I18NLEVEL=1")
		comparators, ok := session.(imapserver.ComparatorSession)
		if !ok {
			t.Skip("backend does not implement ComparatorSession")
		}
		data, err := comparators.Comparators(context.Background(), nil)
		if err != nil {
			t.Fatalf("Comparators: %v", err)
		}
		// The COMPARATOR response always names the comparator in force, so there
		// has to be one even before anything is selected.
		if data == nil || data.Active == "" {
			t.Fatalf("no active comparator reported: %#v", data)
		}
		// Every offered name must be selectable, and selecting one must make it
		// active — otherwise the client cannot tell which comparator its
		// subsequent SEARCH ran under.
		for _, name := range data.Available {
			adopted, err := comparators.SetComparator(context.Background(), []string{name}, nil)
			if err != nil {
				t.Errorf("advertised comparator %q cannot be selected: %v", name, err)
				continue
			}
			if adopted == nil || adopted.Active == "" {
				t.Errorf("SetComparator(%q) adopted nothing", name)
				continue
			}
			now, err := comparators.Comparators(context.Background(), nil)
			if err != nil || now == nil {
				t.Errorf("Comparators after SetComparator(%q): %v", name, err)
				continue
			}
			if now.Active != adopted.Active {
				t.Errorf("SetComparator(%q) reported %q but Comparators reports %q",
					name, adopted.Active, now.Active)
			}
		}
		// A request naming nothing servable is nil, which the framework turns
		// into BADCOMPARATOR. Adopting an unrelated comparator instead would run
		// the client's searches under rules it did not ask for.
		if adopted, _ := comparators.SetComparator(context.Background(), []string{"i;no-such-comparator"}, nil); adopted != nil && adopted.Active != "" {
			t.Errorf("an unavailable comparator was adopted as %q", adopted.Active)
		}
	})

	t.Run("notify-refuses-what-it-cannot-deliver", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireAllowedCapabilities(t, instance, session, "NOTIFY")
		notifier, ok := session.(imapserver.NotifySession)
		if !ok {
			t.Skip("backend does not implement NotifySession")
		}
		mailbox := populate(t, session, "notify")
		updater := &imapserver.SessionUpdater{
			PushFunc: func(*imapserver.SessionUpdate) error { return nil },
		}
		// A registration the backend can serve must be accepted. The framework
		// only passes legal ones: RFC 5465 section 5 requires MessageNew and
		// MessageExpunge together.
		supported := &imapserver.NotifyConfig{
			Watches: []imapserver.NotifyWatch{{
				Specifier: imap.NotifyMailboxes,
				Names:     []string{mailbox},
				Events:    []imap.NotifyEventName{imap.NotifyEventMessageNew, imap.NotifyEventMessageExpunge},
			}},
		}
		if err := notifier.Notify(context.Background(), updater, supported, nil); err != nil {
			t.Fatalf("Notify with MessageNew and MessageExpunge on an explicit mailbox: %v", err)
		}
		// An unrecognised specifier or event must be refused. Accepting one and
		// then never delivering is indistinguishable to the client from a mailbox
		// where nothing happens, so silence is not an available answer.
		unknown := &imapserver.NotifyConfig{
			Watches: []imapserver.NotifyWatch{{
				Specifier: imap.NotifyMailboxSpecifier("NO-SUCH-SPECIFIER"),
				Events:    []imap.NotifyEventName{imap.NotifyEventMessageNew, imap.NotifyEventMessageExpunge},
			}},
		}
		if err := notifier.Notify(context.Background(), updater, unknown, nil); err == nil {
			t.Error("an unrecognised NOTIFY specifier was accepted; refuse it so the client is not left watching nothing")
		}
		unknownEvent := &imapserver.NotifyConfig{
			Watches: []imapserver.NotifyWatch{{
				Specifier: imap.NotifyMailboxes,
				Names:     []string{mailbox},
				Events:    []imap.NotifyEventName{"NoSuchEvent9999"},
			}},
		}
		if err := notifier.Notify(context.Background(), updater, unknownEvent, nil); err == nil {
			t.Error("an unrecognised NOTIFY event was accepted; refuse it rather than never delivering it")
		}
		// NOTIFY NONE must always be accepted: it is how a client stops delivery.
		if err := notifier.Notify(context.Background(), updater, nil, nil); err != nil {
			t.Errorf("NOTIFY NONE was refused: %v", err)
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
