package imapclient_test

import (
	"context"
	"fmt"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

func ExampleClient_CancelUpdate() {
	ctx := context.Background()

	// The callback runs on the reader goroutine: hand the update to the
	// application without blocking, and never issue a command from here.
	updates := make(chan *imap.ContextUpdate, 64)
	client, err := imapclient.DialTLS(ctx, "imap.example.com:993", &imapclient.Options{
		UnilateralData: &imapclient.UnilateralDataHandler{
			ContextUpdate: func(update *imap.ContextUpdate) {
				select {
				case updates <- update:
				default: // full: resynchronise with a fresh search instead
				}
			},
		},
	})
	if err != nil {
		panic(err)
	}
	defer client.Close()
	if err := client.Authenticate(ctx, "user", "pass", nil); err != nil {
		panic(err)
	}
	if _, err := client.Select("INBOX", nil).Wait(ctx); err != nil {
		panic(err)
	}

	// RETURN (UPDATE) needs CONTEXT=SEARCH; the result names the context.
	result, err := client.SearchExtendedUID(imap.SearchUnseen, &imapclient.ESearchOptions{
		ReturnOptions: []imapclient.SearchReturnOption{imapclient.SearchReturnAll, imapclient.SearchReturnUpdate},
	}).Wait(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println("unseen now:", result.AllUIDs)

	// Apply items, and the changes within each, in the order they arrive.
	update := <-updates
	for _, item := range update.Items {
		switch item.Key {
		case imapclient.ESearchReturnKeyAddTo, imapclient.ESearchReturnKeyRemoveFrom:
			for _, change := range item.Changes {
				fmt.Println(item.Key, change.Position, change.UIDs)
			}
		}
	}

	if err := client.CancelUpdate(ctx, []string{result.ContextTag}, nil); err != nil {
		panic(err)
	}
}
