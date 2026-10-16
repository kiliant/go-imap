package imapclient_test

import (
	"context"
	"fmt"
	"github.com/kiliant/go-imap/imapclient"
	"time"
)

func ExampleClient_UIDBatches() {
	// client has authenticated and selected a mailbox advertising UIDBATCHES.
	var client *imapclient.Client
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	data, err := client.UIDBatches(ctx, 1000, nil)
	if err != nil {
		return
	}
	for _, batch := range data.Batches {
		fmt.Printf("UID interval %d:%d\n", batch.Range.Start, batch.Range.Stop)
	}
	// Keep these boundaries for subsequent UID FETCH/SEARCH/STORE requests.
	// Resend only after a different selection or more than 500 additions or
	// expunges; the client does not track the RFC 10022 resend threshold.
}
