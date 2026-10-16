package main

import (
	"context"
	"errors"

	"github.com/kiliant/go-imap"
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
	if !hasAuthMechanism(client.Capabilities()) {
		// Some servers advertise mechanisms only after a CAPABILITY command.
		if err := client.Capability(ctx, nil); err != nil {
			return err
		}
	}
	err := client.Authenticate(ctx, user, password, &imapclient.AuthenticateOptions{
		CredentialKind: imapclient.CredentialPassword,
	})
	var protocol *imap.Error
	if errors.As(err, &protocol) && protocol.Type == imap.ErrorTypeProtocol && protocol.Code == imapclient.CodeNoCompatibleAuthMechanism {
		// The library confirms this request wrote nothing. Preserve every
		// other failure, including a server refusal with the same code token.
		return client.Login(ctx, user, password, nil)
	}
	return err
}

func hasAuthMechanism(caps map[string]bool) bool {
	for name := range caps {
		if len(name) > len("AUTH=") && name[:len("AUTH=")] == "AUTH=" {
			return true
		}
	}
	return false
}
