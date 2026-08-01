//go:build interop

package imapclient_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

// TestMailboxLifecycle exercises the T05 commands against the two M1 server
// implementations. The mailbox name deliberately contains non-ASCII text so
// this also verifies the production client's modified UTF-7 handling.
func TestMailboxLifecycle(t *testing.T) {
	for _, server := range mailboxInteropServers(t) {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			client := dialMailboxInteropClient(t, ctx, server)
			mailbox := harness.UniqueMailbox("mailbox-lifecycle") + "-旅行"
			renamed := harness.UniqueMailbox("mailbox-renamed") + "-旅行"

			if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "CREATE", err)
			}
			if data, err := client.List("", mailbox, nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "LIST created mailbox", err)
			} else if !containsMailbox(data, mailbox) {
				failMailboxInterop(t, server, client, "LIST did not return created mailbox", nil)
			}

			if err := client.Subscribe(mailbox, nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "SUBSCRIBE", err)
			}
			if data, err := client.Lsub("", mailbox, nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "LSUB", err)
			} else if !containsMailbox(data, mailbox) {
				failMailboxInterop(t, server, client, "LSUB did not return subscribed mailbox", nil)
			}

			status, err := client.Status(mailbox, &imapclient.StatusOptions{Items: []imap.StatusItem{
				imap.StatusItemMessages,
				imap.StatusItemUIDNext,
				imap.StatusItemUIDValidity,
				imap.StatusItemUnseen,
				imap.StatusItemRecent,
			}}).Wait(ctx)
			if err != nil {
				failMailboxInterop(t, server, client, "STATUS", err)
			}
			if status.Mailbox != mailbox || status.UIDValidity == 0 {
				failMailboxInterop(t, server, client, "STATUS returned incomplete mailbox data", nil)
			}

			selected, err := client.Select(mailbox, nil).Wait(ctx)
			if err != nil {
				failMailboxInterop(t, server, client, "SELECT", err)
			}
			if selected.Mailbox != mailbox || selected.UIDValidity == 0 || selected.ReadOnly {
				failMailboxInterop(t, server, client, "SELECT returned incomplete mailbox data", nil)
			}
			if err := client.Check(nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "CHECK", err)
			}
			leaveMailboxInterop(t, ctx, server, client)

			if client.Capabilities()["NAMESPACE"] {
				namespace, err := client.Namespace(nil).Wait(ctx)
				if err != nil {
					failMailboxInterop(t, server, client, "NAMESPACE", err)
				}
				if len(namespace.Personal) == 0 {
					failMailboxInterop(t, server, client, "NAMESPACE returned no personal namespace", nil)
				}
			}

			if err := client.Rename(mailbox, renamed, nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "RENAME", err)
			}
			if data, err := client.List("", renamed, nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "LIST renamed mailbox", err)
			} else if !containsMailbox(data, renamed) {
				failMailboxInterop(t, server, client, "LIST did not return renamed mailbox", nil)
			}
			if err := client.Unsubscribe(renamed, nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "UNSUBSCRIBE", err)
			}
			if err := client.Delete(renamed, nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "DELETE", err)
			}

			if err := client.Logout(ctx, nil); err != nil {
				failMailboxInterop(t, server, client, "LOGOUT", err)
			}
		})
	}
}

// TestMailboxCapabilityCommands verifies NAMESPACE and UNSELECT against every
// server that advertises them. Dovecot and Stalwart both exercise these paths
// in the native matrix; capability-based skips remain correct for GreenMail.
func TestMailboxCapabilityCommands(t *testing.T) {
	for _, server := range harness.RunningServers() {
		caps := harness.CapabilitiesFor(server.Profile.Name)
		if !caps["NAMESPACE"] && !caps["UNSELECT"] {
			continue
		}
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			client := dialMailboxInteropClient(t, ctx, server)

			if caps["NAMESPACE"] {
				namespace, err := client.Namespace(nil).Wait(ctx)
				if err != nil {
					failMailboxInterop(t, server, client, "NAMESPACE", err)
				}
				if len(namespace.Personal) == 0 {
					failMailboxInterop(t, server, client, "NAMESPACE returned no personal namespace", nil)
				}
			}
			if caps["UNSELECT"] {
				if _, err := client.Select("INBOX", nil).Wait(ctx); err != nil {
					failMailboxInterop(t, server, client, "SELECT before UNSELECT", err)
				}
				if err := client.Unselect(nil).Wait(ctx); err != nil {
					failMailboxInterop(t, server, client, "UNSELECT", err)
				}
			}
			if err := client.Logout(ctx, nil); err != nil {
				failMailboxInterop(t, server, client, "LOGOUT", err)
			}
		})
	}
}

// TestMailboxUIDValidityChange verifies the cache-invalidation signal using a
// real delete/recreate cycle, rather than a scripted response.
func TestMailboxUIDValidityChange(t *testing.T) {
	for _, server := range mailboxInteropServers(t) {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			client := dialMailboxInteropClient(t, ctx, server)
			mailbox := harness.UniqueMailbox("uidvalidity-recreate")
			if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "CREATE original mailbox", err)
			}
			original, err := client.Select(mailbox, nil).Wait(ctx)
			if err != nil {
				failMailboxInterop(t, server, client, "SELECT original mailbox", err)
			}
			if original.UIDValidity == 0 {
				failMailboxInterop(t, server, client, "SELECT returned zero UIDVALIDITY", nil)
			}
			leaveMailboxInterop(t, ctx, server, client)
			if err := client.Delete(mailbox, nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "DELETE original mailbox", err)
			}
			// GreenMail 2.1.9 derives a newly-created mailbox's UIDVALIDITY from
			// the current Unix second. Wait for its clock to advance so this is a
			// genuine recreation test rather than two folders created in the same
			// UIDVALIDITY tick.
			if server.Profile.Name == "greenmail" {
				if delay := time.Until(time.Unix(int64(original.UIDValidity)+1, 0)); delay > 0 {
					time.Sleep(delay)
				}
			}
			if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "CREATE recreated mailbox", err)
			}
			recreated, err := client.Examine(mailbox, nil).Wait(ctx)
			if err != nil {
				failMailboxInterop(t, server, client, "EXAMINE recreated mailbox", err)
			}
			if recreated.UIDValidity == original.UIDValidity || !recreated.UIDValidityChanged {
				t.Logf("UIDVALIDITY before recreation=%d after recreation=%d changed=%t", original.UIDValidity, recreated.UIDValidity, recreated.UIDValidityChanged)
				failMailboxInterop(t, server, client, "UIDVALIDITY change was not surfaced after delete/recreate", nil)
			}
			leaveMailboxInterop(t, ctx, server, client)
			if err := client.Delete(mailbox, nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "DELETE recreated mailbox", err)
			}
			if err := client.Logout(ctx, nil); err != nil {
				failMailboxInterop(t, server, client, "LOGOUT", err)
			}
		})
	}
}

func mailboxInteropServers(t *testing.T) []*harness.Server {
	t.Helper()
	var selected []*harness.Server
	for _, server := range harness.RunningServers() {
		if server.Profile.Name == "dovecot" || server.Profile.Name == "greenmail" {
			selected = append(selected, server)
		}
	}
	if len(selected) == 0 {
		t.Skip("T05 interop coverage requires the dovecot or greenmail profile")
	}
	return selected
}

func dialMailboxInteropClient(t *testing.T, ctx context.Context, server *harness.Server) *imapclient.Client {
	t.Helper()
	var trace bytes.Buffer
	client, err := imapclient.Dial(ctx, server.Address, &imapclient.Options{
		AllowInsecureAuth: true,
		DebugWriter:       &trace,
	})
	if err == nil {
		err = client.Login(ctx, authInteropUsername, authInteropPassword, nil)
	}
	if err != nil {
		if client != nil {
			_ = client.Close()
		}
		server.LogDiagnostics(context.Background(), t, &trace)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func leaveMailboxInterop(t *testing.T, ctx context.Context, server *harness.Server, client *imapclient.Client) {
	t.Helper()
	var err error
	if client.Capabilities()["UNSELECT"] {
		err = client.Unselect(nil).Wait(ctx)
	} else {
		err = client.CloseMailbox(nil).Wait(ctx)
	}
	if err != nil {
		failMailboxInterop(t, server, client, "leave selected mailbox", err)
	}
	if got := client.State(); got != imapclient.StateAuthenticated {
		failMailboxInterop(t, server, client, "leave selected mailbox did not return to authenticated state", nil)
	}
}

func containsMailbox(data []*imapclient.ListData, mailbox string) bool {
	for _, item := range data {
		if item.Mailbox == mailbox {
			return true
		}
	}
	return false
}

func failMailboxInterop(t *testing.T, server *harness.Server, client *imapclient.Client, operation string, err error) {
	t.Helper()
	if client != nil {
		_ = client.Close()
	}
	server.LogDiagnostics(context.Background(), t, nil)
	if err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
	t.Fatal(operation)
}
