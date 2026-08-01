package imapclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestMailboxSize(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string {
		return "* STATUS frop (SIZE 44421)\r\n" + tag + " OK STATUS completed\r\n"
	})
	extBReady(c, []string{"IMAP4REV1", "STATUS=SIZE"}, nil, false)
	size, err := c.MailboxSize(extBContext(t), "frop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if size != 44421 {
		t.Fatalf("size = %d", size)
	}
	if got := server.LastLine(); !strings.HasSuffix(got, "STATUS frop (SIZE)") {
		t.Fatalf("command line = %q", got)
	}
}

// TestMailboxSizeIs63Bit covers the RFC 8438 section 3 requirement that clients
// accept 63-bit SIZE values: a mailbox above 4 GiB is ordinary.
func TestMailboxSizeIs63Bit(t *testing.T) {
	c, _ := extBDial(t, func(tag, line string) string {
		return "* STATUS big (SIZE 9223372036854775807)\r\n" + tag + " OK done\r\n"
	})
	extBReady(c, []string{"IMAP4REV1", "STATUS=SIZE"}, nil, false)
	size, err := c.MailboxSize(extBContext(t), "big", nil)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(MaxModSeq) {
		t.Fatalf("size = %d, want %d", size, int64(MaxModSeq))
	}
}

func TestMailboxSizeRequiresCapability(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string { return tag + " OK done\r\n" })
	extBReady(c, []string{"IMAP4REV1"}, nil, false)
	if _, err := c.MailboxSize(extBContext(t), "frop", nil); !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v, want ErrCapabilityNotAdvertised", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("a command was sent without STATUS=SIZE: %q", server.Lines())
	}
}

func TestAppendLimitPerMailbox(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string {
		return "* STATUS INBOX (APPENDLIMIT 257890)\r\n" + tag + " OK STATUS completed\r\n"
	})
	extBReady(c, []string{"IMAP4REV1", "APPENDLIMIT"}, nil, false)
	data, err := c.AppendLimit(extBContext(t), "INBOX", nil)
	if err != nil {
		t.Fatal(err)
	}
	if data.Limit != 257890 || data.Unlimited || data.ServerWide {
		t.Fatalf("data = %#v", data)
	}
	if got := server.LastLine(); !strings.HasSuffix(got, "STATUS INBOX (APPENDLIMIT)") {
		t.Fatalf("command line = %q", got)
	}
}

// TestAppendLimitNil covers RFC 7889 section 5's "number / nil": NIL means the
// mailbox has no limit, which must not be reported as a limit of zero — zero
// means the server accepts no APPEND at all.
func TestAppendLimitNil(t *testing.T) {
	c, _ := extBDial(t, func(tag, line string) string {
		return "* STATUS INBOX (APPENDLIMIT NIL)\r\n" + tag + " OK done\r\n"
	})
	extBReady(c, []string{"IMAP4REV1", "APPENDLIMIT"}, nil, false)
	data, err := c.AppendLimit(extBContext(t), "INBOX", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !data.Unlimited {
		t.Fatalf("data = %#v, want Unlimited", data)
	}
}

// TestAppendLimitServerWide covers the "APPENDLIMIT=<n>" capability form, which
// Cyrus advertises. It answers without a round trip.
func TestAppendLimitServerWide(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string { return tag + " OK done\r\n" })
	extBReady(c, []string{"IMAP4REV1", "APPENDLIMIT=4294967295"}, nil, false)
	data, err := c.AppendLimit(extBContext(t), "INBOX", nil)
	if err != nil {
		t.Fatal(err)
	}
	if data.Limit != 4294967295 || !data.ServerWide {
		t.Fatalf("data = %#v", data)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("a STATUS was sent although the limit is server-wide: %q", server.Lines())
	}
}

func TestAppendLimitRequiresCapability(t *testing.T) {
	c, _ := extBDial(t, func(tag, line string) string { return tag + " OK done\r\n" })
	extBReady(c, []string{"IMAP4REV1"}, nil, false)
	if _, err := c.AppendLimit(extBContext(t), "INBOX", nil); !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v, want ErrCapabilityNotAdvertised", err)
	}
}

func TestMailboxHighestModSeq(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string {
		return "* STATUS blurdybloop (HIGHESTMODSEQ 7011231777)\r\n" + tag + " OK STATUS completed\r\n"
	})
	extBReady(c, []string{"IMAP4REV1", "CONDSTORE"}, nil, false)
	modSeq, err := c.MailboxHighestModSeq(extBContext(t), "blurdybloop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if modSeq != 7011231777 {
		t.Fatalf("modseq = %d", modSeq)
	}
	if got := server.LastLine(); !strings.HasSuffix(got, "STATUS blurdybloop (HIGHESTMODSEQ)") {
		t.Fatalf("command line = %q", got)
	}
	if !c.CondStoreEnabled() {
		t.Error("STATUS (HIGHESTMODSEQ) is a CONDSTORE enabling command (RFC 7162 section 3.1)")
	}
}

// TestMailboxHighestModSeqZero pins RFC 7162 section 3.1.7: a mailbox without
// persistent mod-sequences answers zero rather than failing, and zero is a
// value, not an absence.
func TestMailboxHighestModSeqZero(t *testing.T) {
	c, _ := extBDial(t, func(tag, line string) string {
		return "* STATUS legacy (HIGHESTMODSEQ 0)\r\n" + tag + " OK done\r\n"
	})
	extBReady(c, []string{"IMAP4REV1", "CONDSTORE"}, nil, false)
	modSeq, err := c.MailboxHighestModSeq(extBContext(t), "legacy", nil)
	if err != nil {
		t.Fatal(err)
	}
	if modSeq != 0 {
		t.Fatalf("modseq = %d", modSeq)
	}
}

// TestStatusAccessorsPreserveAbsence distinguishes "the server did not send the
// item" from "the item was zero".
func TestStatusAccessorsPreserveAbsence(t *testing.T) {
	empty := &StatusData{Values: map[imap.StatusItemKeyword]any{}}
	if _, ok := StatusSize(empty); ok {
		t.Error("StatusSize reported a value for an absent item")
	}
	if _, _, ok := StatusAppendLimit(empty); ok {
		t.Error("StatusAppendLimit reported a value for an absent item")
	}
	zero := &StatusData{Values: map[imap.StatusItemKeyword]any{imap.StatusItemSize: uint64(0)}}
	size, ok := StatusSize(zero)
	if !ok || size != 0 {
		t.Errorf("StatusSize(zero) = (%d, %v)", size, ok)
	}
}

// TestMailboxHighestModSeqRequiresCondStore also proves QRESYNC alone is
// enough: RFC 7162 section 3.2.3 makes QRESYNC imply CONDSTORE.
func TestMailboxHighestModSeqRequiresCondStore(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string { return tag + " OK done\r\n" })
	extBReady(c, []string{"IMAP4REV1"}, nil, false)
	if _, err := c.MailboxHighestModSeq(extBContext(t), "INBOX", nil); !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v, want ErrCapabilityNotAdvertised", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("a command was sent without CONDSTORE: %q", server.Lines())
	}

	c2, _ := extBDial(t, func(tag, line string) string {
		return "* STATUS INBOX (HIGHESTMODSEQ 5)\r\n" + tag + " OK done\r\n"
	})
	extBReady(c2, []string{"IMAP4REV1", "QRESYNC"}, nil, false)
	if _, err := c2.MailboxHighestModSeq(extBContext(t), "INBOX", nil); err != nil {
		t.Fatalf("QRESYNC implies CONDSTORE but the command was refused: %v", err)
	}
}
