package imapclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestUnclaimedESearchDecodeErrorsAreDiscarded(t *testing.T) {
	for _, wire := range []string{
		`(TAG "zz") ALL 1:*`,
		`(TAG "zz") ADDTO (0 5:*)`,
		`(TAG "zz" FOO 1) ADDTO (0 1)`,
		`(TAG "zz" FOO {4}` + "\r\n" + `test) ADDTO (0 1)`,
		`(TAG "zz") COUNT invalid`,
		`(TAG "zz") ADDTO (0 1`,
	} {
		t.Run(wire, func(t *testing.T) {
			var statuses int
			c, ctx := newRoutingTestClient(t, contextCapabilities, &Options{UnilateralData: &UnilateralDataHandler{
				Status: func(data *StatusData) {
					if data.Mailbox != "INBOX" || data.NumMessages != 7 {
						t.Errorf("following STATUS = %#v", data)
					}
					statuses++
				},
			}}, func(s *extAServer) {
				tag, _ := s.command()
				s.reply("* ESEARCH "+wire, "* STATUS INBOX (MESSAGES 7)", tag+" OK fence")
			})
			if err := c.Noop(nil).Wait(ctx); err != nil {
				t.Fatalf("unclaimed ESEARCH closed connection: %v", err)
			}
			if statuses != 1 {
				t.Fatalf("following STATUS delivered %d times", statuses)
			}
		})
	}
}

func TestESearchSequenceSetWildcard(t *testing.T) {
	for _, uid := range []bool{false, true} {
		mode := ""
		if uid {
			mode = " UID"
		}
		t.Run(mode, func(t *testing.T) {
			c, ctx := newExtATestClient(t, contextCapabilities, func(s *extAServer) {
				s.selectInbox("INBOX", "101")
				tag, _ := s.command()
				s.reply(`* ESEARCH (TAG "`+tag+`")`+mode+` ALL 1:*`, tag+" OK searched")
			})
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			search := c.SearchExtended
			if uid {
				search = c.SearchExtendedUID
			}
			data, err := search(imap.SearchAll, nil).Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			got := data.All.String()
			if uid {
				got = data.AllUIDs.String()
			}
			if got != "1:*" || data.Values["ALL"] != "1:*" {
				t.Fatalf("ALL = %q, values = %#v", got, data.Values)
			}
		})
	}
}

func TestUnclaimedESearchFatalErrorsStillFail(t *testing.T) {
	for _, wire := range []string{
		`(TAG "zz") X {4294967295}`,
		`(TAG "zz" FOO {4294967295}`,
		`(TAG "zz") X ` + strings.Repeat("(", 101),
		`(TAG "zz" FOO ` + strings.Repeat("a", 8192),
	} {
		t.Run(wire[:min(len(wire), 50)], func(t *testing.T) {
			c, ctx := newExtATestClient(t, contextCapabilities, func(s *extAServer) {
				tag, _ := s.command()
				s.reply("* ESEARCH "+wire, tag+" OK fence")
			})
			var protocol *imap.Error
			if err := c.Noop(nil).Wait(ctx); !errors.As(err, &protocol) || protocol.Type != imap.ErrorTypeProtocol {
				t.Fatalf("fatal ESEARCH error = %v", err)
			}
		})
	}
}

func TestOwnedESearchDecodeErrorsStillFail(t *testing.T) {
	for _, state := range []struct {
		name string
		live bool
	}{{"pending", false}, {"live", true}} {
		for _, tail := range []string{` FOO 1) ADDTO (0 1)`, `) COUNT invalid`} {
			t.Run(state.name+"/"+tail, func(t *testing.T) {
				c, ctx := newExtATestClient(t, contextCapabilities, func(s *extAServer) {
					s.selectInbox("INBOX", "101")
					tag, _ := s.command()
					if state.live {
						s.reply(`* ESEARCH (TAG "`+tag+`") COUNT 1`, tag+" OK registered")
						_, _ = s.command()
					}
					s.reply(`* ESEARCH (TAG "` + tag + `"` + tail)
				})
				if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				var options *ESearchOptions
				if state.live {
					options = &ESearchOptions{ReturnOptions: []SearchReturnOption{SearchReturnUpdate}}
				}
				_, err := c.SearchExtended(imap.SearchAll, options).Wait(ctx)
				if state.live {
					if err != nil {
						t.Fatal(err)
					}
					err = c.Noop(nil).Wait(ctx)
				}
				var protocol *imap.Error
				if !errors.As(err, &protocol) || protocol.Type != imap.ErrorTypeProtocol {
					t.Fatalf("owned malformed ESEARCH error = %v", err)
				}
			})
		}
	}
}
