package imapclient

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

func TestSortExtendedWireFormAndOrder(t *testing.T) {
	var sent string
	c, _ := extCDial(t, func(tag, line string) string {
		sent = line
		// RFC 5267 section 3.1: MIN and MAX are positions in the sort order
		// and ALL lists the matches in that order, so 7 sorts before 2.
		return `* ESEARCH (TAG "` + tag + `") UID MIN 7 MAX 3 COUNT 3 ALL 7,2,3` + "\r\n" + tag + " OK sorted\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "SORT", "ESORT"}, nil, true)
	data, err := c.SortExtendedUID(extCContext(t), []SortKeySpec{{Key: SortKeySubject, Reverse: true}}, imap.SearchAll,
		&ESortOptions{ReturnOptions: []SearchReturnOption{SearchReturnMin, SearchReturnMax, SearchReturnCount, SearchReturnAll}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, "UID SORT RETURN (MIN MAX COUNT ALL) (REVERSE SUBJECT) UTF-8 ALL") {
		t.Fatalf("sent = %q", sent)
	}
	if data.Emulated || data.Min != 7 || data.Max != 3 || data.Count != 3 {
		t.Fatalf("data = %#v", data)
	}
	if got := data.AllUIDs.String(); got != "7,2,3" {
		t.Fatalf("ALL = %q, want the sort order preserved as 7,2,3", got)
	}
}

func TestSortExtendedEmulatedKeepsSortOrder(t *testing.T) {
	var sent string
	c, _ := extCDial(t, func(tag, line string) string {
		sent = line
		return "* SORT 5 1 4\r\n" + tag + " OK sorted\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "SORT"}, nil, true)
	data, err := c.SortExtended(extCContext(t), []SortKeySpec{{Key: SortKeyDate}}, imap.SearchAll,
		&ESortOptions{ReturnOptions: []SearchReturnOption{SearchReturnMin, SearchReturnMax, SearchReturnAll}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sent, "RETURN") {
		t.Fatalf("emulation sent a RETURN list to a server without ESORT: %q", sent)
	}
	if !data.Emulated || data.Min != 5 || data.Max != 4 || data.HasCount {
		t.Fatalf("data = %#v", data)
	}
	if got := data.All.String(); got != "5,1,4" {
		t.Fatalf("ALL = %q, want the sort order preserved as 5,1,4", got)
	}
}

func TestSortExtendedRefusesWhatCannotBeEmulated(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1", "SORT"}, nil, true)
	_, err := c.SortExtended(extCContext(t), []SortKeySpec{{Key: SortKeyDate}}, imap.SearchAll,
		&ESortOptions{ReturnOptions: []SearchReturnOption{SearchReturnOptionKeyword("X-FUTURE")}})
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.SortExtended(extCContext(t), []SortKeySpec{{Key: SortKeyDate}}, imap.SearchAll,
		&ESortOptions{ReturnOptions: []SearchReturnOption{SearchReturnSave}}); err == nil {
		t.Fatal("RETURN (SAVE) was accepted")
	}
	extCReady(c, []string{"IMAP4REV1", "SORT", "ESORT", "CONTEXT=SORT"}, nil, true)
	if _, err := c.SortExtended(extCContext(t), []SortKeySpec{{Key: SortKeyDate}}, imap.SearchAll,
		&ESortOptions{ReturnOptions: []SearchReturnOption{SearchReturnCount, SearchReturnUpdate}}); err == nil || !strings.Contains(err.Error(), "UPDATE") {
		t.Fatalf("RETURN (UPDATE) error = %v", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("sent: %q", server.Lines())
	}
}

func TestSortExtendedRequiresSort(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1"}, nil, true)
	_, err := c.SortExtended(extCContext(t), []SortKeySpec{{Key: SortKeyDate}}, nil, nil)
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("sent: %q", server.Lines())
	}
}

// TestPlainSortPipelinesWithExtendedSearch pins v1.1 behaviour that the
// extended-search exclusion must not take away: a plain SORT is answered by
// an untagged SORT, not ESEARCH, so an extended SEARCH may run beside it.
func TestPlainSortPipelinesWithExtendedSearch(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 ESEARCH SORT ESORT] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		sortTag, _ := s.command()
		searchTag, _ := s.command()
		s.reply(`* ESEARCH (TAG "`+searchTag+`") UID COUNT 2`, searchTag+" OK searched")
		s.reply("* SORT 2 1", sortTag+" OK sorted")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	sorted := make(chan error, 1)
	go func() {
		_, err := c.SortUID(ctx, []SortKeySpec{{Key: SortKeyDate}}, imap.SearchAll, nil)
		sorted <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !c.pendingNamed("UID SORT") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.searchPending() {
		t.Fatal("a plain SORT counts as a pending extended search")
	}
	data, err := c.SearchExtendedUID(imap.SearchAll, nil).Wait(ctx)
	if err != nil {
		t.Fatalf("extended SEARCH beside a plain SORT: %v", err)
	}
	if !data.HasCount || data.Count != 2 {
		t.Fatalf("data = %#v", data)
	}
	if err := <-sorted; err != nil {
		t.Fatal(err)
	}
}

// pendingNamed reports whether a command with this name awaits completion.
func (c *Client) pendingNamed(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cmd := range c.pendingQ {
		if cmd.name == name {
			return true
		}
	}
	return false
}
