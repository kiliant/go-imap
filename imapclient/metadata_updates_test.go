package imapclient

import (
	"errors"
	"reflect"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestMetadataChangesStaySeparateFromResults(t *testing.T) {
	for _, handler := range []string{"callback", "nil-callback", "nil-handlers"} {
		t.Run(handler, func(t *testing.T) {
			var changes []*MetadataChange
			options := &Options{MaxUntaggedResponses: 1}
			if handler != "nil-handlers" {
				options.UnilateralData = &UnilateralDataHandler{}
				if handler == "callback" {
					options.UnilateralData.Metadata = func(change *MetadataChange) { changes = append(changes, change) }
				}
			}
			c, ctx := newRoutingTestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 METADATA METADATA-SERVER LIST-EXTENDED LIST-METADATA] ready", options, func(s *extAServer) {
				tag, _ := s.command()
				s.reply(`* METADATA "" /shared/comment`,
					`* METADATA Other (/shared/comment "unsolicited value")`, tag+" OK fence")
				tag, _ = s.command()
				s.reply(`* METADATA "Entw&APw-rfe" /shared/comment /private/comment`,
					`* METADATA "" /private/comment`,
					`* METADATA Other (/shared/comment "foreign")`,
					`* METADATA "Entw&APw-rfe" (/private/comment NIL /shared/empty "" /shared/comment "value")`,
					`* METADATA "Entw&APw-rfe" /shared/comment`, tag+" OK metadata")
				tag, _ = s.command()
				s.reply(`* LIST () "/" INBOX`,
					`* METADATA inbox /shared/comment`,
					`* METADATA INBOX (/shared/comment "list value")`, tag+" OK listed")
				tag, _ = s.command()
				s.reply("* METADATA Other {15}\r\n/private/custom", tag+" OK fence")
			})
			if err := c.Noop(nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			data, err := c.GetMetadata(ctx, "Entwürfe", []MetadataEntryName{"/private/comment", "/shared/empty", "/shared/comment"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if data.Mailbox != "Entwürfe" || len(data.Entries) != 3 || data.Entries[0].Value != nil ||
				data.Entries[1].Value == nil || *data.Entries[1].Value != "" || data.Entries[2].Value == nil || *data.Entries[2].Value != "value" {
				t.Fatalf("GETMETADATA = %#v", data)
			}
			var listMetadata []*MailboxMetadata
			listed, err := c.ListMailboxesExt(ctx, "", "*", nil, &ListExtOptions{Metadata: &ListReturnMetadata{
				Entries: []MetadataEntryName{"/shared/comment"}, Handler: func(data *MailboxMetadata) { listMetadata = append(listMetadata, data) },
			}})
			if err != nil || len(listed) != 1 || len(listMetadata) != 1 || len(listMetadata[0].Entries) != 1 ||
				listMetadata[0].Entries[0].Value == nil || *listMetadata[0].Entries[0].Value != "list value" {
				t.Fatalf("LIST-METADATA = %#v, %#v, %v", listed, listMetadata, err)
			}
			if err := c.Noop(nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if handler != "callback" {
				if len(changes) != 0 {
					t.Fatal("nil handler delivered changes")
				}
				return
			}
			// Check retained pointers after every later response has been read.
			want := []*MetadataChange{
				{Mailbox: "", Entries: []MetadataEntryName{"/shared/comment"}},
				{Mailbox: "Entwürfe", Entries: []MetadataEntryName{"/shared/comment", "/private/comment"}},
				{Mailbox: "", Entries: []MetadataEntryName{"/private/comment"}},
				{Mailbox: "Entwürfe", Entries: []MetadataEntryName{"/shared/comment"}},
				{Mailbox: "INBOX", Entries: []MetadataEntryName{"/shared/comment"}},
				{Mailbox: "Other", Entries: []MetadataEntryName{"/private/custom"}},
			}
			if !reflect.DeepEqual(changes, want) {
				t.Fatalf("changes = %#v, want %#v", changes, want)
			}
		})
	}
}

func TestMetadataChangesValidateWithoutHandler(t *testing.T) {
	for _, wire := range []string{
		`* METADATA INBOX`,
		`* METADATA INBOX /shared/comment (`,
		`* METADATA INBOX {4294967295}`,
		`* METADATA INBOX /shared/comment "unterminated`,
	} {
		t.Run(wire, func(t *testing.T) {
			c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 METADATA] ready", func(s *extAServer) {
				tag, _ := s.command()
				s.reply(wire, tag+" OK fence")
			})
			var protocol *imap.Error
			if err := c.Noop(nil).Wait(ctx); !errors.As(err, &protocol) || protocol.Type != imap.ErrorTypeProtocol {
				t.Fatalf("malformed notification error = %v", err)
			}
		})
	}
}
