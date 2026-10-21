package backendtest

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// Check both the calling selection and a bystander at the call boundary.
// Waiting for a notification here would accept a late change feed, which can
// cause EXPUNGE completion and QRESYNC checkpoints to omit committed changes.
func runSynchronousPublication(t *testing.T, harness *Harness) {
	instance, session := newSession(t, harness)
	defer closeSession(t, instance, session)
	_, observer := authenticate(t, instance)
	defer closeSession(t, instance, observer)
	ctx := context.Background()
	name := populate(t, session, "publication")
	var mu sync.Mutex
	var batches [2][]*imapserver.UpdateBatch
	updater := func(i int) *imapserver.Updater {
		return &imapserver.Updater{PushFunc: func(batch *imapserver.UpdateBatch) error {
			mu.Lock()
			batches[i] = append(batches[i], cloneBatch(batch))
			mu.Unlock()
			return nil
		}}
	}
	selected := selectMailbox(t, session, name, updater(0))
	defer selected.Mailbox.Unselect(ctx, nil)
	watching := selectMailbox(t, observer, name, updater(1))
	defer watching.Mailbox.Unselect(ctx, nil)
	revisions := [2]imapserver.MailboxRevision{selected.Snapshot.Revision, watching.Snapshot.Revision}
	// Selecting may itself publish RECENT changes; start from current states.
	mu.Lock()
	for i := range batches {
		for _, batch := range batches[i] {
			revisions[i] = batch.After
		}
		batches[i] = nil
	}
	mu.Unlock()
	check := func(operation string, origin imapserver.ChangeToken, uid imap.UID, kind string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		for i := range batches {
			found := false
			for _, batch := range batches[i] {
				if batch.Before != revisions[i] {
					t.Fatalf("%s: observer %d revision gap", operation, i)
				}
				revisions[i] = batch.After
				if batch.Origin != origin {
					continue
				}
				for _, update := range batch.Changes {
					switch update := update.(type) {
					case *imapserver.UpdateAdd:
						found = found || kind == "add" && len(update.UIDs) != 0
					case *imapserver.UpdateFlags:
						found = found || kind == "flags" && update.UID == uid
					case *imapserver.UpdateExpunge:
						found = found || kind == "remove" && update.UID == uid
					case *imapserver.UpdateVanished:
						found = found || kind == "remove" && update.UIDs.Contains(uid)
					}
				}
			}
			if !found {
				t.Fatalf("%s returned before publishing %s with Origin %d to observer %d", operation, kind, origin, i)
			}
			batches[i] = nil
		}
	}
	origin := imapserver.ChangeToken(700)
	_, err := session.Append(ctx, name, strings.NewReader("Subject: publication\r\n\r\nbody\r\n"), &imapserver.AppendOptions{MutationOptions: imapserver.MutationOptions{Origin: origin}})
	if err != nil {
		t.Fatal(err)
	}
	check("APPEND", origin, 0, "add")
	writer := &imapserver.FetchWriter{WriteFunc: func(context.Context, *imap.FetchMessageData) error { return nil }}
	uid := selected.Snapshot.UIDs[0]
	origin++
	if err := selected.Mailbox.Store(ctx, writer, imap.UIDSetNum(uid), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, &imapserver.StoreOptions{MutationOptions: imapserver.MutationOptions{Origin: origin}}); err != nil {
		t.Fatal(err)
	}
	check("STORE", origin, uid, "flags")
	origin++
	if err := selected.Mailbox.Expunge(ctx, &imapserver.ExpungeWriter{WriteFunc: func(context.Context, imap.UID, *imapserver.WriteExpungeOptions) error { return nil }}, nil, &imapserver.ExpungeOptions{MutationOptions: imapserver.MutationOptions{Origin: origin}}); err != nil {
		t.Fatal(err)
	}
	check("EXPUNGE", origin, uid, "remove")
	if !supportsCapability(instance, session, "MOVE") {
		return
	}
	destination := name + "-destination"
	if err := session.Create(ctx, destination, nil); err != nil {
		t.Fatal(err)
	}
	origin++
	uid = selected.Snapshot.UIDs[1]
	mover, ok := selected.Mailbox.(imapserver.MoveMailbox)
	if !ok {
		t.Fatal("MOVE witnessed without MoveMailbox")
	}
	if _, err := mover.Move(ctx, imap.UIDSetNum(uid), destination, &imapserver.MoveOptions{MutationOptions: imapserver.MutationOptions{Origin: origin}}); err != nil {
		t.Fatal(err)
	}
	check("MOVE", origin, uid, "remove")
}
