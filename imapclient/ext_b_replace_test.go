package imapclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

const replaceMessage = "Subject: replacement\r\n\r\nbody\r\n"

func TestReplaceUID(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string {
		return "* OK [APPENDUID 1 2001] Replacement Message ready\r\n" +
			"* 5 EXISTS\r\n* 4 EXPUNGE\r\n" +
			tag + " OK Replace completed\r\n"
	})
	extBReady(c, []string{"IMAP4REV1", "REPLACE", "UIDPLUS"}, nil, true)
	data, err := c.ReplaceUID(extBContext(t), 2000, "Drafts",
		&ReplaceOptions{Flags: []imap.Flag{imap.FlagSeen, imap.FlagDraft}},
		int64(len(replaceMessage)), strings.NewReader(replaceMessage))
	if err != nil {
		t.Fatal(err)
	}
	if data.UIDValidity != 1 || data.UID != 2001 {
		t.Fatalf("data = %#v", data)
	}
	if data.Emulated {
		t.Error("a real REPLACE was reported as emulated")
	}
	line := server.LastLine()
	if !strings.Contains(line, "UID REPLACE 2000 Drafts (\\Seen \\Draft) {") {
		t.Fatalf("command line = %q", line)
	}
	literals := server.Literals()
	if len(literals) != 1 || string(literals[0]) != replaceMessage {
		t.Fatalf("literal payload = %q", literals)
	}
}

func TestReplaceSequenceNumber(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string { return tag + " OK Replace completed\r\n" })
	extBReady(c, []string{"IMAP4REV1", "REPLACE"}, nil, true)
	data, err := c.Replace(extBContext(t), 4, "Drafts", nil, int64(len(replaceMessage)), strings.NewReader(replaceMessage))
	if err != nil {
		t.Fatal(err)
	}
	if data.UID != 0 || data.UIDValidity != 0 {
		t.Errorf("APPENDUID reported although the server sent none: %#v", data)
	}
	if !strings.Contains(server.LastLine(), "REPLACE 4 Drafts {") {
		t.Fatalf("command line = %q", server.LastLine())
	}
}

func TestReplaceRequiresSelectedState(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string { return tag + " OK done\r\n" })
	extBReady(c, []string{"IMAP4REV1", "REPLACE"}, nil, false)
	_, err := c.Replace(extBContext(t), 1, "Drafts", nil, int64(len(replaceMessage)), strings.NewReader(replaceMessage))
	if err == nil {
		t.Fatal("REPLACE is only valid in the selected state (RFC 8508 section 3.5)")
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("a command was sent from the wrong state: %q", server.Lines())
	}
}

func TestReplaceWithoutCapabilityIsRefused(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string { return tag + " OK done\r\n" })
	extBReady(c, []string{"IMAP4REV1", "UIDPLUS"}, nil, true)
	_, err := c.ReplaceUID(extBContext(t), 1, "Drafts", nil, int64(len(replaceMessage)), strings.NewReader(replaceMessage))
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v, want ErrCapabilityNotAdvertised", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("REPLACE was sent to a server that never advertised it: %q", server.Lines())
	}
}

// TestReplaceEmulatedFallback exercises the RFC 8508 section 3.4 equivalence
// used when the server has no REPLACE. The append must come first so that a
// failure between steps can only duplicate the message, never lose it.
func TestReplaceEmulatedFallback(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string {
		switch {
		case strings.Contains(line, "APPEND"):
			return tag + " OK [APPENDUID 1 3002] APPEND complete\r\n"
		case strings.Contains(line, "UID STORE"):
			return tag + " OK stored\r\n"
		case strings.Contains(line, "UID EXPUNGE"):
			return "* VANISHED 2000\r\n" + tag + " OK expunged\r\n"
		}
		return tag + " BAD unexpected\r\n"
	})
	extBReady(c, []string{"IMAP4REV1", "UIDPLUS"}, nil, true)
	data, err := c.ReplaceUID(extBContext(t), 2000, "Drafts",
		&ReplaceOptions{AllowNonAtomicFallback: true, Flags: []imap.Flag{imap.FlagDraft}},
		int64(len(replaceMessage)), strings.NewReader(replaceMessage))
	if err != nil {
		t.Fatal(err)
	}
	if !data.Emulated {
		t.Error("the emulated path did not mark its result as emulated")
	}
	lines := server.Lines()
	if len(lines) != 3 {
		t.Fatalf("expected three commands, got %q", lines)
	}
	if !strings.Contains(lines[0], "APPEND Drafts (\\Draft) {") {
		t.Errorf("first command = %q, want the APPEND first", lines[0])
	}
	if !strings.HasSuffix(lines[1], "UID STORE 2000 +FLAGS.SILENT (\\Deleted)") {
		t.Errorf("second command = %q", lines[1])
	}
	if !strings.HasSuffix(lines[2], "UID EXPUNGE 2000") {
		t.Errorf("third command = %q, want a UID-scoped expunge", lines[2])
	}
}

func TestReplaceEmulatedResolvesSequenceNumber(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string {
		switch {
		case strings.Contains(line, "FETCH"):
			return "* 4 FETCH (UID 2000)\r\n" + tag + " OK fetched\r\n"
		case strings.Contains(line, "APPEND"):
			return tag + " OK appended\r\n"
		case strings.Contains(line, "UID STORE"):
			return tag + " OK stored\r\n"
		case strings.Contains(line, "UID EXPUNGE"):
			return tag + " OK expunged\r\n"
		}
		return tag + " BAD unexpected\r\n"
	})
	extBReady(c, []string{"IMAP4REV1", "UIDPLUS"}, nil, true)
	if _, err := c.Replace(extBContext(t), 4, "Drafts",
		&ReplaceOptions{AllowNonAtomicFallback: true},
		int64(len(replaceMessage)), strings.NewReader(replaceMessage)); err != nil {
		t.Fatal(err)
	}
	lines := server.Lines()
	if len(lines) != 4 || !strings.HasSuffix(lines[0], "FETCH 4 (UID)") {
		t.Fatalf("commands = %q", lines)
	}
	if !strings.HasSuffix(lines[3], "UID EXPUNGE 2000") {
		t.Errorf("expunge command = %q, want the resolved UID", lines[3])
	}
}

// TestReplaceEmulatedRequiresUIDPlus records why the fallback refuses without
// UIDPLUS: the only other expunge available removes every \Deleted message in
// the mailbox, which is silent data loss.
func TestReplaceEmulatedRequiresUIDPlus(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string { return tag + " OK done\r\n" })
	extBReady(c, []string{"IMAP4REV1"}, nil, true)
	_, err := c.ReplaceUID(extBContext(t), 1, "Drafts",
		&ReplaceOptions{AllowNonAtomicFallback: true},
		int64(len(replaceMessage)), strings.NewReader(replaceMessage))
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v, want ErrCapabilityNotAdvertised", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("the fallback ran without UID EXPUNGE available: %q", server.Lines())
	}
}

func TestParseReplaceAppendUIDRejectsMalformed(t *testing.T) {
	for _, args := range []string{"", "1", "1 2 3", "0 5", "1 0", "x 5", "1 y"} {
		if _, _, err := parseReplaceAppendUID(args); err == nil {
			t.Errorf("parseReplaceAppendUID(%q) accepted a malformed code", args)
		}
	}
	validity, uid, err := parseReplaceAppendUID("1 2001")
	if err != nil || validity != 1 || uid != 2001 {
		t.Fatalf("parseReplaceAppendUID(\"1 2001\") = (%d, %d, %v)", validity, uid, err)
	}
}
