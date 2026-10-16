//go:build interop

package imapclient_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

func TestMetadataChangeInterop(t *testing.T) {
	for _, subscription := range []string{"ENABLE", "NOTIFY"} {
		t.Run(subscription, func(t *testing.T) {
			t26ForEachServer(t, func(t *testing.T, ctx context.Context, server *harness.Server, caps map[string]bool, writer *imapclient.Client) {
				harness.RequireCapabilities(t, caps, "METADATA", subscription)
				mailbox := t08Mailbox(t, ctx, server, writer, "metadata-change")
				events := make(chan *imap.MetadataChange, 16)
				watcher, err := interopDial(ctx, server, &imapclient.Options{AllowInsecureAuth: true, UnilateralData: &imapclient.UnilateralDataHandler{
					Metadata: func(change *imap.MetadataChange) {
						select {
						case events <- change:
						default:
							t.Error("metadata event buffer overflow")
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
				if subscription == "ENABLE" {
					enabled, err := watcher.Enable(nil, "METADATA").Wait(ctx)
					if err != nil {
						t08Fail(t, server, watcher, "ENABLE METADATA", err)
					}
					accepted := false
					for _, capability := range enabled {
						accepted = accepted || strings.EqualFold(capability, "METADATA")
					}
					if !accepted {
						t.Skip("server does not support enabling optional METADATA notifications")
					}
				}
				t26Select(t, ctx, server, watcher, mailbox)
				if subscription == "NOTIFY" {
					if err := watcher.Notify(ctx, []imapclient.NotifyFilter{{
						Specifier: imapclient.NotifySelected,
						Events:    []imapclient.NotifyEventName{imapclient.NotifyEventMailboxMetadataChange},
					}}, nil); err != nil {
						var protocol *imap.Error
						if errors.As(err, &protocol) && protocol.Code == imap.CodeBadEvent &&
							!strings.Contains(strings.ToUpper(protocol.CodeArgs), "MAILBOXMETADATACHANGE") {
							t.Skipf("server does not support MailboxMetadataChange: %v", err)
						}
						t08Fail(t, server, watcher, "NOTIFY MailboxMetadataChange", err)
					}
				}
				entry := imap.MetadataEntryName("/private/comment")
				if err := writer.SetMetadata(ctx, mailbox, []imap.MetadataEntry{{Name: entry, Value: imapclient.MetadataString("t30 notification")}}, nil); err != nil {
					t08Fail(t, server, writer, "SETMETADATA", err)
				}
				deadline := time.NewTimer(15 * time.Second)
				defer deadline.Stop()
				poll := time.NewTicker(200 * time.Millisecond)
				defer poll.Stop()
				for {
					select {
					case change := <-events:
						if change.Mailbox != mailbox {
							continue
						}
						for _, name := range change.Entries {
							if name == entry {
								data, err := watcher.GetMetadata(ctx, mailbox, []imap.MetadataEntryName{entry}, nil)
								if err != nil || len(data.Entries) != 1 || data.Entries[0].Value == nil || *data.Entries[0].Value != "t30 notification" {
									t.Fatalf("refresh after notification = %#v, %v", data, err)
								}
								return
							}
						}
					case <-poll.C:
						if err := watcher.Noop(nil).Wait(ctx); err != nil {
							t08Fail(t, server, watcher, "poll metadata notification", err)
						}
					case <-deadline.C:
						t.Fatal("no names-only METADATA notification for changed entry within 15s")
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
			})
		})
	}
}
