//go:build ignore

// Append a message with flags and an internal date (RFC 3501 APPEND).
//
// Run:
//
//	IMAP_ADDR=localhost:1143 IMAP_USER=user IMAP_PASS=secret \
//	  go run ./examples/append.go ./examples/config.go Drafts
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

func main() {
	mailbox := "INBOX"
	if len(os.Args) > 1 {
		mailbox = os.Args[1]
	}

	raw := strings.Join([]string{
		"From: sender@example.com",
		"To: recipient@example.com",
		"Subject: appended by go-imap example",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"Hello from examples/append.go",
		"",
	}, "\r\n")

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

	when := time.Now().UTC().Truncate(time.Second)
	data, err := client.Append(ctx, mailbox, &imapclient.AppendOptions{
		Flags:        []imap.Flag{imap.FlagSeen},
		InternalDate: &when,
	}, int64(len(raw)), bytes.NewReader([]byte(raw))).Wait(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("appended uidvalidity=%d uid=%d\n", data.UIDValidity, data.UID)
}
