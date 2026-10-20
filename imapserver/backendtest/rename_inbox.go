package backendtest

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// runRenameInbox checks Session.Rename's documented special case, RFC 3501
// section 6.3.5: renaming INBOX moves its messages to the new name, leaves
// INBOX itself in place, empty, and does not touch INBOX's inferiors.
func runRenameInbox(t *testing.T, harness *Harness) {
	instance, session := newSession(t, harness)
	defer closeSession(t, instance, session)
	ctx := context.Background()
	const archive, taken = "backendtest-inbox-archive", "backendtest-inbox-taken"
	// Inferiors of INBOX are optional (RFC 3501 section 6.3.5), so a backend
	// refusing to create one only skips that part of the check.
	child := ""
	if d := hierarchyDelimiterOrEmpty(t, session); d != "" {
		if err := session.Create(ctx, "INBOX"+d+"backendtest-child", nil); err == nil {
			child = "INBOX" + d + "backendtest-child"
			appendMessage(t, session, child, "child stays")
		}
	}
	date := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	raws := [][]byte{
		[]byte("From: a@example.com\r\nSubject: inbox one\r\n\r\none\r\n"),
		[]byte("From: b@example.com\r\nSubject: inbox two\r\n\r\ntwo\r\n"),
	}
	flags := [][]imap.Flag{{imap.FlagFlagged, "$backendtest"}, {imap.FlagSeen}}
	for i, raw := range raws {
		if _, err := session.Append(ctx, "INBOX", bytes.NewReader(raw), &imapserver.AppendOptions{Flags: flags[i], InternalDate: date.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatalf("Append to INBOX: %v", err)
		}
	}
	if err := session.Create(ctx, taken, nil); err != nil {
		t.Fatal(err)
	}

	// A selection of INBOX in another session must see the messages leave.
	_, watching := authenticate(t, instance)
	defer closeSession(t, instance, watching)
	var (
		mu            sync.Mutex
		batches       []*imapserver.UpdateBatch
		notifications = make(chan struct{}, 8)
	)
	selected := selectMailbox(t, watching, "INBOX", &imapserver.Updater{PushFunc: func(batch *imapserver.UpdateBatch) error {
		mu.Lock()
		batches = append(batches, cloneBatch(batch))
		mu.Unlock()
		select {
		case notifications <- struct{}{}:
		default:
		}
		return nil
	}})
	defer selected.Mailbox.Unselect(context.Background(), nil)
	before := selected.Snapshot
	if len(before.UIDs) != len(raws) {
		t.Fatalf("INBOX lists %v, want the %d seeded messages", before.UIDs, len(raws))
	}
	beforeID := ""
	if supportsCapability(instance, session, "OBJECTID") {
		beforeID = mailboxObjectID(t, session, "INBOX")
	}

	// An occupied destination refuses the whole operation.
	if err := session.Rename(ctx, "INBOX", taken, nil); err == nil {
		t.Fatal("RENAME INBOX onto an existing mailbox succeeded")
	}
	if got := messageCount(t, session, "INBOX"); got != uint32(len(raws)) {
		t.Fatalf("refused RENAME left INBOX with %d messages", got)
	}
	if got := messageCount(t, session, taken); got != 0 {
		t.Fatalf("refused RENAME left %d messages in the destination", got)
	}

	if err := session.Rename(ctx, "INBOX", archive, nil); err != nil {
		t.Fatalf("RENAME INBOX: %v", err)
	}
	listed := listMailboxes(t, session, "", []string{"*"}, nil)
	for _, name := range []string{"INBOX", archive, child} {
		if data := listed[name]; name != "" && (data == nil || imap.ContainsAttr(data.Attrs, imap.MailboxAttrNonExistent)) {
			t.Errorf("after RENAME INBOX, %s is missing: %v", name, listed)
		}
	}
	if child != "" && messageCount(t, session, child) != 1 {
		t.Errorf("RENAME INBOX changed its inferior %s", child)
	}

	// The removals reach the existing selection, continuing its update chain.
	removed := make(map[imap.UID]bool)
	deadline := time.After(time.Second)
	for len(removed) < len(before.UIDs) {
		mu.Lock()
		revision := before.Revision
		for _, batch := range batches {
			if batch.Before != revision {
				mu.Unlock()
				t.Fatalf("update chain broken at %q, want %q", batch.Before, revision)
			}
			revision = batch.After
			for _, change := range batch.Changes {
				switch change := change.(type) {
				case *imapserver.UpdateExpunge:
					removed[change.UID] = true
				case *imapserver.UpdateVanished:
					for _, uid := range before.UIDs {
						if change.UIDs.Contains(uid) {
							removed[uid] = true
						}
					}
				}
			}
		}
		mu.Unlock()
		if len(removed) == len(before.UIDs) {
			break
		}
		select {
		case <-notifications:
		case <-deadline:
			t.Fatalf("the INBOX selection saw removals of %v, want all of %v", removed, before.UIDs)
		}
	}

	// UIDs may be reused only if INBOX has a new UIDVALIDITY (RFC 3501
	// section 6.3.5). Clocks and resync history apply to one incarnation.
	after := selectMailbox(t, session, "INBOX", discardUpdater())
	if len(after.Snapshot.UIDs) != 0 {
		t.Errorf("INBOX still lists %v after RENAME", after.Snapshot.UIDs)
	}
	sameIncarnation := after.Snapshot.Status.UIDValidity == before.Status.UIDValidity
	if sameIncarnation && after.Snapshot.Status.UIDNext < before.Status.UIDNext {
		t.Errorf("INBOX UIDVALIDITY/UIDNEXT went from %d/%d to %d/%d", before.Status.UIDValidity, before.Status.UIDNext, after.Snapshot.Status.UIDValidity, after.Snapshot.Status.UIDNext)
	}
	if sameIncarnation && supportsCapability(instance, session, "CONDSTORE") && !before.Status.NoModSeq && after.Snapshot.Status.HighestModSeq <= before.Status.HighestModSeq {
		t.Errorf("INBOX HIGHESTMODSEQ did not advance past %d for the removals: %d", before.Status.HighestModSeq, after.Snapshot.Status.HighestModSeq)
	}
	if resync, ok := after.Mailbox.(imapserver.QResyncMailbox); ok && sameIncarnation && supportsCapability(instance, session, "QRESYNC") && !before.Status.NoModSeq {
		report, err := resync.Resync(ctx, &imapserver.QResyncSelect{UIDValidity: before.Status.UIDValidity, ModSeq: before.Status.HighestModSeq}, nil)
		if err != nil || report == nil {
			t.Fatalf("Resync after RENAME INBOX = %#v, %v", report, err)
		}
		for _, uid := range before.UIDs {
			if !report.Vanished.Contains(uid) {
				t.Errorf("QRESYNC does not report moved UID %d as vanished: %v", uid, report.Vanished)
			}
		}
	}
	if err := after.Mailbox.Unselect(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if beforeID != "" {
		if id := mailboxObjectID(t, session, archive); id == beforeID {
			t.Errorf("the new mailbox took INBOX's MAILBOXID %s", id)
		}
	}

	// The messages retain their flags, INTERNALDATE and bytes. FETCH order
	// is unrestricted, so identify each fixture by its body.
	moved := selectMailbox(t, session, archive, discardUpdater())
	defer moved.Mailbox.Unselect(context.Background(), nil)
	if len(moved.Snapshot.UIDs) != len(raws) {
		t.Fatalf("%s lists %v, want %d messages", archive, moved.Snapshot.UIDs, len(raws))
	}
	// Literals are valid only during the writer call, so each message is
	// read there.
	type movedMessage struct {
		flags []imap.Flag
		date  time.Time
		body  []byte
	}
	var fetched []movedMessage
	writer := &imapserver.FetchWriter{WriteFunc: func(_ context.Context, data *imap.FetchMessageData) error {
		fetched = append(fetched, movedMessage{flags: fetchedFlags(data), date: fetchedInternalDate(data), body: fetchedBody(t, data)})
		return nil
	}}
	items := []imap.FetchItem{imap.FetchItemUID, imap.FetchItemFlags, imap.FetchItemInternalDate, &imap.FetchItemBodySection{Peek: true}}
	if err := moved.Mailbox.Fetch(ctx, writer, imap.UIDSetNum(moved.Snapshot.UIDs...), &imapserver.FetchOptions{Items: items}); err != nil {
		t.Fatal(err)
	}
	if len(fetched) != len(raws) {
		t.Fatalf("FETCH from %s returned %d messages", archive, len(fetched))
	}
	seen := make(map[int]bool)
	for _, message := range fetched {
		i := -1
		for j, raw := range raws {
			if bytes.Equal(message.body, raw) {
				i = j
				break
			}
		}
		if i < 0 || seen[i] {
			t.Errorf("unexpected or duplicate moved message: %q", message.body)
			continue
		}
		seen[i] = true
		for _, flag := range flags[i] {
			if !imap.ContainsFlag(message.flags, flag) {
				t.Errorf("moved message %d lost %s: %v", i, flag, message.flags)
			}
		}
		if !message.date.Equal(date.Add(time.Duration(i) * time.Hour)) {
			t.Errorf("moved message %d has INTERNALDATE %v", i, message.date)
		}
		if !bytes.Equal(message.body, raws[i]) {
			t.Errorf("moved message %d has body %q", i, message.body)
		}
	}
}

func hierarchyDelimiterOrEmpty(t *testing.T, session imapserver.Session) string {
	t.Helper()
	for _, data := range listMailboxes(t, session, "", []string{""}, nil) {
		if data.Delimiter != 0 {
			return string(data.Delimiter)
		}
	}
	return ""
}

func messageCount(t *testing.T, session imapserver.Session, mailbox string) uint32 {
	t.Helper()
	data, err := session.Status(context.Background(), mailbox, &imapserver.StatusOptions{Items: []imap.StatusItem{imap.StatusItemMessages}})
	if err != nil || data == nil {
		t.Fatalf("STATUS %s = %#v, %v", mailbox, data, err)
	}
	n, ok := data.Number(imap.StatusItemMessages)
	if !ok {
		t.Fatalf("STATUS %s has no MESSAGES", mailbox)
	}
	return uint32(n)
}

func mailboxObjectID(t *testing.T, session imapserver.Session, mailbox string) string {
	t.Helper()
	data, err := session.Status(context.Background(), mailbox, &imapserver.StatusOptions{Items: []imap.StatusItem{imap.StatusItemMailboxID}})
	if err != nil || data == nil {
		t.Fatalf("STATUS %s (MAILBOXID) = %#v, %v", mailbox, data, err)
	}
	id, _ := data.Values[imap.StatusItemMailboxID].(string)
	if id == "" {
		t.Fatalf("STATUS %s has no MAILBOXID", mailbox)
	}
	return id
}

func fetchedFlags(data *imap.FetchMessageData) []imap.Flag {
	for _, value := range data.Items["FLAGS"] {
		if flags, ok := value.(imap.FetchDataFlags); ok {
			return []imap.Flag(flags)
		}
	}
	return nil
}

func fetchedInternalDate(data *imap.FetchMessageData) time.Time {
	for _, values := range data.Items {
		for _, value := range values {
			if date, ok := value.(*imap.FetchDataInternalDate); ok && date != nil {
				return date.Time
			}
		}
	}
	return time.Time{}
}

func fetchedBody(t *testing.T, data *imap.FetchMessageData) []byte {
	t.Helper()
	for _, values := range data.Items {
		for _, value := range values {
			if section, ok := value.(*imap.FetchDataBodySection); ok && section != nil && section.Literal != nil {
				var buf bytes.Buffer
				if _, err := buf.ReadFrom(section.Literal); err != nil {
					t.Fatal(err)
				}
				if closer, ok := section.Literal.(interface{ Close() error }); ok {
					_ = closer.Close()
				}
				return buf.Bytes()
			}
		}
	}
	return nil
}
