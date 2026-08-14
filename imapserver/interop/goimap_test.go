//go:build interop

package interop_test

import (
	"context"
	"testing"
	"time"

	"github.com/kiliant/go-imap/interop/harness"
)

// TestProfileHolds is the row this entry contributes to the matrix: our server,
// seeded and asserted by the same code that produces Dovecot's and Stalwart's
// rows.
//
// Unlike the third-party rows, a failure here is neither a skip nor a container
// problem. harness.AssertProfile says so in its message, because the profile is
// marked FirstParty.
func TestProfileHolds(t *testing.T) {
	servers := harness.RunningServers()
	if len(servers) == 0 {
		t.Fatal("no server started; the in-process profile should always be available")
	}
	for _, server := range servers {
		t.Run(server.Profile.Name, func(t *testing.T) {
			advertised := harness.CapabilitiesFor(server.Profile.Name)
			harness.AssertProfile(t, server.Profile, advertised)
			t.Logf("advertised: %s", harness.FormatCapabilities(advertised))
		})
	}
}

// TestSeededMailboxesAreSelectable checks the fixture state actually landed.
//
// Seeding runs in TestMain against the same Seed the container profiles use, so
// this does not re-test seeding; it asserts that what Seed created is reachable
// afterwards. "T&AOs-st" is the interesting one — it is "Tëst" in modified
// UTF-7, and it catches a server that decodes a mailbox name on the way in and
// re-encodes it differently on the way out.
func TestSeededMailboxesAreSelectable(t *testing.T) {
	for _, server := range harness.RunningServers() {
		t.Run(server.Profile.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			trace := new(harness.Trace)
			session, err := harness.DialSession(ctx, server.Address, trace)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer session.Close()
			if err := session.Login(ctx); err != nil {
				t.Fatalf("login: %v", err)
			}
			for _, mailbox := range []string{"INBOX", "Archive", "Sent", "T&AOs-st"} {
				if err := session.Select(ctx, mailbox); err != nil {
					server.LogDiagnostics(ctx, t, trace)
					t.Fatalf("SELECT %q: %v", mailbox, err)
				}
			}
			if err := session.Noop(ctx); err != nil {
				server.LogDiagnostics(ctx, t, trace)
				t.Fatalf("NOOP: %v", err)
			}
		})
	}
}
