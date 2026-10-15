package imapclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

const contextCapabilities = "* PREAUTH [CAPABILITY IMAP4REV1 ESEARCH CONTEXT=SEARCH SORT ESORT CONTEXT=SORT MULTISEARCH] ready"

func contextSearch(t *testing.T, ctx context.Context, c *Client, uid bool) *ESearchCommand {
	t.Helper()
	opts := &ESearchOptions{ReturnOptions: []SearchReturnOption{SearchReturnCount, SearchReturnUpdate}}
	var cmd *ESearchCommand
	if uid {
		cmd = c.SearchExtendedUID(imap.SearchAll, opts)
	} else {
		cmd = c.SearchExtended(imap.SearchAll, opts)
	}
	data, err := cmd.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if data.ContextTag != cmd.Tag() || data.ContextTag == "" {
		t.Fatalf("context tag = %q, command tag = %q", data.ContextTag, cmd.Tag())
	}
	return cmd
}

func TestContextUpdatesInterleaveCommands(t *testing.T) {
	for _, uid := range []bool{false, true} {
		for _, next := range []string{"SEARCH", "SORT", "MULTISEARCH", "NOOP", "plain SEARCH", "plain SORT"} {
			t.Run(fmt.Sprintf("UID=%t/%s", uid, next), func(t *testing.T) {
				var updates []*ContextUpdate
				mode := ""
				if uid {
					mode = " UID"
				}
				c, ctx := newRoutingTestClient(t, contextCapabilities, &Options{MaxUntaggedResponses: 1, UnilateralData: &UnilateralDataHandler{
					ContextUpdate: func(data *ContextUpdate) { updates = append(updates, data) },
				}}, func(s *extAServer) {
					s.selectInbox("INBOX", "101")
					old, _ := s.command()
					// UPDATE is live before tagged completion. It must not be
					// counted as a second initial ESEARCH or overwrite COUNT.
					s.reply(`* ESEARCH (TAG "`+old+`")`+mode+` COUNT 3`,
						`* ESEARCH (TAG "`+old+`")`+mode+` ADDTO (0 9,4:2 1 7)`, old+" OK done")
					tag, _ := s.command()
					s.reply(`* ESEARCH (TAG "` + old + `")` + mode + ` REMOVEFROM (2 8) ADDTO (1 6) ADDTO (0 1:4294967295) X-FUTURE ("a" NIL)`)
					switch next {
					case "SEARCH", "SORT":
						s.reply(`* ESEARCH (TAG "` + tag + `") COUNT 7`)
					case "MULTISEARCH":
						s.reply(`* ESEARCH (TAG "` + tag + `" MAILBOX "INBOX" UIDVALIDITY 101) UID COUNT 7`)
					case "plain SEARCH":
						s.reply("* SEARCH 7")
					case "plain SORT":
						s.reply("* SORT 7")
					}
					s.ok(tag)
					tag, _ = s.command()
					// Future return items on a completed context stay events.
					s.reply(`* ESEARCH (TAG "`+old+`")`+mode+` X-FUTURE 42`, tag+" OK fence")
				})
				if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				cmd := contextSearch(t, ctx, c, uid)
				if cmd.data.Count != 3 {
					t.Fatal("update changed initial result")
				}
				var err error
				switch next {
				case "SEARCH":
					var data *ESearchData
					data, err = c.SearchExtended(imap.SearchAll, nil).Wait(ctx)
					if err == nil && data.Count != 7 {
						t.Fatal("old context became the new search's result")
					}
				case "SORT":
					_, err = c.SortExtended(ctx, []SortKeySpec{{Key: SortKeyDate}}, nil, nil)
				case "MULTISEARCH":
					_, err = c.MultiSearch(imap.SearchAll, nil).Wait(ctx)
				case "NOOP":
					err = c.Noop(nil).Wait(ctx)
				case "plain SEARCH":
					_, err = c.Search(imap.SearchAll, nil).All(ctx)
				case "plain SORT":
					_, err = c.Sort(ctx, []SortKeySpec{{Key: SortKeyDate}}, nil, nil)
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Noop(nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				if len(updates) != 3 || updates[0].Tag != cmd.Tag() || updates[0].UID != uid || len(updates[0].Items[0].Changes) != 2 {
					t.Fatalf("updates = %#v", updates)
				}
				items := updates[1].Items
				if len(items) != 4 || items[0].Key != imap.ESearchReturnKeyRemoveFrom || items[1].Key != imap.ESearchReturnKeyAddTo || items[2].Key != imap.ESearchReturnKeyAddTo || items[3].Value != `("a" NIL)` {
					t.Fatalf("ordered items = %#v", items)
				}
				change := updates[0].Items[0].Changes[0]
				if uid && (change.UIDs.String() != "9,4:2" || change.SeqNums != nil) || !uid && (change.SeqNums.String() != "9,4:2" || change.UIDs != nil) {
					t.Fatalf("number order/mode changed: %#v", change)
				}
				if updates[2].Items[0].Value != "42" {
					t.Fatal("future return item lost")
				}
			})
		}
	}
}

func TestContextCancelUpdateAcknowledgement(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			var updates []*ContextUpdate
			c, ctx := newRoutingTestClient(t, contextCapabilities, &Options{UnilateralData: &UnilateralDataHandler{
				ContextUpdate: func(data *ContextUpdate) { updates = append(updates, data) },
			}}, func(s *extAServer) {
				s.selectInbox("INBOX", "101")
				old, _ := s.command()
				s.reply(`* ESEARCH (TAG "`+old+`") COUNT 1`, old+" OK done")
				tag, wire := s.command()
				if !strings.Contains(wire, `CANCELUPDATE "`+old+`" "unknown"`) {
					t.Errorf("cancel wire = %q", wire)
				}
				s.reply(`* ESEARCH (TAG "` + old + `") ADDTO (0 8)`)
				if success {
					s.ok(tag)
				} else {
					s.reply(tag + " NO cancellation failed")
				}
				tag, _ = s.command()
				s.reply(`* ESEARCH (TAG "`+old+`") REMOVEFROM (0 8)`, tag+" OK fence")
			})
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			cmd := contextSearch(t, ctx, c, false)
			err := c.CancelUpdate(ctx, []string{cmd.Tag(), "unknown"}, nil)
			if (err == nil) != success {
				t.Fatalf("cancel = %v", err)
			}
			if err := c.Noop(nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			want := 2
			if success {
				want = 1
			}
			if len(updates) != want {
				t.Fatalf("updates = %d, want %d", len(updates), want)
			}
		})
	}
}

func TestContextNoUpdateIsNotResurrectedByCompletion(t *testing.T) {
	for _, tagged := range []bool{false, true} {
		t.Run(fmt.Sprint(tagged), func(t *testing.T) {
			var updates, statuses int
			c, ctx := newRoutingTestClient(t, contextCapabilities, &Options{UnilateralData: &UnilateralDataHandler{
				ContextUpdate: func(*ContextUpdate) { updates++ },
				StatusResponse: func(data *StatusResponse) {
					if data.Code == imap.ResponseCode("NOUPDATE") {
						statuses++
					}
				},
			}}, func(s *extAServer) {
				s.selectInbox("INBOX", "101")
				old, _ := s.command()
				if !tagged {
					s.reply(`* NO [NOUPDATE "` + old + `"] no updates`)
				}
				s.reply(`* ESEARCH (TAG "` + old + `") COUNT 3`)
				if tagged {
					s.reply(old + ` OK [NOUPDATE "` + old + `"] no updates`)
				} else {
					s.ok(old)
				}
				tag, _ := s.command()
				s.reply(`* ESEARCH (TAG "`+old+`") ADDTO (0 9)`, tag+" OK fence")
				s.command() // keep the connection alive until registry assertions
			})
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			contextSearch(t, ctx, c, false)
			if err := c.Noop(nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if updates != 0 || !tagged && statuses != 1 {
				t.Fatalf("updates=%d NOUPDATE callbacks=%d", updates, statuses)
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if len(c.searchContexts) != 0 {
				t.Fatal("refused context was resurrected")
			}
		})
	}
}

func TestContextSelectionAndFailureCleanup(t *testing.T) {
	for _, name := range []string{"SELECT", "EXAMINE", "CLOSE", "UNSELECT", "SEARCH"} {
		for _, success := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", name, success), func(t *testing.T) {
				var count int
				c, ctx := newRoutingTestClient(t, contextCapabilities, &Options{UnilateralData: &UnilateralDataHandler{
					ContextUpdate: func(*ContextUpdate) { count++ },
				}}, func(s *extAServer) {
					s.selectInbox("INBOX", "101")
					old, _ := s.command()
					s.reply(`* ESEARCH (TAG "`+old+`") COUNT 1`, old+" OK registered")
					tag, _ := s.command()
					if success {
						if name == "SEARCH" {
							s.reply(`* ESEARCH (TAG "` + tag + `") COUNT 1`)
						}
						s.ok(tag)
					} else {
						s.reply(tag + " NO failed")
					}
					tag, _ = s.command()
					s.reply(`* ESEARCH (TAG "`+old+`") ADDTO (0 9)`, tag+" OK fence")
					s.command() // EOF must not clear registrations before assertions
				})
				if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				contextSearch(t, ctx, c, false)
				var err error
				switch name {
				case "SELECT":
					_, err = c.Select("Other", nil).Wait(ctx)
				case "EXAMINE":
					_, err = c.Examine("Other", nil).Wait(ctx)
				case "CLOSE":
					err = c.CloseMailbox(nil).Wait(ctx)
				case "UNSELECT":
					err = c.Unselect(nil).Wait(ctx)
				case "SEARCH":
					_, err = c.SearchExtended(imap.SearchAll, &ESearchOptions{ReturnOptions: []SearchReturnOption{SearchReturnUpdate}}).Wait(ctx)
				}
				if (err == nil) != success {
					t.Fatalf("command = %v", err)
				}
				if err := c.Noop(nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				keepOld := name == "SEARCH" || !success && (name == "CLOSE" || name == "UNSELECT")
				if (count == 1) != keepOld {
					t.Fatalf("old-context updates = %d, keep=%t", count, keepOld)
				}
				c.mu.Lock()
				registered := len(c.searchContexts)
				c.mu.Unlock()
				if name == "SEARCH" && registered != 1+boolInt(success) {
					t.Fatalf("registrations after second search = %d", registered)
				}
			})
		}
	}
}

func TestContextUpdatesWithoutPendingCommandAndDuringIdle(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(fmt.Sprint(idle), func(t *testing.T) {
			events := make(chan *ContextUpdate, 1)
			c, ctx := newRoutingTestClient(t, contextCapabilities, &Options{UnilateralData: &UnilateralDataHandler{
				ContextUpdate: func(data *ContextUpdate) {
					select {
					case events <- data:
					default:
						t.Error("unexpected extra event")
					}
				},
			}}, func(s *extAServer) {
				s.selectInbox("INBOX", "101")
				old, _ := s.command()
				s.reply(`* ESEARCH (TAG "`+old+`") UID COUNT 1`, old+" OK done")
				var tag string
				if idle {
					tag, _ = s.command()
					s.reply("+ idling")
				}
				s.reply(`* ESEARCH (TAG "` + old + `") UID REMOVEFROM (0 7)`)
				if idle {
					line, _ := s.r.ReadString('\n')
					if line != "DONE\r\n" {
						t.Errorf("IDLE completion = %q", line)
					}
					s.ok(tag)
				}
				s.command()
			})
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			cmd := contextSearch(t, ctx, c, true)
			var idleCmd *IdleCommand
			if idle {
				c.addCapabilities([]string{"IDLE"})
				idleCmd = c.Idle(nil)
				if err := idleCmd.WaitReady(ctx); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case data := <-events:
				if data.Tag != cmd.Tag() || data.Items[0].Changes[0].UIDs.String() != "7" {
					t.Fatalf("event = %#v", data)
				}
			case <-ctx.Done():
				t.Fatal("no context event")
			}
			if idle {
				if err := idleCmd.Done(); err != nil {
					t.Fatal(err)
				}
				if err := idleCmd.Wait(ctx); err != nil {
					t.Fatal(err)
				}
			}
			c.mu.Lock()
			if len(c.searchContexts) != 1 {
				t.Error("IDLE/no-command delivery lost registration")
			}
			c.mu.Unlock()
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if len(c.searchContexts) != 0 {
				t.Fatal("disconnect retained registrations")
			}
		})
	}
}

func TestContextSortUpdateAndModeValidation(t *testing.T) {
	for _, wire := range []string{"UID ADDTO (1 9,4:2)", "UID ADDTO (0 9)", "ADDTO (1 9)"} {
		t.Run(wire, func(t *testing.T) {
			var updates []*ContextUpdate
			c, ctx := newRoutingTestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 SORT CONTEXT=SORT] ready", &Options{UnilateralData: &UnilateralDataHandler{
				ContextUpdate: func(data *ContextUpdate) { updates = append(updates, data) },
			}}, func(s *extAServer) {
				s.selectInbox("INBOX", "101")
				old, sent := s.command()
				if !strings.Contains(sent, "UID SORT RETURN (UPDATE COUNT)") {
					t.Errorf("sort wire = %q", sent)
				}
				s.reply(`* ESEARCH (TAG "`+old+`") UID COUNT 1`, old+" OK done")
				tag, _ := s.command()
				s.reply(`* ESEARCH (TAG "`+old+`") `+wire, tag+" OK fence")
			})
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			data, err := c.SortExtendedUID(ctx, []SortKeySpec{{Key: SortKeyDate}}, nil, &ESortOptions{
				ReturnOptions: []SearchReturnOption{SearchReturnUpdate, SearchReturnCount},
			})
			if err != nil || data.ContextTag == "" {
				t.Fatalf("sort registration = %#v, %v", data, err)
			}
			err = c.Noop(nil).Wait(ctx)
			if wire == "UID ADDTO (1 9,4:2)" {
				if err != nil || len(updates) != 1 || updates[0].Tag != data.ContextTag || updates[0].Items[0].Changes[0].UIDs.String() != "9,4:2" {
					t.Fatalf("sort update = %#v, %v", updates, err)
				}
			} else if err == nil {
				t.Fatal("invalid SORT update accepted")
			}
		})
	}
}

func TestContextUpdateCapabilitiesAndMultiSearch(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 ESEARCH] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		s.command()
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SearchExtended(imap.SearchAll, &ESearchOptions{ReturnOptions: []SearchReturnOption{SearchReturnUpdate}}).Wait(ctx); !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("missing CONTEXT=SEARCH error = %v", err)
	}

	var updates []*ContextUpdate
	multi, multiCtx := newRoutingTestClient(t, contextCapabilities, &Options{UnilateralData: &UnilateralDataHandler{
		ContextUpdate: func(data *ContextUpdate) { updates = append(updates, data) },
	}}, func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		old, _ := s.command()
		s.reply(`* ESEARCH (TAG "`+old+`" MAILBOX "INBOX" UIDVALIDITY 101) UID COUNT 1`, old+" OK done")
		tag, _ := s.command()
		s.reply(`* ESEARCH (TAG "`+old+`" MAILBOX "INBOX" UIDVALIDITY 101) UID REMOVEFROM (0 9)`, tag+" OK fence")
	})
	if _, err := multi.Select("INBOX", nil).Wait(multiCtx); err != nil {
		t.Fatal(err)
	}
	cmd := multi.MultiSearch(imap.SearchAll, &MultiSearchOptions{ReturnOptions: []SearchReturnOption{SearchReturnUpdate}})
	if _, err := cmd.Wait(multiCtx); err != nil {
		t.Fatal(err)
	}
	if err := multi.Noop(nil).Wait(multiCtx); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Tag != cmd.Tag() || updates[0].Mailbox != "INBOX" || updates[0].UIDValidity != 101 {
		t.Fatalf("MULTISEARCH update = %#v", updates)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestContextMalformedUpdatesFailWithNilHandler(t *testing.T) {
	for _, wire := range []string{`ADDTO ()`, `ADDTO (0)`, `ADDTO (0 0)`, `ADDTO (0 1 2)`, `REMOVEFROM (-1 1)`, `REMOVEFROM (0 1:4294967296)`} {
		t.Run(wire, func(t *testing.T) {
			c, ctx := newExtATestClient(t, contextCapabilities, func(s *extAServer) {
				s.selectInbox("INBOX", "101")
				old, _ := s.command()
				s.reply(`* ESEARCH (TAG "`+old+`") COUNT 1`, old+" OK done")
				tag, _ := s.command()
				s.reply(`* ESEARCH (TAG "`+old+`") `+wire, tag+" OK done")
			})
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			contextSearch(t, ctx, c, false)
			var protocolErr *imap.Error
			if err := c.Noop(nil).Wait(ctx); !errors.As(err, &protocolErr) || protocolErr.Type != imap.ErrorTypeProtocol {
				t.Fatalf("malformed update error = %v", err)
			}
		})
	}
}

func TestContextLogoutAndTransportLossCleanup(t *testing.T) {
	for _, logout := range []bool{false, true} {
		t.Run(fmt.Sprint(logout), func(t *testing.T) {
			var count int
			c, ctx := newRoutingTestClient(t, contextCapabilities, &Options{UnilateralData: &UnilateralDataHandler{
				ContextUpdate: func(*ContextUpdate) { count++ },
			}}, func(s *extAServer) {
				s.selectInbox("INBOX", "101")
				old, _ := s.command()
				s.reply(`* ESEARCH (TAG "`+old+`") COUNT 1`, old+" OK done")
				if logout {
					tag, _ := s.command()
					s.reply(`* ESEARCH (TAG "`+old+`") ADDTO (0 9)`, "* BYE logging out",
						`* ESEARCH (TAG "`+old+`") REMOVEFROM (0 9)`, tag+" OK logged out")
				}
			})
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			contextSearch(t, ctx, c, false)
			if logout {
				if err := c.Logout(ctx, nil); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-c.readerDone:
			case <-ctx.Done():
				t.Fatal("reader did not stop")
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if len(c.searchContexts) != 0 || count != boolInt(logout) {
				t.Fatalf("registrations=%d delivered=%d", len(c.searchContexts), count)
			}
		})
	}
}
