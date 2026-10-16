package imapclient

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kiliant/go-imap"
)

const uidBatchesCaps = "* PREAUTH [CAPABILITY IMAP4rev1 UIDBATCHES] ready"

func TestUIDBatchesUIDOnly(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4rev1 UIDBATCHES UIDONLY ENABLE] ready", func(s *extAServer) {
		tag, wire := s.command()
		if wire != "ENABLE UIDONLY" {
			t.Errorf("wire %q", wire)
		}
		s.reply("* ENABLED UIDONLY", tag+" OK enabled")
		s.selectInbox("INBOX", "1")
		tag, wire = s.command()
		if wire != "UIDBATCHES 500" {
			t.Errorf("wire %q", wire)
		}
		s.reply("* UIDBATCHES (TAG "+tag+") 9:1", tag+" OK done")
	})
	if _, err := c.Enable(nil, "UIDONLY").Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UIDBatches(ctx, 500, nil); err != nil {
		t.Fatal(err)
	}
}

func TestUIDBatchesRequests(t *testing.T) {
	for _, ranged := range []bool{false, true} {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("range=%t/empty=%t", ranged, empty), func(t *testing.T) {
				c, ctx := newExtATestClient(t, uidBatchesCaps, func(s *extAServer) {
					s.selectInbox("INBOX", "1")
					tag, wire := s.command()
					want := "UIDBATCHES 500"
					if ranged {
						want += " 1:200"
					}
					if wire != want {
						t.Errorf("wire %q, want %q", wire, want)
					}
					ranges := " 9:5,4:4,3:1"
					if empty {
						ranges = ""
					}
					s.reply(`* UIDBATCHES (TAG "foreign") 99:1`, `* UIDBATCHES (TAG "`+tag+`")`+ranges, tag+" OK done")
				})
				if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				var options *UIDBatchesOptions
				if ranged {
					options = &UIDBatchesOptions{Indices: imap.UIDBatchIndexRange{First: 1, Last: 200}}
				}
				data, err := c.UIDBatches(ctx, 500, options)
				if err != nil {
					t.Fatal(err)
				}
				if data.Tag == "" || empty && len(data.Batches) != 0 || !empty && (len(data.Batches) != 3 || data.Batches[0].Range.Start != 9 || data.Batches[1].Range.Stop != 4) {
					t.Fatalf("result %#v", data)
				}
			})
		}
	}
}

func TestUIDBatchesLocalRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    uint32
		indices imap.UIDBatchIndexRange
		code    imap.ResponseCode
	}{
		{"small", 499, imap.UIDBatchIndexRange{}, imap.CodeTooFew},
		{"half", 500, imap.UIDBatchIndexRange{First: 1}, imap.CodeClientBug},
		{"half-last", 500, imap.UIDBatchIndexRange{Last: 1}, imap.CodeClientBug},
		{"reverse", 500, imap.UIDBatchIndexRange{First: 2, Last: 1}, imap.CodeClientBug},
		{"span", 500, imap.UIDBatchIndexRange{First: 1, Last: 201}, imap.CodeTooMany},
		{"overflow", 4294967295, imap.UIDBatchIndexRange{First: 1, Last: 4294967295}, imap.CodeTooMany},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ctx := newExtATestClient(t, uidBatchesCaps, func(s *extAServer) {
				s.selectInbox("INBOX", "1")
				tag, wire := s.command()
				if wire != "NOOP" {
					t.Errorf("unexpected wire %q", wire)
				}
				s.ok(tag)
			})
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			_, err := c.UIDBatches(ctx, tc.size, &UIDBatchesOptions{Indices: tc.indices})
			var ie *imap.Error
			if !errors.As(err, &ie) || ie.Type != imap.ErrorTypeProtocol || ie.Code != tc.code {
				t.Fatalf("refusal: %v", err)
			}
			if err := c.Noop(nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
	c, ctx := newExtATestClient(t, "* PREAUTH ready", func(s *extAServer) {
		tag, wire := s.command()
		if wire != "NOOP" {
			t.Errorf("unexpected %q", wire)
		}
		s.ok(tag)
	})
	if _, err := c.UIDBatches(ctx, 500, nil); !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("capability refusal: %v", err)
	}
	if err := c.Noop(nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUIDBatchesProtocolAndServerErrors(t *testing.T) {
	for _, tc := range []struct {
		name, response, completion string
		typ                        imap.ErrorType
		code                       imap.ResponseCode
	}{
		{"missing", "", "OK done", imap.ErrorTypeProtocol, ""},
		{"duplicate", "* UIDBATCHES (TAG %s) 5:1\r\n* UIDBATCHES (TAG %s) 5:1", "OK done", imap.ErrorTypeProtocol, ""},
		{"untagged", "* UIDBATCHES 5:1", "OK done", imap.ErrorTypeProtocol, ""},
		{"few", "", "NO [TOOFEW] minimum", imap.ErrorTypeNo, imap.CodeTooFew},
		{"many", "", "NO [TOOMANY] limit", imap.ErrorTypeNo, imap.CodeTooMany},
		{"repeat", "", "NO [LIMIT] repeat", imap.ErrorTypeNo, imap.CodeLimit},
		{"bad", "", "BAD [CLIENTBUG] bad", imap.ErrorTypeBad, imap.CodeClientBug},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ctx := newExtATestClient(t, uidBatchesCaps, func(s *extAServer) {
				s.selectInbox("INBOX", "1")
				tag, _ := s.command()
				if tc.response != "" {
					line := tc.response
					if tc.name == "duplicate" {
						line = fmt.Sprintf(line, tag, tag)
					}
					s.reply(line)
				}
				s.reply(tag + " " + tc.completion)
			})
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			_, err := c.UIDBatches(ctx, 500, nil)
			var ie *imap.Error
			if !errors.As(err, &ie) || ie.Type != tc.typ || ie.Code != tc.code {
				t.Fatalf("error: %v", err)
			}
		})
	}
}

func TestUIDBatchesPipelinedAndLiteralTags(t *testing.T) {
	c, ctx := newExtATestClient(t, uidBatchesCaps, func(s *extAServer) {
		s.selectInbox("INBOX", "1")
		first, _ := s.command()
		second, _ := s.command()
		// Reverse replies, include another command's untagged data and use
		// a literal tag. Both collectors must inspect the same decoded value.
		s.reply("* 4 EXISTS", fmt.Sprintf("* UIDBATCHES (TAG {%d}\r\n%s) 90:1", len(second), second), second+" OK done", "* UIDBATCHES (TAG "+first+") 9:1", first+" OK done")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		data *UIDBatchesData
		err  error
	}
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() { data, err := c.UIDBatches(ctx, 500, nil); results <- outcome{data, err} }()
	}
	a, b := <-results, <-results
	if a.err != nil || b.err != nil {
		t.Fatalf("results %v, %v", a.err, b.err)
	}
	if a.data.Tag == b.data.Tag || a.data.Batches[0].Range.Start+b.data.Batches[0].Range.Start != 99 {
		t.Fatalf("misattribution: %#v, %#v", a.data, b.data)
	}
}

func TestUIDBatchesInterleavesStatusCollector(t *testing.T) {
	c, ctx := newExtATestClient(t, uidBatchesCaps, func(s *extAServer) {
		s.selectInbox("INBOX", "1")
		statusTag, _ := s.command()
		batchTag, _ := s.command()
		s.reply("* UIDBATCHES (TAG foreign) 999:1", "* STATUS Archive (MESSAGES 12)", "* UIDBATCHES (TAG "+batchTag+") 9:1", statusTag+" OK status", batchTag+" OK batches")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	status := c.Status("Archive", nil)
	data, err := c.UIDBatches(ctx, 500, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, err := status.Wait(ctx)
	if err != nil || st.NumMessages != 12 || data.Batches[0].Range.Start != 9 {
		t.Fatalf("status %#v, batches %#v: %v", st, data, err)
	}
}
