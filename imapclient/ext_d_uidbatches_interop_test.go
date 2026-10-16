//go:build interop

package imapclient_test

import (
	"context"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
	"testing"
)

func TestUIDBatchesInterop(t *testing.T) {
	t26ForEachServer(t, func(t *testing.T, ctx context.Context, server *harness.Server, caps map[string]bool, client *imapclient.Client) {
		harness.RequireCapabilities(t, caps, "UIDBATCHES")
		mailbox := t08Mailbox(t, ctx, server, client, "uidbatches")
		t08Append(t, ctx, client, mailbox, "batch one")
		t08Append(t, ctx, client, mailbox, "batch two")
		t26Select(t, ctx, server, client, mailbox)
		uids := t08AllUIDs(t, ctx, server, client)
		data, err := client.UIDBatches(ctx, 500, nil)
		if err != nil {
			t08Fail(t, server, client, "UIDBATCHES", err)
		}
		if len(uids) != 2 || len(data.Batches) != 1 {
			t.Fatalf("UIDs %v, batches %#v", uids, data)
		}
		r := data.Batches[0].Range
		for _, uid := range uids {
			if !(uid >= r.Start && uid <= r.Stop || uid >= r.Stop && uid <= r.Start) {
				t.Fatalf("batch %v misses UID %v", r, uid)
			}
		}
	})
}
