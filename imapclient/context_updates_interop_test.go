//go:build interop

package imapclient_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

// Live coverage is deliberately separate from the scripted routing catalogue:
// a supporting server must actually send an update caused by an expunge.
func TestContextUpdateRemovalInterop(t *testing.T) {
	for _, sorted := range []bool{false, true} {
		t.Run(fmt.Sprintf("SORT=%t", sorted), func(t *testing.T) {
			t26ForEachServer(t, func(t *testing.T, ctx context.Context, server *harness.Server, caps map[string]bool, setup *imapclient.Client) {
				capability := "CONTEXT=SEARCH"
				if sorted {
					capability = "CONTEXT=SORT"
				}
				harness.RequireCapabilities(t, caps, capability)
				mailbox := t08Mailbox(t, ctx, server, setup, "context-update")
				t08Append(t, ctx, setup, mailbox, "context-message")
				events := make(chan *imap.ContextUpdate, 16)
				watcher, err := interopDial(ctx, server, &imapclient.Options{AllowInsecureAuth: true, UnilateralData: &imapclient.UnilateralDataHandler{
					ContextUpdate: func(data *imap.ContextUpdate) {
						select {
						case events <- data:
						default:
							t.Error("context event buffer overflow")
						}
					},
				}})
				if err != nil {
					t.Fatal(err)
				}
				defer watcher.Close()
				if err := watcher.Login(ctx, authInteropUsername, authInteropPassword, nil); err != nil {
					t.Fatal(err)
				}
				t26Select(t, ctx, server, watcher, mailbox)
				returns := []imapclient.SearchReturnOption{imapclient.SearchReturnAll, imapclient.SearchReturnCount, imapclient.SearchReturnUpdate}
				var initial *imapclient.ESearchData
				if sorted {
					initial, err = watcher.SortExtendedUID(ctx, []imap.SortKeySpec{{Key: imap.SortKeyDate}}, imap.SearchAll, &imapclient.ESortOptions{ReturnOptions: returns})
				} else {
					initial, err = watcher.SearchExtendedUID(imap.SearchAll, &imapclient.ESearchOptions{ReturnOptions: returns}).Wait(ctx)
				}
				if err != nil {
					t08Fail(t, server, watcher, "register UPDATE", err)
				}
				if initial.ContextTag == "" || !initial.HasAll || !initial.HasCount || initial.Count != 1 {
					t.Fatalf("initial context = %#v", initial)
				}
				if err := watcher.StoreUID(initial.AllUIDs, []imap.Flag{imap.FlagDeleted}, &imapclient.StoreOptions{Op: imapclient.StoreFlagsAdd, Silent: true}).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				if err := watcher.Expunge(nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				deadline := time.NewTimer(15 * time.Second)
				defer deadline.Stop()
				poll := time.NewTicker(200 * time.Millisecond)
				defer poll.Stop()
				for {
					select {
					case data := <-events:
						if data.Tag != initial.ContextTag || !data.UID {
							t.Fatalf("update correlation/mode = %#v", data)
						}
						for _, item := range data.Items {
							for _, change := range item.Changes {
								if item.Key == imap.ESearchReturnKeyRemoveFrom && change.UIDs.Equal(initial.AllUIDs) {
									if err := watcher.CancelUpdate(ctx, []string{initial.ContextTag}, nil); err != nil {
										t.Fatal(err)
									}
									return
								}
							}
						}
					case <-poll.C:
						if err := watcher.Noop(nil).Wait(ctx); err != nil {
							t08Fail(t, server, watcher, "poll UPDATE", err)
						}
					case <-deadline.C:
						t.Fatal("no REMOVEFROM for expunged context result within 15s")
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
			})
		})
	}
}
