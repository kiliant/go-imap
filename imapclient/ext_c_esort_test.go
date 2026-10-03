package imapclient

import (
	"errors"
	"strings"
	"testing"

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
