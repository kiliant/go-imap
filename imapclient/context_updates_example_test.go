package imapclient_test

import (
	"fmt"

	"github.com/kiliant/go-imap"
)

func ExampleContextUpdate() {
	// Install a callback through imapclient.Options.UnilateralData.ContextUpdate.
	// Handle or enqueue changes without blocking the reader. The callback may
	// run before the initial SearchExtended/SortExtended result returns.
	handle := func(update *imap.ContextUpdate) {
		for _, item := range update.Items {
			for _, change := range item.Changes {
				fmt.Println(update.Tag, item.Key, change.Position, change.UIDs)
			}
		}
	}
	handle(&imap.ContextUpdate{Tag: "A0002", UID: true, Items: []imap.ContextUpdateItem{{
		Key: imap.ESearchReturnKeyAddTo, Value: "(0 7:9)",
		Changes: []imap.ContextUpdateChange{{Position: 0, UIDs: imap.UIDSetRange(7, 9)}},
	}}})
	// To stop updates outside the callback:
	// err := client.CancelUpdate(ctx, []string{result.ContextTag}, nil)
	// For SearchExtended and MultiSearch, command.Tag() is also available.
	// Output: A0002 ADDTO 0 7:9
}
