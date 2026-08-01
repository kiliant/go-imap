//go:build ignore

// Authenticate with OAUTHBEARER or XOAUTH2 using a bearer token from the environment.
//
// Gmail and Outlook accept XOAUTH2; Stalwart accepts OAUTHBEARER. Set IMAP_TOKEN
// to a valid access token and optionally IMAP_OAUTH_MECH (defaults to OAUTHBEARER).
//
// Run:
//
//	IMAP_ADDR=localhost:143 IMAP_USER=user@example.com IMAP_TOKEN=ya29... \
//	  IMAP_STARTTLS=1 go run ./examples/oauth2.go ./examples/config.go
package main

import (
	"fmt"
	"os"

	"github.com/kiliant/go-imap/imapclient"
)

func main() {
	ctx, cancel := exampleContext()
	defer cancel()

	client, err := dialExample(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer client.Close()

	user := mustEnv("IMAP_USER")
	token := mustEnv("IMAP_TOKEN")
	mech := os.Getenv("IMAP_OAUTH_MECH")
	if mech == "" {
		mech = "OAUTHBEARER"
	}

	if err := client.Authenticate(ctx, user, "", &imapclient.AuthenticateOptions{
		Mechanism: mech,
		Token:     token,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Println("authenticated with", mech)
}
