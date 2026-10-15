//go:build interop

package imapclient_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

// TestAuthenticateCredentialInterop logs in through automatic mechanism
// selection, restricted by AuthenticateOptions.CredentialKind. Stalwart is
// the case that motivated it: it advertises OAUTHBEARER beside PLAIN, so the
// unrestricted default sends the password as a bearer token and is refused.
func TestAuthenticateCredentialInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			harness.AssertProfile(t, server.Profile, harness.CapabilitiesFor(server.Profile.Name))
			if len(authCasesFor(server.Profile.Name)) == 0 {
				t.Skip("no T04 authentication account is configured for this profile")
			}
			t.Run("password", func(t *testing.T) {
				testCredentialInterop(t, server, imapclient.CredentialPassword)
			})
			t.Run("bearer-token", func(t *testing.T) {
				if server.Profile.Name != "stalwart" {
					t.Skip("only the Stalwart profile issues OAuth tokens")
				}
				testCredentialInterop(t, server, imapclient.CredentialBearerToken)
			})
		})
	}
}

func testCredentialInterop(t *testing.T, server *harness.Server, kind imapclient.CredentialKind) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	trace := new(authInteropTrace)
	// The harness listeners are cleartext, and PLAIN is a password mechanism
	// selection may land on. This is an explicit test-only opt-in.
	client, err := imapclient.Dial(ctx, server.Address, &imapclient.Options{Trace: trace.Add, AllowInsecureAuth: true})
	if err != nil {
		server.LogDiagnostics(context.Background(), t, trace)
		t.Fatal(err)
	}
	defer client.Close()

	token := ""
	if kind == imapclient.CredentialBearerToken {
		harness.RequireCapabilities(t, client.Capabilities(), "AUTH=OAUTHBEARER")
		if token, err = stalwartOAuthToken(ctx, server); err != nil {
			server.LogDiagnostics(context.Background(), t, trace)
			t.Fatal(err)
		}
	}

	err = client.Authenticate(ctx, authInteropUsername, authInteropPassword, &imapclient.AuthenticateOptions{
		CredentialKind: kind,
		Token:          token,
	})
	if err == nil && client.State() != imapclient.StateAuthenticated {
		err = fmt.Errorf("state after %s authentication = %s, want authenticated", kind, client.State())
	}
	if err == nil {
		err = client.Noop(nil).Wait(ctx)
	}
	if err == nil {
		err = client.Logout(ctx, nil)
	}
	if err != nil {
		server.LogDiagnostics(context.Background(), t, trace)
		t.Fatal(err)
	}
}
