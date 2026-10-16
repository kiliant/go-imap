package imapclient

import (
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestMultiSearchMailboxEncoding(t *testing.T) {
	for _, tc := range []struct {
		name, wire, want string
		utf8             bool
	}{
		{"modified-UTF7", "Entw&APw-rfe", "Entwürfe", false},
		{"UTF8", "Entwürfe", "Entwürfe", true},
		{"UTF8-literal-ampersand", "Entw&APw-rfe", "Entw&APw-rfe", true},
		{"INBOX", "inbox", "INBOX", false},
		{"UTF8-INBOX", "inbox", "INBOX", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var updates []*ContextUpdate
			greeting := strings.Replace(contextCapabilities, "] ready", " ENABLE UTF8=ACCEPT] ready", 1)
			c, ctx := newRoutingTestClient(t, greeting, &Options{UnilateralData: &UnilateralDataHandler{
				ContextUpdate: func(data *ContextUpdate) { updates = append(updates, data) },
			}}, func(s *extAServer) {
				if tc.utf8 {
					tag, _ := s.command()
					s.reply("* ENABLED UTF8=ACCEPT", tag+" OK enabled")
				}
				s.selectInbox("INBOX", "101")
				old, _ := s.command()
				correlator := `" MAILBOX "` + tc.wire + `" UIDVALIDITY 101) UID `
				s.reply(`* ESEARCH (TAG "`+old+correlator+`COUNT 1`, old+" OK searched")
				tag, _ := s.command()
				s.reply(`* ESEARCH (TAG "`+old+correlator+`ADDTO (0 9)`, tag+" OK fence")
			})
			if tc.utf8 {
				if _, err := c.EnableUTF8Accept(nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			data, err := c.MultiSearch(imap.SearchAll, &MultiSearchOptions{ReturnOptions: []SearchReturnOption{SearchReturnUpdate}}).Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(data.Results) != 1 || data.Results[0].Mailbox != tc.want {
				t.Fatalf("MULTISEARCH results = %#v, want mailbox %q", data.Results, tc.want)
			}
			if err := c.Noop(nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if len(updates) != 1 || updates[0].Mailbox != tc.want || updates[0].UIDValidity != 101 {
				t.Fatalf("CONTEXT updates = %#v, want mailbox %q", updates, tc.want)
			}
		})
	}
}
