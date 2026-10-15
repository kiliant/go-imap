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
	for _, tc := range []struct {
		uid  bool
		wire string
		want string // the delivered set; empty when the update must fail
	}{
		{true, "UID ADDTO (1 9,4:2)", "9,4:2"},
		{true, "UID ADDTO (0 9)", ""},
		// As for the initial result, a UID context tolerates a missing UID
		// indicator; an explicit one on a sequence-number context fails.
		{true, "ADDTO (1 9)", "9"},
		{false, "UID ADDTO (1 9)", ""},
		{false, "ADDTO (2 7:5)", "7:5"},
	} {
		t.Run(fmt.Sprintf("UID=%t/%s", tc.uid, tc.wire), func(t *testing.T) {
			var updates []*ContextUpdate
			mode, prefix := "", ""
			if tc.uid {
				mode, prefix = " UID", "UID "
			}
			c, ctx := newRoutingTestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 SORT CONTEXT=SORT] ready", &Options{UnilateralData: &UnilateralDataHandler{
				ContextUpdate: func(data *ContextUpdate) { updates = append(updates, data) },
			}}, func(s *extAServer) {
				s.selectInbox("INBOX", "101")
				old, sent := s.command()
				if !strings.HasPrefix(sent, prefix+"SORT RETURN (UPDATE COUNT)") {
					t.Errorf("sort wire = %q", sent)
				}
				s.reply(`* ESEARCH (TAG "`+old+`")`+mode+` COUNT 1`, old+" OK done")
				tag, _ := s.command()
				s.reply(`* ESEARCH (TAG "`+old+`") `+tc.wire, tag+" OK fence")
			})
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			sort := c.SortExtended
			if tc.uid {
				sort = c.SortExtendedUID
			}
			data, err := sort(ctx, []SortKeySpec{{Key: SortKeyDate}}, nil, &ESortOptions{
				ReturnOptions: []SearchReturnOption{SearchReturnUpdate, SearchReturnCount},
			})
			if err != nil || data.ContextTag == "" {
				t.Fatalf("sort registration = %#v, %v", data, err)
			}
			err = c.Noop(nil).Wait(ctx)
			if tc.want == "" {
				if err == nil {
					t.Fatal("invalid SORT update accepted")
				}
				return
			}
			if err != nil || len(updates) != 1 || updates[0].Tag != data.ContextTag || updates[0].UID != tc.uid {
				t.Fatalf("sort update = %#v, %v", updates, err)
			}
			change := updates[0].Items[0].Changes[0]
			got := change.SeqNums.String()
			if tc.uid {
				got = change.UIDs.String()
			}
			if got != tc.want || tc.uid != (change.UIDs != nil) || tc.uid == (change.SeqNums != nil) {
				t.Fatalf("sort change = %#v", change)
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

// Wait fails when the initial result correlates with another tag. The server
// accepted the request, but the caller has no result to which updates could
// apply, so the registration ends with tagged completion rather than at Wait.
func TestContextForeignInitialResultLeavesNoRegistration(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprintf("MULTISEARCH=%t", multi), func(t *testing.T) {
			var updates int
			c, ctx := newRoutingTestClient(t, contextCapabilities, &Options{UnilateralData: &UnilateralDataHandler{
				ContextUpdate: func(*ContextUpdate) { updates++ },
			}}, func(s *extAServer) {
				s.selectInbox("INBOX", "101")
				tag, _ := s.command()
				if multi {
					s.reply(`* ESEARCH (TAG "X0001" MAILBOX "INBOX" UIDVALIDITY 101) UID COUNT 1`, tag+" OK done")
				} else {
					s.reply(`* ESEARCH (TAG "X0001") COUNT 1`, tag+" OK done")
				}
				fence, _ := s.command()
				if multi {
					s.reply(`* ESEARCH (TAG "`+tag+`" MAILBOX "INBOX" UIDVALIDITY 101) UID ADDTO (0 9)`, fence+" OK fence")
				} else {
					s.reply(`* ESEARCH (TAG "`+tag+`") ADDTO (0 9)`, fence+" OK fence")
				}
				s.command() // keep the connection alive until registry assertions
			})
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			returns := []SearchReturnOption{SearchReturnCount, SearchReturnUpdate}
			var err error
			if multi {
				_, err = c.MultiSearch(imap.SearchAll, &MultiSearchOptions{ReturnOptions: returns}).Wait(ctx)
			} else {
				_, err = c.SearchExtended(imap.SearchAll, &ESearchOptions{ReturnOptions: returns}).Wait(ctx)
			}
			if err == nil || !strings.Contains(err.Error(), `"X0001"`) {
				t.Fatalf("foreign correlator error = %v", err)
			}
			if err := c.Noop(nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			c.mu.Lock()
			registered := len(c.searchContexts)
			c.mu.Unlock()
			if updates != 0 || registered != 0 {
				t.Fatalf("updates=%d registrations=%d after a failed initial result", updates, registered)
			}
		})
	}
}

// A NOUPDATE that names no tag identifies no registration. It must not fail a
// connection that may never have asked for UPDATE; the condition is delivered
// as before CONTEXT delivery existed, and live registrations stay live.
func TestContextMalformedNoUpdateKeepsConnection(t *testing.T) {
	for _, code := range []string{"[NOUPDATE]", "[NOUPDATE (bad)]"} {
		for _, tagged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/tagged=%t", code, tagged), func(t *testing.T) {
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
					s.reply(`* ESEARCH (TAG "` + old + `") COUNT 1`)
					if tagged {
						s.reply(old + " OK " + code + " done")
					} else {
						s.reply("* NO "+code+" refused", old+" OK done")
					}
					fence, _ := s.command()
					s.reply(`* ESEARCH (TAG "`+old+`") ADDTO (0 9)`, fence+" OK fence")
				})
				if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				contextSearch(t, ctx, c, false)
				if err := c.Noop(nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				if updates != 1 || statuses != boolInt(!tagged) {
					t.Fatalf("updates=%d NOUPDATE callbacks=%d", updates, statuses)
				}
			})
		}
	}
}

// CONTEXT=SEARCH and CONTEXT=SORT do not imply ESEARCH's or ESORT's return
// options (RFC 5267 section 4.1). Without ESEARCH or ESORT the documented v1
// fallback still applies; only RETURN (UPDATE), which has no emulation, is
// sent in the extended form.
func TestContextCapabilityKeepsDocumentedFallback(t *testing.T) {
	for _, sorted := range []bool{false, true} {
		t.Run(fmt.Sprintf("SORT=%t", sorted), func(t *testing.T) {
			greeting := "* PREAUTH [CAPABILITY IMAP4REV1 CONTEXT=SEARCH] ready"
			if sorted {
				greeting = "* PREAUTH [CAPABILITY IMAP4REV1 SORT CONTEXT=SORT] ready"
			}
			c, ctx := newRoutingTestClient(t, greeting, nil, func(s *extAServer) {
				s.selectInbox("INBOX", "101")
				tag, sent := s.command()
				if strings.Contains(sent, "RETURN") {
					t.Errorf("MIN was not emulated: %q", sent)
				}
				if sorted {
					s.reply("* SORT 5 2", tag+" OK sorted")
				} else {
					s.reply("* SEARCH 2 5", tag+" OK searched")
				}
				tag, sent = s.command()
				if !strings.Contains(sent, "RETURN (UPDATE)") {
					t.Errorf("UPDATE was not sent in the extended form: %q", sent)
				}
				s.reply(`* ESEARCH (TAG "`+tag+`")`, tag+" OK registered")
			})
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			search := func(returns ...SearchReturnOption) (*ESearchData, error) {
				if sorted {
					return c.SortExtended(ctx, []SortKeySpec{{Key: SortKeyDate}}, nil, &ESortOptions{ReturnOptions: returns})
				}
				return c.SearchExtended(imap.SearchAll, &ESearchOptions{ReturnOptions: returns}).Wait(ctx)
			}
			data, err := search(SearchReturnMin)
			if err != nil || !data.Emulated || !data.HasMin {
				t.Fatalf("MIN = %#v, %v", data, err)
			}
			data, err = search(SearchReturnUpdate)
			if err != nil || data.Emulated || data.ContextTag == "" {
				t.Fatalf("UPDATE = %#v, %v", data, err)
			}
		})
	}
}

// An update for a tag nobody registered is discarded even when malformed: a
// connection that never asked for UPDATE must not fail over one.
func TestContextUnregisteredMalformedUpdatesAreDiscarded(t *testing.T) {
	var updates int
	c, ctx := newRoutingTestClient(t, contextCapabilities, &Options{UnilateralData: &UnilateralDataHandler{
		ContextUpdate: func(*ContextUpdate) { updates++ },
	}}, func(s *extAServer) {
		tag, _ := s.command()
		s.reply(`* ESEARCH (TAG "zz") ADDTO (0 0)`,
			`* ESEARCH ADDTO (0 1)`,
			`* ESEARCH (TAG "zz" TAG "zz") REMOVEFROM (0 1)`,
			`* ESEARCH (TAG "zz" MAILBOX "INBOX") ADDTO (0 1)`,
			`* ESEARCH (TAG "zz") UID REMOVEFROM (0 1)`,
			tag+" OK fence")
	})
	if err := c.Noop(nil).Wait(ctx); err != nil {
		t.Fatalf("connection failed over an unregistered update: %v", err)
	}
	if updates != 0 {
		t.Fatalf("delivered %d updates without a registration", updates)
	}
}
