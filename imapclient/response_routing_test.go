package imapclient

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

func newRoutingTestClient(t *testing.T, greeting string, options *Options, serve func(*extAServer)) (*Client, context.Context) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		s := &extAServer{t: t, conn: serverConn, r: bufio.NewReader(serverConn)}
		s.reply(greeting)
		serve(s)
	}()
	c := NewClient(clientConn, options)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(func() {
		cancel()
		_ = c.Close()
		<-done
	})
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	return c, ctx
}

func TestRoutingPreservesMissingUIDIndicatorTolerance(t *testing.T) {
	c, ctx := newExtATestClient(t, esearchCapabilities, func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, _ := s.command()
		s.reply(`* ESEARCH (TAG "`+tag+`") ALL 2,10:11 COUNT 3`, tag+" OK searched")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.SearchExtendedUID(imap.SearchAll, nil).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !data.UID || !data.HasAll || data.All != nil || data.AllUIDs.String() != "2,10:11" {
		t.Fatalf("UID result without UID indicator = %#v", data)
	}
}

func TestStatusRoutesByMailbox(t *testing.T) {
	for _, test := range []struct{ mailbox, reply, event string }{
		{"INBOX", "inbox", "Archive"},
		{"Archive", "Archive", "archive"},
		{"INBOX/child", "INBOX/child", "inbox/child"},
	} {
		t.Run(test.mailbox, func(t *testing.T) {
			var events []*StatusData
			c, ctx := newRoutingTestClient(t, "* PREAUTH ready", &Options{UnilateralData: &UnilateralDataHandler{
				Status: func(data *StatusData) { events = append(events, data) },
			}}, func(s *extAServer) {
				tag, _ := s.command()
				s.reply("* STATUS "+test.event+" (MESSAGES 90 X-EVENT 7)",
					"* STATUS "+test.reply+" (MESSAGES 3)",
					"* STATUS "+test.event+" (MESSAGES 91)",
					"* STATUS "+test.reply+" (UIDNEXT 8)", tag+" OK done")
			})
			data, err := c.Status(test.mailbox, nil).Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if data.Mailbox != normalisedMailbox(test.reply) || data.NumMessages != 3 || data.UIDNext != 8 || data.Values["X-EVENT"] != nil {
				t.Fatalf("command includes another mailbox's data: %#v", data)
			}
			if len(events) != 2 || events[0].Mailbox != test.event || events[0].NumMessages != 90 || events[1].NumMessages != 91 {
				t.Fatalf("events = %#v, want two separate updates for %q", events, test.event)
			}
			if events[0].Values["X-EVENT"] != uint64(7) {
				t.Fatal("event lost its extension item")
			}
		})
	}
}

func TestPipelinedStatusRepliesCanArriveOutOfOrder(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH ready", func(s *extAServer) {
		first, _ := s.command()
		second, _ := s.command()
		s.reply("* STATUS Sent (MESSAGES 8)", second+" OK second",
			"* STATUS INBOX (MESSAGES 3)", first+" OK first")
	})
	first := c.Status("INBOX", nil)
	second := c.Status("Sent", nil)
	sent, err := second.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := first.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sent.Mailbox != "Sent" || sent.NumMessages != 8 || inbox.Mailbox != "INBOX" || inbox.NumMessages != 3 {
		t.Fatalf("misrouted status: Sent=%#v INBOX=%#v", sent, inbox)
	}
}

func TestStatusWithoutEventHandlerKeepsStreamAligned(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH ready", func(s *extAServer) {
		tag, _ := s.command()
		s.reply("* STATUS Other (MESSAGES 90)", "* STATUS INBOX (MESSAGES 3)", tag+" OK done")
		tag, _ = s.command()
		s.ok(tag)
	})
	data, err := c.Status("INBOX", nil).Wait(ctx)
	if err != nil || data.NumMessages != 3 {
		t.Fatalf("status = %#v, %v", data, err)
	}
	if err := c.Noop(nil).Wait(ctx); err != nil {
		t.Fatalf("response after declined event: %v", err)
	}
}

func TestLsubDoesNotClaimListEvents(t *testing.T) {
	var events []*ListData
	c, ctx := newRoutingTestClient(t, "* PREAUTH ready", &Options{UnilateralData: &UnilateralDataHandler{
		List: func(data *ListData) { events = append(events, data) },
	}}, func(s *extAServer) {
		tag, _ := s.command()
		s.reply(`* LIST () "." Renamed ("OLDNAME" ("Old"))`,
			`* LSUB () "." Subscribed`, tag+" OK done")
	})
	listed, err := c.Lsub("", "*", nil).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Mailbox != "Subscribed" {
		t.Fatalf("LSUB collected a LIST event: %#v", listed)
	}
	if len(events) != 1 || events[0].Mailbox != "Renamed" || events[0].OldName != "Old" {
		t.Fatalf("LIST event = %#v", events)
	}
}

func TestMetadataNotificationsDoNotBecomeGetResults(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 METADATA METADATA-SERVER] ready", func(s *extAServer) {
		tag, _ := s.command()
		s.reply(`* METADATA "" /shared/comment`,
			`* METADATA INBOX /private/comment /shared/comment`,
			`* METADATA Other (/shared/comment "foreign")`,
			`* METADATA INBOX (/private/comment NIL)`,
			`* METADATA INBOX (/shared/comment "")`, tag+" OK done")
		tag, _ = s.command()
		s.ok(tag)
	})
	data, err := c.GetMetadata(ctx, "INBOX", []MetadataEntryName{"/private/comment", "/shared/comment"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if data.Mailbox != "INBOX" || len(data.Entries) != 2 || data.Entries[0].Value != nil ||
		data.Entries[1].Value == nil || *data.Entries[1].Value != "" {
		t.Fatalf("GETMETADATA = %#v, want NIL and empty values only for INBOX", data)
	}
	if err := c.Noop(nil).Wait(ctx); err != nil {
		t.Fatalf("response after metadata notifications: %v", err)
	}
}

func TestListMetadataIgnoresNamesOnlyNotifications(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 LIST-EXTENDED LIST-METADATA] ready", func(s *extAServer) {
		tag, _ := s.command()
		s.reply(`* LIST () "/" INBOX`, `* METADATA INBOX /shared/comment`,
			`* METADATA INBOX (/shared/comment "value")`, tag+" OK done")
	})
	var metadata []*MailboxMetadata
	listed, err := c.ListMailboxesExt(ctx, "", "*", nil, &ListExtOptions{Metadata: &ListReturnMetadata{
		Entries: []MetadataEntryName{"/shared/comment"},
		Handler: func(data *MailboxMetadata) { metadata = append(metadata, data) },
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || len(metadata) != 1 || len(metadata[0].Entries) != 1 || metadata[0].Entries[0].Value == nil || *metadata[0].Entries[0].Value != "value" {
		t.Fatalf("LIST-METADATA = %#v, %#v", listed, metadata)
	}
}

// LIST cannot distinguish a NOTIFY event that looks like a command result.
// Keep the existing first-collector policy rather than inventing a local
// interpretation of the server's reference and wildcard rules.
func TestListKeepsAmbiguousResponses(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH ready", func(s *extAServer) {
		tag, _ := s.command()
		s.reply(`* LIST () "." Mailbox`, `* LIST () "." Mailbox`, tag+" OK done")
	})
	listed, err := c.List("", "*", nil).Wait(ctx)
	if err != nil || len(listed) != 2 || listed[0].Mailbox != "Mailbox" || listed[1].Mailbox != "Mailbox" {
		t.Fatalf("ambiguous LIST responses = %#v, %v", listed, err)
	}
}

func TestMetadataRoutingPreservesServerNamespace(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 METADATA METADATA-SERVER] ready", func(s *extAServer) {
		tag, _ := s.command()
		s.reply(`* METADATA "" /shared/comment`,
			`* METADATA INBOX (/shared/comment "mailbox")`,
			`* METADATA "" (/shared/comment "server")`, tag+" OK done")
	})
	data, err := c.GetMetadata(ctx, "", []MetadataEntryName{"/shared/comment"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if data.Mailbox != "" || len(data.Entries) != 1 || data.Entries[0].Value == nil || *data.Entries[0].Value != "server" {
		t.Fatalf("server metadata = %#v", data)
	}
}

func TestMetadataRoutingPreservesResponseLimits(t *testing.T) {
	for _, list := range []bool{false, true} {
		for _, overLimit := range []bool{false, true} {
			name := "GETMETADATA"
			if list {
				name = "LIST-METADATA"
			}
			if overLimit {
				name += "/over-limit"
			}
			t.Run(name, func(t *testing.T) {
				c, ctx := newRoutingTestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 METADATA LIST-EXTENDED LIST-METADATA] ready", &Options{MaxUntaggedResponses: 1}, func(s *extAServer) {
					tag, _ := s.command()
					if list {
						s.reply(`* LIST () "/" INBOX`)
					}
					s.reply(`* METADATA INBOX /shared/comment`, `* METADATA INBOX /private/comment`,
						`* METADATA INBOX (/shared/comment "value")`)
					if overLimit {
						s.reply(`* METADATA INBOX (/private/comment "second")`)
					}
					s.ok(tag)
				})
				var err error
				if list {
					_, err = c.ListMailboxesExt(ctx, "", "*", nil, &ListExtOptions{Metadata: &ListReturnMetadata{
						Entries: []MetadataEntryName{"/shared/comment"},
					}})
				} else {
					_, err = c.GetMetadata(ctx, "INBOX", []MetadataEntryName{"/shared/comment"}, nil)
				}
				if !overLimit {
					if err != nil {
						t.Fatalf("notifications counted as command results: %v", err)
					}
					return
				}
				var protocolErr *imap.Error
				if !errors.As(err, &protocolErr) || protocolErr.Err == nil || !strings.Contains(protocolErr.Err.Error(), "too many untagged responses") {
					t.Fatalf("response limit error = %v", err)
				}
			})
		}
	}
}

func TestUnclaimedDecodedResponsesKeepStreamAligned(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH ready", func(s *extAServer) {
		tag, _ := s.command()
		s.reply(`* ESEARCH (TAG "old") UID ADDTO (0 9)`,
			`* METADATA "" /shared/comment`, `* LSUB () "/" Old`,
			`* XLIST () "/" Old`, tag+" OK done")
		tag, _ = s.command()
		s.ok(tag)
	})
	for i := 0; i < 2; i++ {
		if err := c.Noop(nil).Wait(ctx); err != nil {
			t.Fatalf("NOOP %d after unclaimed responses: %v", i, err)
		}
	}
}
