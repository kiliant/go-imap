package main

import (
	"context"

	"github.com/kiliant/go-imap/imapclient"
)

// authenticate logs in with a password, choosing only mechanisms that take
// one.
//
// imapclient's automatic choice ranks OAUTHBEARER and XOAUTH2 above PLAIN and
// sends the password as the bearer token when no token is given. A server that
// offers both — Stalwart does by default — then rejects a correct password.
// SCRAM still wins when it is offered, and the library picks the variant, so
// channel binding is negotiated the way the library intends.
func authenticate(ctx context.Context, client *imapclient.Client, user, password string) error {
	caps := client.Capabilities()
	if !hasAuthMechanism(caps) {
		// Some servers advertise mechanisms only after a CAPABILITY command.
		if err := client.Capability(ctx, nil); err != nil {
			return err
		}
		caps = client.Capabilities()
	}
	if !hasAuthMechanism(caps) {
		return client.Login(ctx, user, password, nil)
	}
	return client.Authenticate(ctx, user, password, passwordAuthOptions(caps))
}

// passwordAuthOptions returns nil when imapclient's own preference already
// lands on a password mechanism, and otherwise names the strongest one the
// server offers.
func passwordAuthOptions(caps map[string]bool) *imapclient.AuthenticateOptions {
	for _, m := range []string{"SCRAM-SHA-256-PLUS", "SCRAM-SHA-256", "SCRAM-SHA-1-PLUS", "SCRAM-SHA-1"} {
		if caps["AUTH="+m] {
			return nil
		}
	}
	for _, m := range []string{"CRAM-MD5", "PLAIN", "LOGIN"} {
		if caps["AUTH="+m] {
			return &imapclient.AuthenticateOptions{Mechanism: m}
		}
	}
	return nil
}

func hasAuthMechanism(caps map[string]bool) bool {
	for name := range caps {
		if len(name) > len("AUTH=") && name[:len("AUTH=")] == "AUTH=" {
			return true
		}
	}
	return false
}
