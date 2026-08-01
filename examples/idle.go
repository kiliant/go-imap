//go:build ignore

// Wait for new mail with IDLE (RFC 2177), falling back to NOOP polling when IDLE
// is not advertised.
//
// Run:
//
//	IMAP_ADDR=localhost:1143 IMAP_USER=user IMAP_PASS=secret go run ./examples/idle.go ./examples/config.go
package main

import (
	"fmt"
	"os"

	"github.com/kiliant/go-imap/imapclient"
)

func main() {
	ctx, cancel := exampleContext()
	defer cancel()

	client, err := dialExampleWithOptions(ctx, &imapclient.Options{
		UnilateralData: &imapclient.UnilateralDataHandler{
			Exists: func(count uint32) {
				fmt.Printf("mailbox now has %d messages\n", count)
			},
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer client.Close()

	if err := authenticate(ctx, client); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if _, err := client.Select("INBOX", nil).Wait(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	idle := client.Idle()
	if err := idle.Wait(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
