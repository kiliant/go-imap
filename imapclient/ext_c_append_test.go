package imapclient

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

func TestMultiAppendWritesTwoLiterals(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string {
		return tag + " OK [APPENDUID 1 10:11] appended\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "MULTIAPPEND", "UIDPLUS"}, nil, true)
	msg1 := "Subject: one\r\n\r\nbody1\r\n"
	msg2 := "Subject: two\r\n\r\nbody2\r\n"
	data, err := c.MultiAppend(extCContext(t), "INBOX", []AppendMessage{
		{Flags: []imap.Flag{imap.FlagSeen}, Size: int64(len(msg1)), Literal: strings.NewReader(msg1)},
		{Size: int64(len(msg2)), Literal: strings.NewReader(msg2)},
	}).Wait(extCContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if data.UIDValidity != 1 || !data.UIDs.Equal(imap.UIDSet{{Start: 10, Stop: 11}}) {
		t.Fatalf("data = %#v", data)
	}
	lits := server.Literals()
	if len(lits) != 2 || string(lits[0]) != msg1 || string(lits[1]) != msg2 {
		t.Fatalf("literals = %#v", lits)
	}
	if !strings.Contains(server.LastLine(), "APPEND INBOX") {
		t.Fatalf("line = %q", server.LastLine())
	}
}

func TestMultiAppendRequiresCapability(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1"}, nil, true)
	_, err := c.MultiAppend(extCContext(t), "INBOX", []AppendMessage{
		{Size: 3, Literal: strings.NewReader("a\r\n")},
		{Size: 3, Literal: strings.NewReader("b\r\n")},
	}).Wait(extCContext(t))
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("command was sent: %q", server.Lines())
	}
}

func TestCatenateAppendWireForm(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string {
		return tag + " OK [APPENDUID 9 42] done\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "CATENATE", "UIDPLUS"}, nil, true)
	text := "From: a@b\r\n\r\n"
	data, err := c.CatenateAppend(extCContext(t), "INBOX", []CatenatePart{
		{Text: &CatenateText{Size: int64(len(text)), Literal: strings.NewReader(text)}},
		{URL: "/INBOX/;UID=1"},
	}, nil).Wait(extCContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if data.UIDValidity != 9 || !data.UIDs.Contains(imap.UID(42)) {
		t.Fatalf("data = %#v", data)
	}
	line := server.LastLine()
	if !strings.Contains(line, "CATENATE (TEXT {") || !strings.Contains(line, "URL /INBOX/;UID=1") {
		t.Fatalf("line = %q", line)
	}
}

func TestCatenateRequiresCapability(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1"}, nil, true)
	_, err := c.CatenateAppend(extCContext(t), "INBOX", []CatenatePart{
		{URL: "/INBOX/;UID=1"},
	}, nil).Wait(extCContext(t))
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("command was sent: %q", server.Lines())
	}
}

func TestCatenateRejectsMixedPart(t *testing.T) {
	c, _ := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1", "CATENATE"}, nil, true)
	_, err := c.CatenateAppend(extCContext(t), "INBOX", []CatenatePart{
		{URL: "/x", Text: &CatenateText{Size: 1, Literal: strings.NewReader("x")}},
	}, nil).Wait(extCContext(t))
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestMultiAppendInternalDate(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK done\r\n" })
	extCReady(c, []string{"IMAP4REV1"}, nil, true)
	when := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	msg := "x\r\n"
	if _, err := c.MultiAppend(extCContext(t), "INBOX", []AppendMessage{
		{InternalDate: &when, Size: int64(len(msg)), Literal: strings.NewReader(msg)},
	}).Wait(extCContext(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(server.LastLine(), "1-Jun-2024") {
		t.Fatalf("line = %q", server.LastLine())
	}
}
