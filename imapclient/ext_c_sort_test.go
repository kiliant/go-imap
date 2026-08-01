package imapclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestSortWireFormAndResponse(t *testing.T) {
	var sent string
	c, _ := extCDial(t, func(tag, line string) string {
		sent = line
		return "* SORT 3 1 2\r\n" + tag + " OK sorted\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "SORT"}, nil, true)
	data, err := c.Sort(extCContext(t), []SortKeySpec{{Key: SortKeyDate}, {Key: SortKeySubject, Reverse: true}}, imap.SearchAll, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, "SORT (DATE REVERSE SUBJECT) UTF-8 ALL") {
		t.Fatalf("sent = %q", sent)
	}
	if data.Emulated || len(data.SeqNums) != 3 || data.SeqNums[0] != 3 {
		t.Fatalf("data = %#v", data)
	}
}

func TestSortUID(t *testing.T) {
	c, _ := extCDial(t, func(tag, line string) string {
		return "* SORT 10 20\r\n" + tag + " OK sorted\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "SORT"}, nil, true)
	data, err := c.SortUID(extCContext(t), []SortKeySpec{{Key: SortKeyArrival}}, imap.SearchSeen, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.UIDs) != 2 || data.UIDs[0] != 10 {
		t.Fatalf("data = %#v", data)
	}
}

func TestSortDisplayRequiresCapability(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1", "SORT"}, nil, true)
	_, err := c.Sort(extCContext(t), []SortKeySpec{{Key: SortKeyDisplayFrom}}, imap.SearchAll, nil)
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("sent: %q", server.Lines())
	}
}

func TestSortClientFallback(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string {
		switch {
		case strings.Contains(line, "SEARCH"):
			return "* SEARCH 2 1\r\n" + tag + " OK\r\n"
		case strings.Contains(line, "FETCH"):
			return "* 1 FETCH (UID 1 RFC822.SIZE 10 INTERNALDATE \"01-Jun-2024 12:00:00 +0000\")\r\n" +
				"* 2 FETCH (UID 2 RFC822.SIZE 20 INTERNALDATE \"02-Jun-2024 12:00:00 +0000\")\r\n" +
				tag + " OK\r\n"
		default:
			return tag + " BAD\r\n"
		}
	})
	extCReady(c, []string{"IMAP4REV1"}, nil, true)
	data, err := c.Sort(extCContext(t), []SortKeySpec{{Key: SortKeyArrival}}, imap.SearchAll, &SortOptions{AllowClientFallback: true})
	if err != nil {
		t.Fatal(err)
	}
	if !data.Emulated || len(data.SeqNums) != 2 || data.SeqNums[0] != 1 || data.SeqNums[1] != 2 {
		t.Fatalf("data = %#v", data)
	}
	if len(server.Lines()) < 2 {
		t.Fatalf("lines = %q", server.Lines())
	}
}

func TestSortRelevancyNotEmulated(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1", "SEARCH=FUZZY"}, nil, true)
	_, err := c.Sort(extCContext(t), []SortKeySpec{{Key: SortKeyRelevancy}}, imap.SearchAll, &SortOptions{AllowClientFallback: true})
	if err == nil {
		t.Fatal("RELEVANCY must not be silently emulated")
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("sent: %q", server.Lines())
	}
}

func TestSortRequiresCapabilityWithoutFallback(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1"}, nil, true)
	_, err := c.Sort(extCContext(t), []SortKeySpec{{Key: SortKeyDate}}, imap.SearchAll, nil)
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("sent: %q", server.Lines())
	}
}

func TestThreadReferences(t *testing.T) {
	var sent string
	c, _ := extCDial(t, func(tag, line string) string {
		sent = line
		return "* THREAD (2)(3 6 (4 23)(44 7 96))\r\n" + tag + " OK\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "THREAD=REFERENCES"}, nil, true)
	data, err := c.Thread(extCContext(t), ThreadReferences, imap.SearchAll, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, "THREAD REFERENCES UTF-8 ALL") {
		t.Fatalf("sent = %q", sent)
	}
	if len(data.Roots) != 2 || data.Roots[0].Num != 2 {
		t.Fatalf("roots = %#v", data.Roots)
	}
	second := data.Roots[1]
	if second.Num != 3 || len(second.Children) != 1 || second.Children[0].Num != 6 {
		t.Fatalf("second = %#v", second)
	}
	six := second.Children[0]
	if len(six.Children) != 2 || six.Children[0].Num != 4 || six.Children[1].Num != 44 {
		t.Fatalf("six = %#v", six)
	}
}

func TestThreadEmpty(t *testing.T) {
	c, _ := extCDial(t, func(tag, line string) string {
		return "* THREAD\r\n" + tag + " OK\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "THREAD=ORDEREDSUBJECT"}, nil, true)
	data, err := c.ThreadUID(extCContext(t), ThreadOrderedSubject, imap.SearchAll, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !data.UID || len(data.Roots) != 0 {
		t.Fatalf("data = %#v", data)
	}
}

func TestThreadRequiresCapability(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1"}, nil, true)
	_, err := c.Thread(extCContext(t), ThreadReferences, imap.SearchAll, nil)
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("sent: %q", server.Lines())
	}
}
