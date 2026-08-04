//go:build interop

package imapclient_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

// t09Session is one live client plus the mailbox name it works on.
func t09Dial(t *testing.T, ctx context.Context, server *harness.Server, enableQResync bool) *imapclient.Client {
	t.Helper()
	client, err := interopDial(ctx, server, &imapclient.Options{AllowInsecureAuth: true})
	if err != nil {
		t.Fatalf("dial %s: %v", server.Profile.Name, err)
	}
	if err := client.Login(ctx, "interop@example.test", "interop-pw", nil); err != nil {
		_ = client.Close()
		t.Fatalf("login %s: %v", server.Profile.Name, err)
	}
	if err := client.Capability(ctx, nil); err != nil {
		_ = client.Close()
		t.Fatalf("capability %s: %v", server.Profile.Name, err)
	}
	if enableQResync {
		enabled, err := client.Enable(nil, "QRESYNC").Wait(ctx)
		if err != nil {
			_ = client.Close()
			t.Fatalf("ENABLE QRESYNC on %s: %v", server.Profile.Name, err)
		}
		if !client.QResyncEnabled() {
			_ = client.Close()
			t.Fatalf("%s did not enable QRESYNC: %v", server.Profile.Name, enabled)
		}
	}
	return client
}

func t09Message(subject string) string {
	return "From: sender@example.test\r\nTo: receiver@example.test\r\nSubject: " + subject +
		"\r\nMessage-Id: <" + subject + "@example.test>\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=us-ascii\r\n\r\nbody of " + subject + "\r\n"
}

func t09Append(t *testing.T, ctx context.Context, client *imapclient.Client, mailbox, subject string) {
	t.Helper()
	message := t09Message(subject)
	if _, err := client.Append(ctx, mailbox, nil, int64(len(message)), strings.NewReader(message)).Wait(ctx); err != nil {
		t.Fatalf("APPEND %s: %v", subject, err)
	}
}

// t09EveryUID addresses every message in a mailbox. Prefer the dynamic form
// "1:*" now that sequence-set encoding writes "*" via Special rather than Atom.
var t09EveryUID = imap.UIDSetRange(1, 0)

// t09UIDsBySubject returns the UID of every message in the selected mailbox,
// keyed by its Subject header, so the test can name messages independently of
// the sequence numbers the server happens to assign.
func t09UIDsBySubject(t *testing.T, ctx context.Context, client *imapclient.Client) map[string]imap.UID {
	t.Helper()
	uids := make(map[string]imap.UID)
	cmd := client.FetchUID(t09EveryUID, nil, imap.FetchItemUID, imap.FetchItemEnvelope)
	for {
		data, err := cmd.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("UID FETCH: %v", err)
		}
		var uid imap.UID
		var subject string
		for _, value := range data.Items["UID"] {
			if v, ok := value.(imap.FetchDataUID); ok {
				uid = imap.UID(v)
			}
		}
		for _, value := range data.Items["ENVELOPE"] {
			if v, ok := value.(*imap.FetchDataEnvelope); ok && v.Envelope != nil {
				subject = v.Envelope.Subject
			}
		}
		if uid != 0 && subject != "" {
			uids[subject] = uid
		}
	}
	if err := cmd.Wait(ctx); err != nil {
		t.Fatalf("UID FETCH completion: %v", err)
	}
	return uids
}

func t09Flags(t *testing.T, data *imap.FetchMessageData) []imap.Flag {
	t.Helper()
	var flags []imap.Flag
	for _, value := range data.Items["FLAGS"] {
		if v, ok := value.(imap.FetchDataFlags); ok {
			flags = append(flags, v...)
		}
	}
	return flags
}

func t09UID(data *imap.FetchMessageData) (imap.UID, bool) {
	for _, value := range data.Items["UID"] {
		if v, ok := value.(imap.FetchDataUID); ok {
			return imap.UID(v), true
		}
	}
	return 0, false
}

func t09VanishedContains(vanished []imapclient.VanishedData, uid imap.UID) (found, earlier bool) {
	for _, v := range vanished {
		if v.UIDs.Contains(uid) {
			return true, v.Earlier
		}
	}
	return false, false
}

// t09Publish makes a freshly created mailbox visible to sessions other than the
// one that created it.
//
// This works around a server defect, not a protocol requirement. Stalwart
// 0.11.8 keeps a per-account mailbox cache that a session which logs in after
// the CREATE can be served from in its stale form: the new mailbox is then
// answered NO [NONEXISTENT] to every other connection indefinitely, while the
// creating session uses it happily. One LIST issued by a session that was
// already connected when the CREATE happened refreshes that cache and the
// mailbox stays visible from then on, which is what this does. Dovecot and
// Cyrus need none of it and are unaffected.
//
// It is deliberately an assertion as well as a workaround: if the mailbox is
// still invisible afterwards, the test fails here rather than misattributing
// the failure to QRESYNC.
func t09Publish(t *testing.T, ctx context.Context, observer *imapclient.Client, mailbox string) {
	t.Helper()
	boxes, err := observer.List("", "*", nil).Wait(ctx)
	if err != nil {
		t.Fatalf("LIST after CREATE %s: %v", mailbox, err)
	}
	for _, box := range boxes {
		if box.Mailbox == mailbox {
			return
		}
	}
	names := make([]string, 0, len(boxes))
	for _, box := range boxes {
		names = append(names, box.Mailbox)
	}
	t.Fatalf("mailbox %s is not visible to a second connection after CREATE; LIST returned %v", mailbox, names)
}

// TestQResyncIncrementalResyncInterop is the load-bearing test for T09.
//
// It synchronises a mailbox, disconnects, mutates the mailbox from a completely
// separate connection — adding messages, changing a flag, and expunging a
// message — then reconnects and asks the server for the delta with
// SELECT ... (QRESYNC ...). It asserts that the reported delta is exactly the
// three mutations, because a client that applies a wrong delta silently
// corrupts its local cache, which is the failure this whole extension group
// exists to avoid.
//
// The delta is then asked for a second way, with
// UID FETCH ... (CHANGEDSINCE n VANISHED), and the two must agree.
func TestQResyncIncrementalResyncInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			// The observer connection is opened before the test mailbox exists
			// and stays open for the whole test; see t09Publish for why.
			observer := t09Dial(t, ctx, server, false)
			defer observer.Close()

			capabilities := observer.Capabilities()
			harness.AssertProfile(t, server.Profile, capabilities)
			harness.RequireCapabilities(t, capabilities, "QRESYNC", "CONDSTORE")

			mailbox := server.Profile.MailboxPrefix + harness.UniqueMailbox("t09-resync")

			// --- Phase 1: initial synchronisation. ------------------------------
			first := t09Dial(t, ctx, server, true)
			defer first.Close()
			if err := first.Create(mailbox, nil).Wait(ctx); err != nil {
				t.Fatalf("CREATE %s: %v", mailbox, err)
			}
			t09Publish(t, ctx, observer, mailbox)
			defer func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cleanupCancel()
				_ = observer.Delete(mailbox, nil).Wait(cleanupCtx)
			}()
			for _, subject := range []string{"t09-a", "t09-b", "t09-c", "t09-d"} {
				t09Append(t, ctx, first, mailbox, subject)
			}

			anchor, err := first.SelectSync(mailbox, &imapclient.SyncSelectOptions{CondStore: true}).Wait(ctx)
			if err != nil {
				server.LogDiagnostics(context.Background(), t, nil)
				t.Fatalf("SELECT (CONDSTORE): %v", err)
			}
			if anchor.NoModSeq {
				t.Skipf("%s reports NOMODSEQ for a freshly created mailbox, so it cannot be resynchronised incrementally", server.Profile.Name)
			}
			if anchor.Status.UIDValidity == 0 || anchor.Status.HighestModSeq == 0 {
				t.Fatalf("incomplete synchronisation anchor: UIDVALIDITY=%d HIGHESTMODSEQ=%d",
					anchor.Status.UIDValidity, anchor.Status.HighestModSeq)
			}
			if anchor.Status.NumMessages != 4 {
				t.Fatalf("EXISTS = %d, want 4", anchor.Status.NumMessages)
			}
			knownSubjects := t09UIDsBySubject(t, ctx, first)
			if len(knownSubjects) != 4 {
				t.Fatalf("resolved %d UIDs, want 4: %v", len(knownSubjects), knownSubjects)
			}
			knownUIDs := imap.UIDSet(nil)
			for _, uid := range knownSubjects {
				knownUIDs.AddNum(uid)
			}
			knownUIDs = knownUIDs.Normalized()
			cachedUIDValidity := anchor.Status.UIDValidity
			cachedModSeq := anchor.Status.HighestModSeq

			// --- Phase 2: disconnect. -------------------------------------------
			if err := first.Logout(ctx, nil); err != nil {
				t.Fatalf("LOGOUT: %v", err)
			}

			// --- Phase 3: mutate from a second, independent connection. ---------
			second := t09Dial(t, ctx, server, false)
			if _, err := second.Select(mailbox, nil).Wait(ctx); err != nil {
				t.Fatalf("second connection SELECT: %v", err)
			}
			t09Append(t, ctx, second, mailbox, "t09-e")
			t09Append(t, ctx, second, mailbox, "t09-f")
			flagged := knownSubjects["t09-b"]
			expunged := knownSubjects["t09-c"]
			if flagged == 0 || expunged == 0 {
				t.Fatalf("could not resolve the messages to mutate: %v", knownSubjects)
			}
			if err := second.StoreUID(imap.UIDSetNum(flagged), []imap.Flag{imap.FlagFlagged},
				&imapclient.StoreOptions{Op: imapclient.StoreFlagsAdd, Silent: true}).Wait(ctx); err != nil {
				t.Fatalf("second connection UID STORE: %v", err)
			}
			if err := second.StoreUID(imap.UIDSetNum(expunged), []imap.Flag{imap.FlagDeleted},
				&imapclient.StoreOptions{Op: imapclient.StoreFlagsAdd, Silent: true}).Wait(ctx); err != nil {
				t.Fatalf("second connection UID STORE \\Deleted: %v", err)
			}
			if err := second.Expunge(nil).Wait(ctx); err != nil {
				t.Fatalf("second connection EXPUNGE: %v", err)
			}
			if err := second.Logout(ctx, nil); err != nil {
				t.Fatalf("second connection LOGOUT: %v", err)
			}

			// --- Phase 4: reconnect and ask for the delta. ----------------------
			third := t09Dial(t, ctx, server, true)
			defer third.Close()
			resync, err := third.SelectSync(mailbox, &imapclient.SyncSelectOptions{
				QResync: &imapclient.QResyncOptions{
					UIDValidity: cachedUIDValidity,
					ModSeq:      cachedModSeq,
					KnownUIDs:   knownUIDs,
				},
			}).Wait(ctx)
			if err != nil {
				server.LogDiagnostics(context.Background(), t, nil)
				t.Fatalf("SELECT (QRESYNC): %v", err)
			}

			// --- Phase 5: assert the delta is exactly the three mutations. ------
			if resync.ResyncRejected {
				t.Fatalf("the server rejected the synchronisation anchor (UIDVALIDITY was %d, is %d; NOMODSEQ=%v)",
					cachedUIDValidity, resync.Status.UIDValidity, resync.NoModSeq)
			}
			if resync.Status.UIDValidity != cachedUIDValidity {
				t.Fatalf("UIDVALIDITY changed from %d to %d", cachedUIDValidity, resync.Status.UIDValidity)
			}
			if resync.Status.HighestModSeq <= cachedModSeq {
				t.Fatalf("HIGHESTMODSEQ did not advance across the mutations: %d then %d",
					cachedModSeq, resync.Status.HighestModSeq)
			}
			if resync.Status.NumMessages != 5 {
				t.Fatalf("EXISTS = %d after two appends and one expunge, want 5", resync.Status.NumMessages)
			}

			found, earlier := t09VanishedContains(resync.Vanished, expunged)
			if !found {
				t.Fatalf("UID %d was expunged while disconnected but no VANISHED response reported it: %v",
					expunged, resync.Vanished)
			}
			if !earlier {
				t.Errorf("the expunge that happened while disconnected was not tagged (EARLIER): %v", resync.Vanished)
			}
			for _, subject := range []string{"t09-a", "t09-b", "t09-d"} {
				if surviving, _ := t09VanishedContains(resync.Vanished, knownSubjects[subject]); surviving {
					t.Errorf("VANISHED named UID %d (%s), which still exists: %v",
						knownSubjects[subject], subject, resync.Vanished)
				}
			}

			sawFlagUpdate := false
			for _, message := range resync.Fetched {
				uid, ok := t09UID(message)
				if !ok {
					t.Errorf("a QRESYNC FETCH response carried no UID: %#v", message.Items)
					continue
				}
				if uid != flagged {
					continue
				}
				if imap.ContainsFlag(t09Flags(t, message), imap.FlagFlagged) {
					sawFlagUpdate = true
				}
			}
			if !sawFlagUpdate {
				t.Fatalf("the flag change made on UID %d while disconnected was not reported: %#v",
					flagged, resync.Fetched)
			}

			// --- Phase 6: the same delta through UID FETCH CHANGEDSINCE VANISHED.
			fetchCmd := third.FetchUIDSync(knownUIDs, &imapclient.SyncFetchOptions{
				ChangedSince:   cachedModSeq,
				ReportVanished: true,
			}, imap.FetchItemUID, imap.FetchItemFlags)
			var refetched []*imap.FetchMessageData
			for {
				data, err := fetchCmd.Next(ctx)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("UID FETCH CHANGEDSINCE: %v", err)
				}
				refetched = append(refetched, data)
			}
			if err := fetchCmd.Wait(ctx); err != nil {
				t.Fatalf("UID FETCH CHANGEDSINCE completion: %v", err)
			}
			if found, _ := t09VanishedContains(fetchCmd.Vanished(), expunged); !found {
				t.Fatalf("UID FETCH (CHANGEDSINCE %d VANISHED) did not report UID %d as expunged: %v",
					cachedModSeq, expunged, fetchCmd.Vanished())
			}
			sawFlagUpdate = false
			for _, message := range refetched {
				if uid, ok := t09UID(message); ok && uid == flagged &&
					imap.ContainsFlag(t09Flags(t, message), imap.FlagFlagged) {
					sawFlagUpdate = true
				}
			}
			if !sawFlagUpdate {
				t.Fatalf("UID FETCH (CHANGEDSINCE %d) did not report the flag change on UID %d: %#v",
					cachedModSeq, flagged, refetched)
			}
			if !third.CondStoreEnabled() {
				t.Error("ENABLE QRESYNC did not make the session CONDSTORE-aware (RFC 7162 section 3.2.3)")
			}
		})
	}
}

// TestQResyncRejectedAnchorInterop proves against live servers that a stale
// UIDVALIDITY is reported rather than silently producing an empty delta. RFC
// 7162 section 3.2.5 has the server answer a mismatched anchor by ignoring the
// remaining parameters and saying nothing about it, so a client that does not
// compare concludes that nothing changed.
func TestQResyncRejectedAnchorInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			probe := t09Dial(t, ctx, server, false)
			capabilities := probe.Capabilities()
			_ = probe.Logout(ctx, nil)
			harness.RequireCapabilities(t, capabilities, "QRESYNC")

			mailbox := server.Profile.MailboxPrefix + harness.UniqueMailbox("t09-anchor")
			client := t09Dial(t, ctx, server, true)
			defer client.Close()
			if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
				t.Fatalf("CREATE: %v", err)
			}
			defer func() { _ = client.Delete(mailbox, nil).Wait(ctx) }()
			t09Append(t, ctx, client, mailbox, "t09-anchor-a")

			status, err := client.SelectSync(mailbox, &imapclient.SyncSelectOptions{CondStore: true}).Wait(ctx)
			if err != nil {
				t.Fatalf("SELECT (CONDSTORE): %v", err)
			}
			if status.NoModSeq {
				t.Skipf("%s reports NOMODSEQ for this mailbox", server.Profile.Name)
			}
			if err := client.Unselect(nil).Wait(ctx); err != nil {
				// UNSELECT is not universal; CLOSE is an acceptable substitute
				// here because the mailbox has no \Deleted messages.
				if err := client.CloseMailbox(nil).Wait(ctx); err != nil {
					t.Fatalf("closing the mailbox: %v", err)
				}
			}

			stale := status.Status.UIDValidity + 1
			if stale == 0 {
				stale = 1
			}
			rejected, err := client.SelectSync(mailbox, &imapclient.SyncSelectOptions{
				QResync: &imapclient.QResyncOptions{UIDValidity: stale, ModSeq: status.Status.HighestModSeq},
			}).Wait(ctx)
			if err != nil {
				t.Fatalf("SELECT (QRESYNC) with a stale anchor: %v", err)
			}
			if !rejected.ResyncRejected {
				t.Fatalf("a stale UIDVALIDITY (%d, server reports %d) was not reported as a rejected anchor",
					stale, rejected.Status.UIDValidity)
			}
		})
	}
}

// TestGroupBStatusItemsInterop covers the STATUS-side extensions of this group
// against every server that advertises them.
func TestGroupBStatusItemsInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			client := t09Dial(t, ctx, server, false)
			defer client.Close()
			capabilities := client.Capabilities()

			mailbox := server.Profile.MailboxPrefix + harness.UniqueMailbox("t09-status")
			if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
				t.Fatalf("CREATE: %v", err)
			}
			defer func() { _ = client.Delete(mailbox, nil).Wait(ctx) }()
			message := t09Message("t09-status-a")
			t09Append(t, ctx, client, mailbox, "t09-status-a")

			t.Run("size", func(t *testing.T) {
				harness.RequireCapabilities(t, capabilities, "STATUS=SIZE")
				size, err := client.MailboxSize(ctx, mailbox, nil)
				if err != nil {
					t.Fatalf("STATUS (SIZE): %v", err)
				}
				// RFC 8438 section 3: the value must be at least the sum of the
				// RFC822.SIZE of every message in the mailbox.
				if size < int64(len(message)) {
					t.Fatalf("SIZE = %d, want at least the one message's %d octets", size, len(message))
				}
			})

			t.Run("highestmodseq", func(t *testing.T) {
				harness.RequireCapabilities(t, capabilities, "CONDSTORE")
				modSeq, err := client.MailboxHighestModSeq(ctx, mailbox, nil)
				if err != nil {
					t.Fatalf("STATUS (HIGHESTMODSEQ): %v", err)
				}
				if modSeq > imapclient.MaxModSeq {
					t.Fatalf("HIGHESTMODSEQ %d exceeds the RFC 7162 63-bit limit", modSeq)
				}
				if !client.CondStoreEnabled() {
					t.Error("STATUS (HIGHESTMODSEQ) did not record the CONDSTORE activation")
				}
			})

			t.Run("appendlimit", func(t *testing.T) {
				if len(client.CapabilityValues("APPENDLIMIT")) == 0 {
					harness.RequireCapabilities(t, capabilities, "APPENDLIMIT")
				}
				data, err := client.AppendLimit(ctx, mailbox, nil)
				if err != nil {
					t.Fatalf("APPENDLIMIT: %v", err)
				}
				if !data.Unlimited && data.Limit <= 0 {
					t.Fatalf("APPENDLIMIT = %#v, want a positive limit or Unlimited", data)
				}
			})
		})
	}
}

// TestCondStoreUnchangedSinceInterop exercises the conditional STORE against
// live servers, in both the succeeding and the failing direction.
func TestCondStoreUnchangedSinceInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			client := t09Dial(t, ctx, server, false)
			defer client.Close()
			harness.RequireCapabilities(t, client.Capabilities(), "CONDSTORE")

			mailbox := server.Profile.MailboxPrefix + harness.UniqueMailbox("t09-condstore")
			if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
				t.Fatalf("CREATE: %v", err)
			}
			defer func() { _ = client.Delete(mailbox, nil).Wait(ctx) }()
			t09Append(t, ctx, client, mailbox, "t09-condstore-a")

			status, err := client.SelectSync(mailbox, &imapclient.SyncSelectOptions{CondStore: true}).Wait(ctx)
			if err != nil {
				t.Fatalf("SELECT (CONDSTORE): %v", err)
			}
			if status.NoModSeq {
				t.Skipf("%s reports NOMODSEQ for this mailbox", server.Profile.Name)
			}
			uids := t09UIDsBySubject(t, ctx, client)
			uid := uids["t09-condstore-a"]
			if uid == 0 {
				t.Fatalf("could not resolve the appended message: %v", uids)
			}

			// A store conditional on the current mod-sequence must succeed.
			current := status.Status.HighestModSeq
			data, err := client.StoreUIDSync(imap.UIDSetNum(uid), []imap.Flag{imap.FlagFlagged},
				&imapclient.SyncStoreOptions{Op: imapclient.StoreFlagsAdd, UnchangedSince: &current}).Wait(ctx)
			if err != nil {
				t.Fatalf("UID STORE (UNCHANGEDSINCE %d): %v", current, err)
			}
			if data.HasModified() {
				t.Fatalf("a store conditional on the current mod-sequence reported failures: %#v", data)
			}
			flags := t09FetchFlags(t, ctx, client, uid)
			if !imap.ContainsFlag(flags, imap.FlagFlagged) {
				t.Fatalf("the conditional store did not apply: flags = %v", flags)
			}

			// A store conditional on a mod-sequence the message has already
			// passed must not apply. RFC 7162 Example 8 uses UNCHANGEDSINCE 0
			// for exactly this.
			stale := uint64(0)
			data, err = client.StoreUIDSync(imap.UIDSetNum(uid), []imap.Flag{imap.FlagAnswered},
				&imapclient.SyncStoreOptions{Op: imapclient.StoreFlagsAdd, UnchangedSince: &stale}).Wait(ctx)
			if err != nil {
				var ierr *imap.Error
				if !errors.As(err, &ierr) || ierr.Code != imap.CodeModified {
					t.Fatalf("UID STORE (UNCHANGEDSINCE 0): %v", err)
				}
			}
			flags = t09FetchFlags(t, ctx, client, uid)
			if imap.ContainsFlag(flags, imap.FlagAnswered) {
				t.Fatalf("UNCHANGEDSINCE 0 applied a change it must have refused: flags = %v", flags)
			}
			if data == nil || !data.HasModified() || !data.ModifiedUIDs.Contains(uid) {
				t.Fatalf("%s refused the store but MODIFIED did not name UID %d: %#v (err=%v)",
					server.Profile.Name, uid, data, err)
			}
			t.Logf("%s reported the refused conditional store as err=%v modified=%v",
				server.Profile.Name, err, data)
		})
	}
}

func t09FetchFlags(t *testing.T, ctx context.Context, client *imapclient.Client, uid imap.UID) []imap.Flag {
	t.Helper()
	cmd := client.FetchUID(imap.UIDSetNum(uid), nil, imap.FetchItemUID, imap.FetchItemFlags)
	var flags []imap.Flag
	for {
		data, err := cmd.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("UID FETCH FLAGS: %v", err)
		}
		flags = append(flags, t09Flags(t, data)...)
	}
	if err := cmd.Wait(ctx); err != nil {
		t.Fatalf("UID FETCH FLAGS completion: %v", err)
	}
	return flags
}

// TestReplaceInterop exercises REPLACE where it is advertised and the emulated
// fallback where it is not, so both halves of the capability gate are covered
// by live servers rather than only by scripted ones.
func TestReplaceInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			client := t09Dial(t, ctx, server, false)
			defer client.Close()
			capabilities := client.Capabilities()
			native := capabilities["REPLACE"]
			if !native && !capabilities["UIDPLUS"] {
				t.Skip("server advertises neither REPLACE nor the UIDPLUS the fallback needs")
			}

			mailbox := server.Profile.MailboxPrefix + harness.UniqueMailbox("t09-replace")
			if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
				t.Fatalf("CREATE: %v", err)
			}
			defer func() { _ = client.Delete(mailbox, nil).Wait(ctx) }()
			t09Append(t, ctx, client, mailbox, "t09-replace-old")
			if _, err := client.Select(mailbox, nil).Wait(ctx); err != nil {
				t.Fatalf("SELECT: %v", err)
			}
			uids := t09UIDsBySubject(t, ctx, client)
			old := uids["t09-replace-old"]
			if old == 0 {
				t.Fatalf("could not resolve the original message: %v", uids)
			}

			replacement := t09Message("t09-replace-new")
			data, err := client.ReplaceUID(ctx, old, mailbox,
				&imapclient.ReplaceOptions{AllowNonAtomicFallback: true},
				int64(len(replacement)), strings.NewReader(replacement))
			if err != nil {
				server.LogDiagnostics(context.Background(), t, nil)
				t.Fatalf("UID REPLACE (native=%v): %v", native, err)
			}
			if data.Emulated == native {
				t.Fatalf("REPLACE advertised=%v but Emulated=%v", native, data.Emulated)
			}

			// The mailbox must now hold exactly the replacement.
			if err := client.Unselect(nil).Wait(ctx); err != nil {
				_ = client.CloseMailbox(nil).Wait(ctx)
			}
			if _, err := client.Select(mailbox, nil).Wait(ctx); err != nil {
				t.Fatalf("re-SELECT: %v", err)
			}
			after := t09UIDsBySubject(t, ctx, client)
			if _, stillThere := after["t09-replace-old"]; stillThere {
				t.Errorf("REPLACE left the original message behind: %v", after)
			}
			if _, ok := after["t09-replace-new"]; !ok {
				t.Errorf("REPLACE did not store the replacement: %v", after)
			}
			if native && data.UID == 0 {
				t.Logf("%s: native REPLACE returned no untagged APPENDUID (%s)",
					server.Profile.Name, fmt.Sprint(data))
			}
		})
	}
}

// TestSaveDatePreviewObjectIDInterop covers SAVEDATE (RFC 8514), PREVIEW
// (RFC 8970) and OBJECTID (RFC 8474) against every server that advertises them.
func TestSaveDatePreviewObjectIDInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			client := t09Dial(t, ctx, server, false)
			defer client.Close()
			capabilities := client.Capabilities()
			if !capabilities["SAVEDATE"] && !capabilities["PREVIEW"] && !capabilities["OBJECTID"] {
				t.Skip("server advertises none of SAVEDATE, PREVIEW, OBJECTID")
			}

			mailbox := server.Profile.MailboxPrefix + harness.UniqueMailbox("t09-items")
			if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
				t.Fatalf("CREATE: %v", err)
			}
			defer func() { _ = client.Delete(mailbox, nil).Wait(ctx) }()
			t09Append(t, ctx, client, mailbox, "t09-items-a")

			if capabilities["OBJECTID"] {
				status, err := client.Status(mailbox, &imapclient.StatusOptions{
					Items: []imap.StatusItem{imap.StatusItemMailboxID},
				}).Wait(ctx)
				if err != nil {
					t.Fatalf("STATUS MAILBOXID: %v", err)
				}
				id, ok := status.Values[imap.StatusItemMailboxID].(string)
				if !ok || id == "" {
					t.Fatalf("MAILBOXID = %#v", status.Values[imap.StatusItemMailboxID])
				}
				t.Logf("MAILBOXID = %q", id)
			}

			if _, err := client.Select(mailbox, nil).Wait(ctx); err != nil {
				t.Fatalf("SELECT: %v", err)
			}

			items := []imap.FetchItem{imap.FetchItemUID}
			if capabilities["SAVEDATE"] {
				items = append(items, imap.FetchItemSaveDate)
			}
			if capabilities["PREVIEW"] {
				items = append(items, &imap.FetchItemPreview{Lazy: true})
			}
			if capabilities["OBJECTID"] {
				items = append(items, imap.FetchItemEmailID, imap.FetchItemThreadID)
			}

			cmd := client.FetchUID(imap.UIDSetRange(1, 0), nil, items...)
			var sawSaveDate, sawPreview, sawEmailID bool
			for {
				data, err := cmd.Next(ctx)
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					server.LogDiagnostics(context.Background(), t, nil)
					t.Fatalf("UID FETCH: %v", err)
				}
				if values := data.Items[imap.FetchDataKey("SAVEDATE")]; len(values) != 0 {
					if _, ok := values[0].(*imap.FetchDataSaveDate); !ok {
						t.Fatalf("SAVEDATE = %#v, want *FetchDataSaveDate", values[0])
					}
					sawSaveDate = true
				}
				if values := data.Items[imap.FetchDataKey("PREVIEW")]; len(values) != 0 {
					if _, ok := values[0].(*imap.FetchDataPreview); !ok {
						t.Fatalf("PREVIEW = %#v, want *FetchDataPreview", values[0])
					}
					sawPreview = true
				}
				if values := data.Items[imap.FetchDataKey("EMAILID")]; len(values) != 0 {
					id, ok := values[0].(imap.FetchDataObjectID)
					if !ok || id == "" {
						t.Fatalf("EMAILID = %#v", values[0])
					}
					sawEmailID = true
					t.Logf("EMAILID = %q", id)
				}
			}
			if err := cmd.Wait(ctx); err != nil {
				t.Fatalf("UID FETCH completion: %v", err)
			}
			if capabilities["SAVEDATE"] && !sawSaveDate {
				t.Fatal("SAVEDATE was requested but no typed value reached the caller")
			}
			if capabilities["PREVIEW"] && !sawPreview {
				t.Fatal("PREVIEW was requested but no typed value reached the caller")
			}
			if capabilities["OBJECTID"] && !sawEmailID {
				t.Fatal("EMAILID was requested but no typed value reached the caller")
			}
		})
	}
}
