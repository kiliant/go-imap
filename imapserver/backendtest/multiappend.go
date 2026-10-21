package backendtest

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func runMultiAppend(t *testing.T, harness *Harness) {
	t.Run("multiappend-is-atomic", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireAllowedCapabilities(t, instance, session, "MULTIAPPEND")
		batch, ok := session.(imapserver.MultiAppendSession)
		if !ok {
			t.Skip("backend does not implement MultiAppendSession")
		}
		mailbox := populate(t, session, "multiappend")
		_, watching := authenticate(t, instance)
		defer closeSession(t, instance, watching)
		var mu sync.Mutex
		var batches []*imapserver.UpdateBatch
		notifications := make(chan struct{}, 8)
		selected := selectMailbox(t, watching, mailbox, &imapserver.Updater{PushFunc: func(batch *imapserver.UpdateBatch) error {
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
		for _, test := range []struct {
			name     string
			last     io.Reader
			size     int64
			hasSize  bool
			canceled bool
		}{
			{name: "empty", last: strings.NewReader("")},
			{name: "nil"},
			{name: "read-error", last: io.MultiReader(strings.NewReader("partial"), appendErrorReader{})},
			{name: "short", last: strings.NewReader("short"), size: 100, hasSize: true},
			{name: "cancel", last: strings.NewReader("valid"), canceled: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if test.canceled {
					cancel()
				}
				_, err := batch.MultiAppend(ctx, mailbox, []imapserver.AppendMessage{
					{Literal: strings.NewReader("Subject: first\r\n\r\nbody"), Flags: []imap.Flag{"batch-keyword"}},
					{Literal: test.last, Size: test.size, HasSize: test.hasSize},
				}, nil)
				if err == nil {
					t.Fatal("invalid batch succeeded")
				}
				after := selectMailbox(t, session, mailbox, discardUpdater())
				defer after.Mailbox.Unselect(context.Background(), nil)
				// A new selection can legitimately reassign Recent messages.
				after.Snapshot.Status.NumRecent = before.Status.NumRecent
				if !reflect.DeepEqual(before, after.Snapshot) {
					t.Fatalf("failed batch changed snapshot: before %#v, after %#v", before, after.Snapshot)
				}
				mu.Lock()
				count := len(batches)
				mu.Unlock()
				if count != 0 {
					t.Fatal("failed batch published updates")
				}
			})
		}
		const origin imapserver.ChangeToken = 579
		data, err := batch.MultiAppend(t.Context(), mailbox, []imapserver.AppendMessage{
			{Literal: strings.NewReader("Subject: batch one\r\n\r\none"), Flags: []imap.Flag{imap.FlagSeen}},
			{Literal: strings.NewReader("Subject: batch two\r\n\r\ntwo")},
		}, &imapserver.MultiAppendOptions{MutationOptions: imapserver.MutationOptions{Origin: origin}})
		if err != nil {
			t.Fatal(err)
		}
		after := selectMailbox(t, session, mailbox, discardUpdater())
		defer after.Mailbox.Unselect(context.Background(), nil)
		if len(after.Snapshot.UIDs) != len(before.UIDs)+2 {
			t.Fatal("successful batch did not add both messages")
		}
		got := waitForBatchCount(&mu, &batches, notifications, 1)
		if len(got) != 1 || got[0].Before != before.Revision || got[0].After != after.Snapshot.Revision || got[0].Origin != origin {
			t.Fatalf("atomic update chain = %#v", got)
		}
		added := after.Snapshot.UIDs[len(before.UIDs):]
		for _, uid := range added {
			if !batchAddsUID(got[0], uid) {
				t.Errorf("atomic update omits UID %d", uid)
			}
			if supportsCapability(instance, session, "UIDPLUS") && (data == nil || !data.HasUIDs || data.UIDValidity != before.Status.UIDValidity || !data.UIDs.Contains(uid)) {
				t.Errorf("APPENDUID omits UID %d: %#v", uid, data)
			}
		}
		// Reported data becomes APPENDUID whether or not UIDPLUS obliges it,
		// so it must name exactly the appended messages.
		if data != nil && data.HasUIDs {
			reported, ok := data.UIDs.Nums()
			if !ok || data.UIDValidity != before.Status.UIDValidity || !slices.Equal(reported, added) {
				t.Errorf("MultiAppend reported %#v, but appended UIDs %v under UIDVALIDITY %d", data, added, before.Status.UIDValidity)
			}
		}
	})
}

type appendErrorReader struct{}

func (appendErrorReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("injected literal read failure")
}
