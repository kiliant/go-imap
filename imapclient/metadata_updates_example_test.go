package imapclient_test

import (
	"context"
	"fmt"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

func ExampleUnilateralDataHandler_metadata() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	changes := make(chan *imap.MetadataChange, 64)
	client, err := imapclient.DialTLS(ctx, "imap.example.com:993", &imapclient.Options{
		UnilateralData: &imapclient.UnilateralDataHandler{
			Metadata: func(change *imap.MetadataChange) {
				// Never block the reader goroutine. If this buffer fills,
				// the application must refresh its metadata cache.
				select {
				case changes <- change:
				default:
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
	// Installing a handler does not subscribe. A server supporting unsolicited
	// METADATA accepts this ENABLE; alternatively use NOTIFY metadata events.
	enabled, err := client.Enable(nil, "METADATA").Wait(ctx)
	if err != nil {
		panic(err)
	}
	if len(enabled) == 0 {
		return // this server does not offer enabled METADATA notifications
	}
	// Refresh outside the callback. A names-only change never tells us whether
	// the current value is unset, empty or present; GETMETADATA does.
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	for {
		select {
		case change := <-changes:
			data, err := client.GetMetadata(ctx, change.Mailbox, change.Entries, nil)
			if err != nil {
				panic(err)
			}
			fmt.Println(data.Mailbox, data.Entries)
			return
		case <-poll.C:
			// Some servers queue notifications until the next command. An
			// application can use its ordinary command loop or IDLE instead.
			if err := client.Noop(nil).Wait(ctx); err != nil {
				panic(err)
			}
		case <-ctx.Done():
			return
		}
	}
}
