//go:build interop

package smoke

import (
	"context"
	"testing"
	"time"

	"github.com/kiliant/go-imap/interop/harness"
)

func TestAuthenticatedSmoke(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			trace := new(harness.Trace)
			session, err := harness.DialSession(ctx, server.Address, trace)
			if err == nil {
				defer session.Close()
				err = session.Login(ctx)
			}
			if err == nil {
				err = session.Noop(ctx)
			}
			if err == nil {
				err = session.Select(ctx, "INBOX")
			}
			if err != nil {
				server.DumpDiagnostics(context.Background(), t.Output(), trace)
				t.Fatal(err)
			}
		})
	}
}

func TestCapabilityProfiles(t *testing.T) {
	for _, server := range harness.RunningServers() {
		harness.AssertProfile(t, server.Profile, harness.CapabilitiesFor(server.Profile.Name))
	}
}
