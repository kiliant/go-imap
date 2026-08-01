//go:build ignore

// Connect to an IMAP server, authenticate, and list subscribed mailboxes.
//
// Run against the interop matrix:
//
//	IMAP_ADDR=localhost:1143 IMAP_USER=user IMAP_PASS=secret go run ./examples/connect_auth_list.go ./examples/config.go
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

	if err := authenticate(ctx, client); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	mailboxes, err := client.List("", "*", &imapclient.ListOptions{
		SelectionOptions: []imapclient.ListSelectOption{
			imapclient.ListSelectSubscribed,
		},
	}).Wait(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	for _, m := range mailboxes {
		fmt.Println(m.Mailbox)
	}
}
