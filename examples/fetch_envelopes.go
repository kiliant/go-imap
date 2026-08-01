//go:build ignore

// Fetch envelopes of the ten most recent messages in INBOX.
//
// Run:
//
//	IMAP_ADDR=localhost:1143 IMAP_USER=user IMAP_PASS=secret go run ./examples/fetch_envelopes.go ./examples/config.go
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/kiliant/go-imap"
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

	status, err := client.Select("INBOX", nil).Wait(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if status.NumMessages == 0 {
		return
	}

	start := imap.SeqNum(1)
	if status.NumMessages > 10 {
		start = imap.SeqNum(status.NumMessages - 9)
	}
	set := imap.SeqSetRange(start, imap.SeqNum(status.NumMessages))

	cmd := client.Fetch(set, imap.FetchItemEnvelope, imap.FetchItemUID)
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	for _, item := range data.Items {
			for _, v := range item {
				env, ok := v.(*imap.FetchDataEnvelope)
				if !ok {
					continue
				}
				var uid imap.UID
				if uids, ok := data.Items[imap.FetchDataKey("UID")]; ok && len(uids) > 0 {
					uid = imap.UID(uids[0].(imap.FetchDataUID))
				}
				fmt.Printf("uid=%d subject=%q\n", uid, env.Envelope.Subject)
			}
		}
	}
}
