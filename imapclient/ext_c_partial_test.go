package imapclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestSearchPartialWireAndParse(t *testing.T) {
	var sent string
	c, _ := extCDial(t, func(tag, line string) string {
		sent = line
		return `* ESEARCH (TAG "` + tag + `") UID PARTIAL (1:500 2,10:11)` + "\r\n" + tag + " OK\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "ESEARCH", "PARTIAL"}, nil, true)
	data, partial, _, err := c.SearchPartialUID(extCContext(t), imap.SearchAll, &PartialSearchOptions{
		Range: PartialRange{FirstStart: 1, FirstEnd: 500},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, "UID SEARCH RETURN (PARTIAL 1:500)") {
		t.Fatalf("sent = %q", sent)
	}
	if data == nil || partial == nil || !partial.HasResults {
		t.Fatalf("data=%#v partial=%#v", data, partial)
	}
	if partial.Range.FirstStart != 1 || partial.Range.FirstEnd != 500 {
		t.Fatalf("range = %#v", partial.Range)
	}
	if !partial.AllUIDs.Equal(imap.UIDSet{{Start: 2, Stop: 2}, {Start: 10, Stop: 11}}) {
		t.Fatalf("uids = %#v", partial.AllUIDs)
	}
}

func TestSearchPartialNilWindow(t *testing.T) {
	c, _ := extCDial(t, func(tag, line string) string {
		return `* ESEARCH (TAG "` + tag + `") PARTIAL (-1:-100 NIL)` + "\r\n" + tag + " OK\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "ESEARCH", "PARTIAL"}, nil, true)
	_, partial, _, err := c.SearchPartial(extCContext(t), imap.SearchAll, &PartialSearchOptions{
		Range: PartialRange{LastStart: 1, LastEnd: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	if partial == nil || partial.HasResults {
		t.Fatalf("partial = %#v", partial)
	}
}

func TestSearchPartialRequiresCapability(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1", "ESEARCH"}, nil, true)
	_, _, _, err := c.SearchPartial(extCContext(t), imap.SearchAll, &PartialSearchOptions{
		Range: PartialRange{FirstStart: 1, FirstEnd: 10},
	})
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("sent: %q", server.Lines())
	}
}

func TestSearchPartialRejectsAll(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1", "ESEARCH", "PARTIAL"}, nil, true)
	_, _, _, err := c.SearchPartial(extCContext(t), imap.SearchAll, &PartialSearchOptions{
		Range:         PartialRange{FirstStart: 1, FirstEnd: 10},
		ReturnOptions: []SearchReturnOption{SearchReturnAll},
	})
	if err == nil {
		t.Fatal("expected ALL rejection")
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("sent: %q", server.Lines())
	}
}

func TestSearchPartialCompanionReturn(t *testing.T) {
	var sent string
	c, _ := extCDial(t, func(tag, line string) string {
		sent = line
		return `* ESEARCH (TAG "` + tag + `") UID COUNT 2 PARTIAL (1:10 2,10)` + "\r\n" + tag + " OK\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "ESEARCH", "PARTIAL"}, nil, true)
	_, partial, _, err := c.SearchPartialUID(extCContext(t), imap.SearchAll, &PartialSearchOptions{
		Range:         PartialRange{FirstStart: 1, FirstEnd: 10},
		ReturnOptions: []SearchReturnOption{SearchReturnCount, SearchReturnMin},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, "UID SEARCH RETURN (PARTIAL 1:10 COUNT MIN)") {
		t.Fatalf("sent = %q", sent)
	}
	if partial == nil || !partial.HasResults {
		t.Fatalf("partial = %#v", partial)
	}
}

func TestFetchUIDPartial(t *testing.T) {
	var sent string
	c, _ := extCDial(t, func(tag, line string) string {
		sent = line
		return "* 1 FETCH (UID 100 FLAGS (\\Seen))\r\n" + tag + " OK\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "PARTIAL"}, nil, true)
	cmd := c.FetchUIDPartial(imap.UIDSet{{Start: 1, Stop: 1000}}, &PartialFetchOptions{Range: PartialRange{LastStart: 1, LastEnd: 3}}, imap.FetchItemUID, imap.FetchItemFlags)
	msg, err := cmd.Next(extCContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(extCContext(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, "UID FETCH 1:1000 (UID FLAGS) (PARTIAL -1:-3)") {
		t.Fatalf("sent = %q", sent)
	}
	if msg.SeqNum != 1 {
		t.Fatalf("msg = %#v", msg)
	}
}

func TestMultiSearchParsesPerMailbox(t *testing.T) {
	var sent string
	c, _ := extCDial(t, func(tag, line string) string {
		sent = line
		return `* ESEARCH (TAG "` + tag + `" MAILBOX "folder1" UIDVALIDITY 1) UID ALL 4001,4003` + "\r\n" +
			`* ESEARCH (TAG "` + tag + `" MAILBOX "folder2" UIDVALIDITY 503) UID ALL 3002` + "\r\n" +
			tag + " OK done\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "MULTISEARCH", "ESEARCH"}, nil, true)
	data, err := c.MultiSearch(imap.SearchUnseen, &MultiSearchOptions{
		Sources: []MultiSearchSource{
			MultiSearchMailboxes{Names: []string{"folder1"}},
			MultiSearchSubtree{Mailbox: "folder2"},
		},
		ReturnOptions: []SearchReturnOption{SearchReturnAll},
	}).Wait(extCContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, `ESEARCH IN (mailboxes folder1 subtree folder2) RETURN (ALL) UNSEEN`) {
		t.Fatalf("sent = %q", sent)
	}
	if len(data.Results) != 2 {
		t.Fatalf("results = %#v", data.Results)
	}
	if data.Results[0].Mailbox != "folder1" || data.Results[0].UIDValidity != 1 {
		t.Fatalf("r0 = %#v", data.Results[0])
	}
	if data.Results[1].Mailbox != "folder2" || !data.Results[1].Data.HasAll {
		t.Fatalf("r1 = %#v", data.Results[1])
	}
}

func TestMultiSearchRequiresCapability(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1"}, nil, true)
	_, err := c.MultiSearch(imap.SearchAll, nil).Wait(extCContext(t))
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("sent: %q", server.Lines())
	}
}

func TestMultiSearchSelectedPointerAllowsSave(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string {
		return `* ESEARCH (TAG "` + tag + `" MAILBOX "INBOX" UIDVALIDITY 1) UID ALL 1` + "\r\n" + tag + " OK\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "MULTISEARCH", "ESEARCH", "SEARCHRES"}, nil, true)
	cmd := c.MultiSearch(imap.SearchAll, &MultiSearchOptions{
		Sources:       []MultiSearchSource{&MultiSearchSelected{}},
		ReturnOptions: []SearchReturnOption{SearchReturnAll, SearchReturnSave},
	})
	_, err := cmd.Wait(extCContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(server.LastLine(), "RETURN (ALL SAVE)") {
		t.Fatalf("line = %q", server.LastLine())
	}
	if cmd.SavedResult() == nil {
		t.Fatal("SAVE requested but SavedResult is nil")
	}
}

func TestSearchFuzzyRequiresCapability(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1"}, nil, true)
	err := c.SearchFuzzy(imap.SearchString{Key: imap.SearchKeySubject, Value: "hello"}, nil).Wait(extCContext(t))
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("sent: %q", server.Lines())
	}
}

func TestSearchFuzzyWireForm(t *testing.T) {
	var sent string
	c, _ := extCDial(t, func(tag, line string) string {
		sent = line
		return "* SEARCH 1 5\r\n" + tag + " OK\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "SEARCH=FUZZY"}, nil, true)
	nums, err := c.SearchFuzzy(imap.SearchString{Key: imap.SearchKeySubject, Value: "IMAP"}, nil).All(extCContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, `SEARCH FUZZY SUBJECT "IMAP"`) {
		t.Fatalf("sent = %q", sent)
	}
	if len(nums) != 2 {
		t.Fatalf("nums = %#v", nums)
	}
}

func TestParseRelevancyScores(t *testing.T) {
	scores, err := (imap.ESearchData{Values: map[imap.ESearchReturnKey]string{
		ESearchReturnKeyRelevancy: "(4 99 42)",
	}}).RelevancyScores()
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 3 || scores[1] != 99 {
		t.Fatalf("scores = %#v", scores)
	}
}

func TestEnableUTF8Accept(t *testing.T) {
	c, _ := extCDial(t, func(tag, line string) string {
		if strings.Contains(line, "ENABLE") {
			return "* ENABLED UTF8=ACCEPT\r\n" + tag + " OK\r\n"
		}
		return tag + " BAD\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "ENABLE", "UTF8=ACCEPT"}, nil, false)
	enabled, err := c.EnableUTF8Accept(nil).Wait(extCContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 1 || enabled[0] != "UTF8=ACCEPT" {
		t.Fatalf("enabled = %#v", enabled)
	}
	if !c.UTF8AcceptEnabled() || !c.UTF8AppendAllowed() {
		t.Fatal("UTF8 state not updated")
	}
}

func TestUTF8CapabilityConstants(t *testing.T) {
	for _, name := range []string{CapabilityUTF8Accept, CapabilityUTF8All, CapabilityUTF8Append, CapabilityUTF8Only, CapabilityUTF8User} {
		if !utf8CapabilityKnown(name) {
			t.Fatalf("unknown %q", name)
		}
	}
}
