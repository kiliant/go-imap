//go:build interop

package imapclient_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

// TestCapabilityEnableInterop verifies the post-auth capability refresh on all
// native servers and negotiates rev2 where Stalwart advertises it. Dovecot and
// GreenMail remain rev1 sessions, exercising the same public capability API.
func TestCapabilityEnableInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			client := dialMailboxInteropClient(t, ctx, server)
			if err := client.Capability(ctx); err != nil {
				failMailboxInterop(t, server, client, "CAPABILITY refresh", err)
			}
			if server.Profile.Name == "stalwart" {
				enabled, err := client.Enable("IMAP4rev2").Wait(ctx)
				if err != nil {
					failMailboxInterop(t, server, client, "ENABLE IMAP4rev2", err)
				}
				if !containsCapability(enabled, "IMAP4REV2") || !client.EnabledCapabilities()["IMAP4REV2"] {
					failMailboxInterop(t, server, client, "server did not enable IMAP4rev2", nil)
				}
			}
			if err := client.Logout(ctx); err != nil {
				failMailboxInterop(t, server, client, "LOGOUT", err)
			}
		})
	}
}

// TestIdlePushInterop appends through a second authenticated connection and
// requires an EXISTS notification within one second. Servers without IDLE are
// skipped because Idle deliberately falls back to NOOP polling for them.
func TestIdlePushInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		// T07's rev1/rev2 acceptance matrix is Dovecot, GreenMail, and
		// Stalwart. Other native profiles advertise IDLE but do not all emit a
		// prompt cross-session EXISTS notification with their test defaults.
		if server.Profile.Name != "dovecot" && server.Profile.Name != "greenmail" && server.Profile.Name != "stalwart" {
			continue
		}
		caps := harness.CapabilitiesFor(server.Profile.Name)
		if !caps["IDLE"] {
			continue
		}
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			updates := make(chan uint32, 1)
			client := dialIdleInteropClient(t, ctx, server, updates)
			writer := dialMailboxInteropClient(t, ctx, server)
			defer writer.Close()
			if _, err := client.Select("INBOX", nil).Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "SELECT INBOX", err)
			}
			// SELECT's EXISTS is collected by SelectCommand, but drain defensively
			// so the assertion below can only be satisfied by the post-IDLE APPEND.
			drainExists(updates)
			idle := client.Idle() // Issue IDLE before the second connection mutates INBOX.
			if err := idle.WaitReady(ctx); err != nil {
				failMailboxInterop(t, server, client, "wait for IDLE continuation", err)
			}
			idleCtx, stopIdle := context.WithCancel(ctx)
			idleResult := make(chan error, 1)
			go func() { idleResult <- idle.Wait(idleCtx) }()

			message := "Subject: idle interop\r\n\r\nbody\r\n"
			if _, err := writer.Append(ctx, "INBOX", nil, int64(len(message)), strings.NewReader(message)).Wait(ctx); err != nil {
				stopIdle()
				failMailboxInterop(t, server, client, "APPEND through second connection", err)
			}
			select {
			case <-updates:
			case <-time.After(time.Second):
				stopIdle()
				failMailboxInterop(t, server, client, "IDLE did not receive EXISTS within one second", nil)
			}
			stopIdle()
			if err := <-idleResult; !errors.Is(err, context.Canceled) {
				failMailboxInterop(t, server, client, "ending IDLE", err)
			}
			if err := client.CloseMailbox().Wait(ctx); err != nil {
				failMailboxInterop(t, server, client, "CLOSE after IDLE", err)
			}
			if err := client.Logout(ctx); err != nil {
				failMailboxInterop(t, server, client, "LOGOUT", err)
			}
		})
	}
}

func dialIdleInteropClient(t *testing.T, ctx context.Context, server *harness.Server, updates chan<- uint32) *imapclient.Client {
	t.Helper()
	var trace bytes.Buffer
	client, err := imapclient.Dial(ctx, server.Address, &imapclient.Options{
		AllowInsecureAuth: true,
		DebugWriter:       &trace,
		UnilateralData: &imapclient.UnilateralDataHandler{
			Exists: func(uint32) { updates <- 0 },
		},
	})
	if err == nil {
		err = client.Login(ctx, authInteropUsername, authInteropPassword)
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

func containsCapability(capabilities []string, want string) bool {
	for _, capability := range capabilities {
		if strings.EqualFold(capability, want) {
			return true
		}
	}
	return false
}

func drainExists(updates <-chan uint32) {
	for {
		select {
		case <-updates:
		default:
			return
		}
	}
}
