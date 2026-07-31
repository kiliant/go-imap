package imapclient

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

const esearchCapabilities = "* PREAUTH [CAPABILITY IMAP4REV1 ESEARCH SEARCHRES WITHIN] ready"

func TestSearchExtendedParsesEveryReturnItem(t *testing.T) {
	var sent string
	c, ctx := newExtATestClient(t, esearchCapabilities, func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, rest := s.command()
		sent = rest
		s.reply(
			`* ESEARCH (TAG "`+tag+`") UID MIN 7 MAX 3800 COUNT 3 ALL 2,10:11 MODSEQ 917162488 X-VENDOR ("a" "b")`,
			tag+" OK searched")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.SearchExtendedUID(imap.SearchAnd{imap.SearchFlagged}, &ESearchOptions{
		ReturnOptions: []SearchReturnOption{SearchReturnMin, SearchReturnMax, SearchReturnCount, SearchReturnAll},
	}).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// RFC 4466 section 2.6 puts RETURN before the search-program, and the
	// CHARSET belongs to the search-program.
	if sent != "UID SEARCH RETURN (MIN MAX COUNT ALL) FLAGGED" {
		t.Fatalf("wire form = %q", sent)
	}
	if !data.UID || data.Tag == "" {
		t.Fatalf("data = %#v", data)
	}
	if !data.HasMin || data.Min != 7 || !data.HasMax || data.Max != 3800 {
		t.Fatalf("MIN/MAX = %#v", data)
	}
	if !data.HasCount || data.Count != 3 {
		t.Fatalf("COUNT = %#v", data)
	}
	if !data.HasAll || !data.AllUIDs.Equal(imap.UIDSet{{Start: 2, Stop: 2}, {Start: 10, Stop: 11}}) {
		t.Fatalf("ALL = %#v", data.AllUIDs)
	}
	if !data.HasModSeq || data.ModSeq != 917162488 {
		t.Fatalf("MODSEQ = %#v", data)
	}
	// An item this package does not model must survive verbatim rather than
	// being dropped.
	if got := data.Values["X-VENDOR"]; got != `("a" "b")` {
		t.Fatalf("unmodelled X-VENDOR value = %q", got)
	}
	if data.Emulated {
		t.Fatal("Emulated set on a native ESEARCH")
	}
}

// TestSearchExtendedEmptyResultOmitsMinMaxAll pins the absent-versus-zero
// distinction RFC 4731 section 3.1 requires.
func TestSearchExtendedEmptyResultOmitsMinMaxAll(t *testing.T) {
	c, ctx := newExtATestClient(t, esearchCapabilities, func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, _ := s.command()
		s.reply(`* ESEARCH (TAG "`+tag+`") COUNT 0`, tag+" OK searched")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.SearchExtended(imap.SearchDeleted, &ESearchOptions{
		ReturnOptions: []SearchReturnOption{SearchReturnMin, SearchReturnCount},
	}).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if data.HasMin || data.HasMax || data.HasAll {
		t.Fatalf("MIN/MAX/ALL reported for an empty result: %#v", data)
	}
	if !data.HasCount || data.Count != 0 {
		t.Fatalf("COUNT = %d has=%t, want 0 present", data.Count, data.HasCount)
	}
}

func TestSearchExtendedRejectsForeignCorrelator(t *testing.T) {
	c, ctx := newExtATestClient(t, esearchCapabilities, func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, _ := s.command()
		s.reply(`* ESEARCH (TAG "ZZZZ") COUNT 1`, tag+" OK searched")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SearchExtended(imap.SearchAll, nil).Wait(ctx); err == nil {
		t.Fatal("an ESEARCH response for another tag was accepted")
	}
}

func TestSearchExtendedEmulatesWithoutESEARCH(t *testing.T) {
	var sent string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, rest := s.command()
		sent = rest
		s.reply("* SEARCH 11 2 10", tag+" OK searched")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.SearchExtended(imap.SearchFlagged, &ESearchOptions{
		ReturnOptions: []SearchReturnOption{SearchReturnMin, SearchReturnMax, SearchReturnCount, SearchReturnAll},
	}).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sent != "SEARCH FLAGGED" {
		t.Fatalf("fallback wire form = %q", sent)
	}
	if !data.Emulated {
		t.Fatal("Emulated not set on the fallback path")
	}
	if !data.HasMin || data.Min != 2 || !data.HasMax || data.Max != 11 {
		t.Fatalf("computed MIN/MAX = %#v", data)
	}
	if !data.HasCount || data.Count != 3 {
		t.Fatalf("computed COUNT = %#v", data)
	}
	// RFC 4731 section 3.1: ALL is a sequence set, so the flat list has to be
	// coalesced and ordered rather than echoed.
	if !data.HasAll || data.All.String() != "2,10:11" {
		t.Fatalf("computed ALL = %q", data.All.String())
	}
	if data.UID {
		t.Fatal("UID set on a sequence-number search")
	}
}

func TestSearchExtendedEmulationOnEmptyResult(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, _ := s.command()
		s.reply("* SEARCH", tag+" OK searched")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.SearchExtended(imap.SearchDeleted, &ESearchOptions{
		ReturnOptions: []SearchReturnOption{SearchReturnMin, SearchReturnMax, SearchReturnAll, SearchReturnCount},
	}).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if data.HasMin || data.HasMax || data.HasAll {
		t.Fatalf("emulated empty result reported MIN/MAX/ALL: %#v", data)
	}
	if !data.HasCount || data.Count != 0 {
		t.Fatalf("emulated COUNT = %d has=%t", data.Count, data.HasCount)
	}
}

func TestSearchExtendedEmulationDefaultsToAll(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, _ := s.command()
		s.reply("* SEARCH 4 5", tag+" OK searched")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.SearchExtended(imap.SearchAll, nil).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !data.HasAll || data.All.String() != "4:5" || data.HasCount {
		t.Fatalf("empty RETURN list is not equivalent to (ALL): %#v", data)
	}
}

func TestSearchExtendedSaveRequiresSEARCHRES(t *testing.T) {
	sawSearch := false
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 ESEARCH] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		if tag, _ := s.command(); tag != "" {
			sawSearch = true
			s.ok(tag)
		}
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := c.SearchExtended(imap.SearchAll, &ESearchOptions{
		ReturnOptions: []SearchReturnOption{SearchReturnSave},
	}).Wait(ctx)
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("SAVE without SEARCHRES error = %v", err)
	}
	if sawSearch {
		t.Fatal("a SEARCH reached the wire although SEARCHRES is absent")
	}
}

func TestSearchExtendedGatesWithinKeys(t *testing.T) {
	sawSearch := false
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 ESEARCH] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		if tag, _ := s.command(); tag != "" {
			sawSearch = true
			s.ok(tag)
		}
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	criteria := imap.SearchAnd{
		imap.SearchUnseen,
		imap.SearchNot{Criteria: imap.SearchWithin{Key: imap.SearchWithinKeyYounger, Seconds: 259200}},
	}
	_, err := c.SearchExtended(criteria, nil).Wait(ctx)
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("WITHIN key without the WITHIN capability: %v", err)
	}
	if sawSearch {
		t.Fatal("a SEARCH reached the wire although WITHIN is absent")
	}
}

func TestSearchExtendedSendsWithinKeys(t *testing.T) {
	var sent string
	c, ctx := newExtATestClient(t, esearchCapabilities, func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, rest := s.command()
		sent = rest
		s.reply(`* ESEARCH (TAG "`+tag+`") COUNT 2`, tag+" OK searched")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	criteria := imap.SearchAnd{
		imap.SearchUnseen,
		imap.SearchWithin{Key: imap.SearchWithinKeyYounger, Seconds: 259200},
	}
	if _, err := c.SearchExtended(criteria, &ESearchOptions{
		ReturnOptions: []SearchReturnOption{SearchReturnCount},
	}).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if sent != "SEARCH RETURN (COUNT) UNSEEN YOUNGER 259200" {
		t.Fatalf("WITHIN wire form = %q", sent)
	}
}

func TestSavedSearchResultLifecycle(t *testing.T) {
	var saveCommand string
	c, ctx := newExtATestClient(t, esearchCapabilities, func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, rest := s.command()
		saveCommand = rest
		s.reply(`* ESEARCH (TAG "`+tag+`") MIN 2`, tag+" OK saved")
		// A second SELECT of a different mailbox resets "$".
		tag, _ = s.command()
		s.reply("* 0 EXISTS", "* OK [UIDVALIDITY 202] valid", tag+" OK [READ-WRITE] selected")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	cmd := c.SearchExtended(imap.SearchFlagged, &ESearchOptions{
		ReturnOptions: []SearchReturnOption{SearchReturnSave, SearchReturnMin},
	})
	if _, err := cmd.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if saveCommand != "SEARCH RETURN (SAVE MIN) FLAGGED" {
		t.Fatalf("SAVE wire form = %q", saveCommand)
	}
	saved := cmd.SavedResult()
	if saved == nil {
		t.Fatal("SavedResult() = nil after RETURN (SAVE)")
	}
	if saved.Mailbox() != "INBOX" || saved.UID() {
		t.Fatalf("saved = %q uid=%t", saved.Mailbox(), saved.UID())
	}
	if !saved.Valid() {
		t.Fatal("Valid() = false immediately after saving")
	}
	if _, err := c.Select("Archive", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if saved.Valid() {
		t.Fatal("Valid() = true after selecting a different mailbox")
	}
	if (*SavedSearchResult)(nil).Valid() || (*SavedSearchResult)(nil).UID() {
		t.Fatal("nil SavedSearchResult reported as valid")
	}
}

func TestSearchExtendedWithoutSaveHasNoHandle(t *testing.T) {
	c, ctx := newExtATestClient(t, esearchCapabilities, func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, _ := s.command()
		s.reply(`* ESEARCH (TAG "`+tag+`") COUNT 0`, tag+" OK searched")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	cmd := c.SearchExtended(imap.SearchAll, &ESearchOptions{ReturnOptions: []SearchReturnOption{SearchReturnCount}})
	if _, err := cmd.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if cmd.SavedResult() != nil {
		t.Fatal("SavedResult() is non-nil without RETURN (SAVE)")
	}
	if (*ESearchCommand)(nil).SavedResult() != nil {
		t.Fatal("nil command returned a saved result")
	}
	if _, err := (*ESearchCommand)(nil).Wait(ctx); err == nil {
		t.Fatal("nil command waited successfully")
	}
}

// TestSearchExtendedRefusesPipelinedSearch documents the deliberate
// restriction: the collector chain cannot demultiplex two correlated ESEARCH
// responses, so the second command is refused rather than risking one command's
// matches being delivered to another.
func TestSearchExtendedRefusesPipelinedSearch(t *testing.T) {
	release := make(chan struct{})
	c, ctx := newExtATestClient(t, esearchCapabilities, func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, _ := s.command()
		<-release
		s.reply(`* ESEARCH (TAG "`+tag+`") COUNT 1`, tag+" OK searched")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	first := c.SearchExtended(imap.SearchAll, nil)
	// The first command is on the wire and not yet answered.
	deadline := time.Now().Add(2 * time.Second)
	for !c.searchPending() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	second := c.SearchExtended(imap.SearchDeleted, nil)
	_, err := second.Wait(ctx)
	if err == nil || !strings.Contains(err.Error(), "pipelined") {
		t.Fatalf("second extended SEARCH error = %v", err)
	}
	close(release)
	if _, err := first.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSearchReturnKeywordsRejectsUnknownType(t *testing.T) {
	if _, _, err := searchReturnKeywords([]SearchReturnOption{badReturnOption{}}); err == nil {
		t.Fatal("an unmodelled SearchReturnOption implementation was accepted")
	}
	if _, _, err := searchReturnKeywords([]SearchReturnOption{SearchReturnOptionKeyword("BAD OPTION")}); err == nil {
		t.Fatal("a keyword that is not an atom was accepted")
	}
}

type badReturnOption struct{}

func (badReturnOption) searchReturnOption() {}
