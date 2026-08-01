//go:build ignore

// Search INBOX for a non-ASCII term using UTF-8 criteria (UTF8=ACCEPT, RFC 9755).
//
// Run:
//
//	IMAP_ADDR=localhost:1143 IMAP_USER=user IMAP_PASS=secret \
//	  go run ./examples/search_unicode.go ./examples/config.go 旅行
package main

import (
	"fmt"
	"os"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

func main() {
	term := "旅行"
	if len(os.Args) > 1 {
		term = os.Args[1]
	}

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
	if _, err := client.Select("INBOX", nil).Wait(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	criteria := imap.SearchString{
		Key:   imap.SearchKeySubject,
		Value: term,
	}
	nums, err := client.Search(criteria, &imapclient.SearchOptions{
		Charset: "UTF-8",
	}).All(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("matched %d messages for %q\n", len(nums), term)
}
