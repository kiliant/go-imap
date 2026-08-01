package imapclient_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

func ExampleDial_connectAndList() {
	ctx := context.Background()
	client, err := imapclient.DialTLS(ctx, "imap.example.com:993", nil)
	if err != nil {
		panic(err)
	}
	defer client.Close()

	if err := client.Authenticate(ctx, "user", "pass", nil); err != nil {
		panic(err)
	}

	mailboxes, err := client.List("", "*", nil).Wait(ctx)
	if err != nil {
		panic(err)
	}
	for _, m := range mailboxes {
		fmt.Println(m.Mailbox)
	}
}

func ExampleClient_Fetch_envelopes() {
	ctx := context.Background()
	var client *imapclient.Client // obtained from Dial

	status, err := client.Select("INBOX", nil).Wait(ctx)
	if err != nil {
		panic(err)
	}
	set := imap.SeqSetRange(imap.SeqNum(status.NumMessages), imap.SeqNum(status.NumMessages))
	cmd := client.Fetch(set, nil, imap.FetchItemEnvelope, imap.FetchItemUID)
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}
		for _, values := range data.Items {
			for _, v := range values {
				if env, ok := v.(*imap.FetchDataEnvelope); ok {
					fmt.Println(env.Envelope.Subject)
				}
			}
		}
	}
}

func ExampleClient_Fetch_streamAttachment() {
	ctx := context.Background()
	var client *imapclient.Client

	section := &imap.FetchItemBodySection{Part: []int{2}, Peek: true}
	cmd := client.Fetch(imap.SeqSetNum(1), nil, section)
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			panic(err)
		}
		for _, values := range data.Items {
			for _, v := range values {
				body, ok := v.(*imap.FetchDataBodySection)
				if !ok {
					continue
				}
				_, _ = io.Copy(io.Discard, body.Literal)
			}
		}
	}
}

func ExampleClient_Idle() {
	ctx := context.Background()
	var client *imapclient.Client

	idle := client.Idle(nil)
	if err := idle.Wait(ctx); err != nil {
		panic(err)
	}
}

func ExampleClient_SelectSync() {
	ctx := context.Background()
	var client *imapclient.Client

	if _, err := client.Enable(nil, "CONDSTORE", "QRESYNC").Wait(ctx); err != nil {
		panic(err)
	}
	status, err := client.SelectSync("INBOX", &imapclient.SyncSelectOptions{
		CondStore: true,
		QResync: &imapclient.QResyncOptions{
			UIDValidity: 1,
			ModSeq:      42,
		},
	}).Wait(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println(status.Status.HighestModSeq)
}

func ExampleClient_Append() {
	ctx := context.Background()
	var client *imapclient.Client

	raw := "From: a@example.com\r\nTo: b@example.com\r\n\r\nHi\r\n"
	when := time.Now()
	data, err := client.Append(ctx, "Drafts", &imapclient.AppendOptions{
		Flags:        []imap.Flag{imap.FlagSeen},
		InternalDate: &when,
	}, int64(len(raw)), strings.NewReader(raw)).Wait(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println(data.UID)
}

func ExampleClient_Authenticate_oauth2() {
	ctx := context.Background()
	var client *imapclient.Client

	err := client.Authenticate(ctx, "user@example.com", "", &imapclient.AuthenticateOptions{
		Mechanism: "OAUTHBEARER",
		Token:     "access-token",
	})
	if err != nil {
		panic(err)
	}
}

func ExampleClient_Search_unicode() {
	ctx := context.Background()
	var client *imapclient.Client

	criteria := imap.SearchString{Key: imap.SearchKeySubject, Value: "旅行"}
	nums, err := client.Search(criteria, &imapclient.SearchOptions{Charset: "UTF-8"}).All(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println(len(nums))
}

// ExampleAppendMessage demonstrates typed append payload construction.
func ExampleAppendMessage() {
	msg := imapclient.AppendMessage{
		Flags:   []imap.Flag{imap.FlagSeen},
		Size:    4,
		Literal: bytes.NewReader([]byte("test")),
	}
	fmt.Println(msg.Size)
	// Output: 4
}
