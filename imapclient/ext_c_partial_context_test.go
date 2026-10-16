package imapclient

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestSearchPartialContextLifecycle(t *testing.T) {
	for _, uid := range []bool{false, true} {
		for _, partialCapability := range []string{"", " PARTIAL"} {
			t.Run(fmt.Sprintf("UID=%t/PARTIAL=%t", uid, partialCapability != ""), func(t *testing.T) {
				var updates []*ContextUpdate
				mode, prefix := "", ""
				if uid {
					mode, prefix = " UID", "UID "
				}
				c, ctx := newRoutingTestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 ESEARCH CONTEXT=SEARCH"+partialCapability+"] ready", &Options{UnilateralData: &UnilateralDataHandler{
					ContextUpdate: func(data *ContextUpdate) { updates = append(updates, data) },
				}}, func(s *extAServer) {
					s.selectInbox("INBOX", "101")
					old, wire := s.command()
					if wire != prefix+"SEARCH RETURN (PARTIAL 1:2 UPDATE) ALL" {
						t.Errorf("partial wire = %q", wire)
					}
					s.reply(`* ESEARCH (TAG "`+old+`")`+mode+` PARTIAL (1:2 5:6)`,
						`* ESEARCH (TAG "`+old+`")`+mode+` ADDTO (0 7)`, old+" OK initial")
					tag, _ := s.command()
					s.reply(`* ESEARCH (TAG "`+old+`")`+mode+` REMOVEFROM (1 5)`,
						`* ESEARCH (TAG "`+tag+`") COUNT 9`, tag+" OK searched")
					tag, wire = s.command()
					if !strings.Contains(wire, `CANCELUPDATE "`+old+`"`) {
						t.Errorf("cancel wire = %q", wire)
					}
					s.ok(tag)
					tag, _ = s.command()
					s.reply(`* ESEARCH (TAG "`+old+`")`+mode+` ADDTO (0 8)`, tag+" OK fence")
				})
				if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				search := c.SearchPartial
				if uid {
					search = c.SearchPartialUID
				}
				data, partial, saved, err := search(ctx, imap.SearchAll, &PartialSearchOptions{
					Range: PartialRange{FirstStart: 1, FirstEnd: 2}, ReturnOptions: []SearchReturnOption{SearchReturnUpdate},
				})
				if err != nil {
					t.Fatal(err)
				}
				if partial == nil || !partial.HasResults || data.ContextTag == "" || data.ContextTag != data.Tag || saved != nil {
					t.Fatalf("partial result = %#v, %#v, %#v", data, partial, saved)
				}
				next, err := c.SearchExtended(imap.SearchAll, nil).Wait(ctx)
				if err != nil || next.Count != 9 {
					t.Fatalf("next search = %#v, %v", next, err)
				}
				if err := c.CancelUpdate(ctx, []string{data.ContextTag}, nil); err != nil {
					t.Fatal(err)
				}
				if err := c.Noop(nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				if len(updates) != 2 {
					t.Fatalf("delivered %d updates, want initial and interleaved updates only", len(updates))
				}
				for i, update := range updates {
					if update.Tag != data.ContextTag || update.UID != uid {
						t.Fatalf("update %d = %#v", i, update)
					}
				}
			})
		}
	}
}

func TestSearchPartialUpdateRequiresContext(t *testing.T) {
	for _, uid := range []bool{false, true} {
		t.Run(fmt.Sprintf("UID=%t", uid), func(t *testing.T) {
			c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
			extCReady(c, []string{"IMAP4REV1", "ESEARCH", "PARTIAL"}, nil, true)
			search := c.SearchPartial
			if uid {
				search = c.SearchPartialUID
			}
			_, _, _, err := search(extCContext(t), imap.SearchAll, &PartialSearchOptions{
				Range: PartialRange{FirstStart: 1, FirstEnd: 2}, ReturnOptions: []SearchReturnOption{SearchReturnOptionKeyword("update")},
			})
			if !errors.Is(err, ErrCapabilityNotAdvertised) || len(server.Lines()) != 0 {
				t.Fatalf("missing CONTEXT = %v, wire = %q", err, server.Lines())
			}
		})
	}
}
