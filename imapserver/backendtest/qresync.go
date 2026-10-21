package backendtest

import (
	"context"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func runQResyncHistory(t *testing.T, harness *Harness) {
	t.Run("qresync-repeated-history", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireWitness(t, instance, session, "QRESYNC")
		requireAllowedCapabilities(t, instance, session, "CONDSTORE")
		name := populate(t, session, "qresync-history")
		selected := selectMailbox(t, session, name, discardUpdater())
		defer selected.Mailbox.Unselect(context.Background(), nil)
		if selected.Snapshot.Status.NoModSeq {
			t.Skip("the selected mailbox does not support persistent modification sequences")
		}
		mailbox, ok := selected.Mailbox.(imapserver.QResyncMailbox)
		if !ok {
			t.Fatal("QRESYNC witness has no QResyncMailbox")
		}
		uid := selected.Snapshot.UIDs[0]
		clock := selected.Snapshot.Status.HighestModSeq
		if err := selected.Mailbox.Store(t.Context(), discardFetchWriter(), imap.UIDSetNum(uid), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil); err != nil {
			t.Fatal(err)
		}
		writer := &imapserver.ExpungeWriter{WriteFunc: func(context.Context, imap.UID, *imapserver.WriteExpungeOptions) error { return nil }}
		if err := selected.Mailbox.Expunge(t.Context(), writer, nil, nil); err != nil {
			t.Fatal(err)
		}
		// UID FETCH can ask repeatedly for a historical UID with no current
		// counterpart. Reporting history must not consume the removal record.
		for i := 0; i < 2; i++ {
			result, err := mailbox.Resync(t.Context(), &imapserver.QResyncSelect{
				UIDValidity: selected.Snapshot.Status.UIDValidity,
				ModSeq:      clock,
				KnownUIDs:   imap.UIDSetNum(uid),
			}, nil)
			if err != nil || result == nil || !result.Vanished.Equal(imap.UIDSetNum(uid)) {
				t.Fatalf("history request %d: %#v, %v", i, result, err)
			}
		}
		other := selected.Snapshot.UIDs[1]
		result, err := mailbox.Resync(t.Context(), &imapserver.QResyncSelect{
			UIDValidity: selected.Snapshot.Status.UIDValidity, ModSeq: clock, KnownUIDs: imap.UIDSetNum(other),
		}, nil)
		if err != nil || result != nil && !result.Vanished.IsEmpty() {
			t.Fatalf("KnownUIDs did not restrict history: %#v, %v", result, err)
		}
	})
}
