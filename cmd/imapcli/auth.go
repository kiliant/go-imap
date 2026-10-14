package main

import (
	"context"

	"github.com/kiliant/go-imap/imapclient"
)

// authenticate logs in with a password, choosing only mechanisms that take
// one.
//
// imapclient's default selection ranks OAUTHBEARER and XOAUTH2 above PLAIN and
// sends the password as the bearer token when no token is given. A server that
// offers both — Stalwart does by default — then rejects a correct password.
// CredentialPassword keeps the choice to password mechanisms. SCRAM still wins
// when it is offered, and the library picks the variant, so channel binding is
// negotiated the way the library intends.
func authenticate(ctx context.Context, client *imapclient.Client, user, password string) error {
	caps := client.Capabilities()
	if !hasAuthMechanism(caps) {
		// Some servers advertise mechanisms only after a CAPABILITY command.
		if err := client.Capability(ctx, nil); err != nil {
			return err
		}
		caps = client.Capabilities()
	}
	if !offersPasswordMechanism(caps) {
		// With no mechanism that takes a password, Authenticate would refuse
		// before sending anything, and LOGIN is the fallback imapclient
		// documents for that case. Login itself refuses under LOGINDISABLED.
		return client.Login(ctx, user, password, nil)
	}
	return client.Authenticate(ctx, user, password, &imapclient.AuthenticateOptions{
		Credential: imapclient.CredentialPassword,
	})
}

// passwordMechanisms are the SASL mechanisms imapclient selects with
// CredentialPassword, as its documentation lists them.
var passwordMechanisms = []string{
	"SCRAM-SHA-256-PLUS", "SCRAM-SHA-256", "SCRAM-SHA-1-PLUS", "SCRAM-SHA-1",
	"CRAM-MD5", "PLAIN", "LOGIN",
}

func offersPasswordMechanism(caps map[string]bool) bool {
	for _, m := range passwordMechanisms {
		if caps["AUTH="+m] {
			return true
		}
	}
	return false
}

func hasAuthMechanism(caps map[string]bool) bool {
	for name := range caps {
		if len(name) > len("AUTH=") && name[:len("AUTH=")] == "AUTH=" {
			return true
		}
	}
	return false
}
