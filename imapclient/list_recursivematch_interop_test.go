//go:build interop

package imapclient_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

// listChildInfoOmitted records the LIST-EXTENDED servers observed answering
// LIST (SUBSCRIBED RECURSIVEMATCH) without the CHILDINFO item RFC 5258
// section 3.5 describes. A server here is recorded, not failed.
var listChildInfoOmitted = map[string]bool{}

// TestListRecursiveMatchInterop runs RFC 5258 section 3.5's scenario: a child
// is subscribed and its parent is not, and LIST (SUBSCRIBED RECURSIVEMATCH)
// returns the parent with CHILDINFO ("SUBSCRIBED").
func TestListRecursiveMatchInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			client := dialMailboxInteropClient(t, ctx, server)
			harness.RequireCapabilities(t, client.Capabilities(), "LIST-EXTENDED")

			root, err := client.List("", "", nil).Wait(ctx)
			if err != nil || len(root) != 1 || root[0].Delimiter == 0 {
				failMailboxInterop(t, server, client, "LIST \"\" \"\" for the hierarchy delimiter", err)
			}
			parent := harness.UniqueMailbox("recursivematch")
			child := parent + string(root[0].Delimiter) + "child"
			for _, mailbox := range []string{parent, child} {
				if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
					failMailboxInterop(t, server, client, "CREATE "+mailbox, err)
				}
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = client.Unsubscribe(child, nil).Wait(ctx)
				_ = client.Delete(child, nil).Wait(ctx)
				_ = client.Delete(parent, nil).Wait(ctx)
			})
			if err := client.Subscribe(child, nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "SUBSCRIBE "+child, err)
			}

			data, err := client.List("", parent, &imapclient.ListOptions{
				SelectionOptions: []imapclient.ListSelectOption{imapclient.ListSelectSubscribed, imapclient.ListSelectRecursiveMatch},
			}).Wait(ctx)
			if err != nil {
				failMailboxInterop(t, server, client, "LIST (SUBSCRIBED RECURSIVEMATCH)", err)
			}
			var got *imapclient.ListData
			for _, item := range data {
				if item.Mailbox == parent {
					got = item
				}
			}
			omitted := got == nil || !slices.Contains(got.ChildInfo, "SUBSCRIBED")
			switch {
			case omitted && !listChildInfoOmitted[server.Profile.Name]:
				t.Errorf("server did not return the parent with CHILDINFO (\"SUBSCRIBED\"), an omission the matrix has not recorded: %#v", data)
			case omitted:
				t.Logf("recorded omission: no CHILDINFO for the parent of a subscribed child")
			case listChildInfoOmitted[server.Profile.Name]:
				t.Logf("recorded omission not observed: server returned %#v", got)
			}
			if got != nil {
				for _, item := range got.Extended {
					if item.Tag != "CHILDINFO" && item.Tag != "OLDNAME" {
						t.Logf("server sent an extended data item this library does not decode: %#v", item)
					}
				}
			}
		})
	}
}
