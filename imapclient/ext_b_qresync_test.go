package imapclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

const qresyncCaps = "IMAP4REV1 ENABLE CONDSTORE QRESYNC"

// selectResponse is the recorded RFC 7162 section 3.2.5.1 example, adapted to a
// small mailbox: a CLOSED boundary, the ordinary SELECT data, a
// VANISHED (EARLIER) line, and the flag changes that followed it.
const qresyncSelectResponse = "* OK [CLOSED] Previous mailbox closed\r\n" +
	"* 100 EXISTS\r\n" +
	"* 11 RECENT\r\n" +
	"* OK [UIDVALIDITY 67890007] UIDVALIDITY\r\n" +
	"* OK [UIDNEXT 600] Predicted next UID\r\n" +
	"* OK [HIGHESTMODSEQ 90060115205545359] Highest mailbox mod-sequence\r\n" +
	"* OK [MAILBOXID (F2212ea87-6097-4256-9d51-71338625)] Ok\r\n" +
	"* OK [UNSEEN 7] There are some unseen messages\r\n" +
	"* FLAGS (\\Answered \\Flagged \\Draft \\Deleted \\Seen)\r\n" +
	"* OK [PERMANENTFLAGS (\\Answered \\Flagged \\Draft \\Deleted \\Seen \\*)] Permanent flags\r\n" +
	"* VANISHED (EARLIER) 41,43:116,118,120:211,214:540\r\n" +
	"* 49 FETCH (UID 117 FLAGS (\\Seen \\Answered) MODSEQ (90060115194045001))\r\n" +
	"* 50 FETCH (UID 119 FLAGS (\\Draft $MDNSent) MODSEQ (90060115194045308))\r\n"

func TestSelectSyncQResyncCollectsDelta(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string {
		return qresyncSelectResponse + tag + " OK [READ-WRITE] mailbox selected\r\n"
	})
	extBReady(c, strings.Fields(qresyncCaps), []string{"QRESYNC", "CONDSTORE"}, false)
	ctx := extBContext(t)

	data, err := c.SelectSync("INBOX", &SyncSelectOptions{
		CondStore: true,
		QResync: &QResyncOptions{
			UIDValidity: 67890007,
			ModSeq:      90060115194045000,
			KnownUIDs:   imap.UIDSetRange(41, 211),
		},
	}).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}

	want := "SELECT INBOX (CONDSTORE QRESYNC (67890007 90060115194045000 41:211))"
	if got := server.LastLine(); !strings.HasSuffix(got, want) {
		t.Fatalf("command line = %q, want suffix %q", got, want)
	}
	if !data.Closed {
		t.Error("CLOSED response code was not reported")
	}
	if data.NoModSeq {
		t.Error("NOMODSEQ reported when the server sent HIGHESTMODSEQ")
	}
	if data.ResyncRejected {
		t.Error("ResyncRejected set although UIDVALIDITY matched")
	}
	if data.MailboxID != "F2212ea87-6097-4256-9d51-71338625" {
		t.Errorf("MailboxID = %q", data.MailboxID)
	}
	if data.Status.UIDValidity != 67890007 || data.Status.HighestModSeq != 90060115205545359 {
		t.Errorf("anchor = (%d, %d)", data.Status.UIDValidity, data.Status.HighestModSeq)
	}
	if data.Status.NumMessages != 100 || data.Status.UIDNext != 600 {
		t.Errorf("status = %#v", data.Status)
	}
	if len(data.Vanished) != 1 || !data.Vanished[0].Earlier {
		t.Fatalf("Vanished = %#v", data.Vanished)
	}
	wantVanished := "41,43:116,118,120:211,214:540"
	if got := data.Vanished[0].UIDs.String(); got != wantVanished {
		t.Errorf("vanished UIDs = %q, want %q", got, wantVanished)
	}
	if !data.Vanished[0].UIDs.Contains(43) || data.Vanished[0].UIDs.Contains(42) {
		t.Errorf("vanished set does not describe the expunged UIDs: %q", data.Vanished[0].UIDs.String())
	}
	if len(data.Fetched) != 2 {
		t.Fatalf("Fetched = %#v", data.Fetched)
	}
	if uid := fetchedUID(t, data.Fetched[0]); uid != 117 {
		t.Errorf("first flag update UID = %d", uid)
	}
	if modSeq := fetchedModSeq(t, data.Fetched[1]); modSeq != 90060115194045308 {
		t.Errorf("second flag update MODSEQ = %d", modSeq)
	}
	if c.State() != StateSelected {
		t.Errorf("state after SELECT = %q", c.State())
	}
}

func fetchedUID(t *testing.T, data *imap.FetchMessageData) imap.UID {
	t.Helper()
	for _, value := range data.Items["UID"] {
		if v, ok := value.(imap.FetchDataUID); ok {
			return imap.UID(v)
		}
	}
	t.Fatalf("no UID in %#v", data.Items)
	return 0
}

func fetchedModSeq(t *testing.T, data *imap.FetchMessageData) uint64 {
	t.Helper()
	for _, value := range data.Items["MODSEQ"] {
		if v, ok := value.(imap.FetchDataModSeq); ok {
			return uint64(v)
		}
	}
	t.Fatalf("no MODSEQ in %#v", data.Items)
	return 0
}

// TestSelectSyncVanishedPrecedesFetch pins the ordering requirement of RFC 7162
// section 3.2.6: every VANISHED (EARLIER) response arrives before any FETCH
// response, so a client applying them in order never maps a stale sequence
// number onto a live message.
func TestSelectSyncVanishedPrecedesFetch(t *testing.T) {
	c, _ := extBDial(t, func(tag, line string) string {
		return "* OK [UIDVALIDITY 1] v\r\n* OK [HIGHESTMODSEQ 20] m\r\n" +
			"* VANISHED (EARLIER) 3:4\r\n" +
			"* 1 FETCH (UID 5 MODSEQ (19))\r\n" +
			"* VANISHED (EARLIER) 9\r\n" +
			tag + " OK selected\r\n"
	})
	extBReady(c, strings.Fields(qresyncCaps), []string{"QRESYNC"}, false)
	data, err := c.SelectSync("INBOX", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 1, ModSeq: 10}}).Wait(extBContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Vanished) != 2 || len(data.Fetched) != 1 {
		t.Fatalf("Vanished = %#v Fetched = %#v", data.Vanished, data.Fetched)
	}
	if data.Vanished[0].UIDs.String() != "3:4" || data.Vanished[1].UIDs.String() != "9" {
		t.Errorf("VANISHED responses were reordered: %#v", data.Vanished)
	}
}

func TestSelectSyncReportsUIDValidityMismatch(t *testing.T) {
	c, _ := extBDial(t, func(tag, line string) string {
		// RFC 7162 section 3.2.5: on a UIDVALIDITY mismatch the server ignores
		// the remaining parameters and reports nothing at all about it.
		return "* 464 EXISTS\r\n* OK [UIDVALIDITY 3857529045] UIDVALIDITY\r\n" +
			"* OK [HIGHESTMODSEQ 90060128194045007] m\r\n" +
			tag + " OK [READ-WRITE] Sorry, UIDVALIDITY mismatch\r\n"
	})
	extBReady(c, strings.Fields(qresyncCaps), []string{"QRESYNC"}, false)
	data, err := c.SelectSync("INBOX", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 67890007, ModSeq: 20050715194045000}}).Wait(extBContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if !data.ResyncRejected {
		t.Fatal("a UIDVALIDITY mismatch was not reported: a caller would treat an empty delta as 'nothing changed'")
	}
	if len(data.Vanished) != 0 || len(data.Fetched) != 0 {
		t.Errorf("delta reported for a rejected anchor: %#v %#v", data.Vanished, data.Fetched)
	}
}

func TestSelectSyncReportsNoModSeq(t *testing.T) {
	c, _ := extBDial(t, func(tag, line string) string {
		return "* OK [UIDVALIDITY 67890007] v\r\n" +
			"* OK [NOMODSEQ] Sorry, this mailbox format doesn't support modsequences\r\n" +
			tag + " OK [READ-WRITE] selected\r\n"
	})
	extBReady(c, strings.Fields(qresyncCaps), []string{"QRESYNC"}, false)
	data, err := c.SelectSync("INBOX", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 67890007, ModSeq: 5}}).Wait(extBContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if !data.NoModSeq {
		t.Fatal("NOMODSEQ was not reported")
	}
	if !data.ResyncRejected {
		t.Fatal("NOMODSEQ must reject the anchor: RFC 7162 section 3.2.5.1 has the server ignore the QRESYNC parameters")
	}
	if data.Status.HighestModSeq != 0 {
		t.Errorf("HighestModSeq = %d", data.Status.HighestModSeq)
	}
}

func TestSelectSyncEncodesSequenceMatchData(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string {
		return "* OK [UIDVALIDITY 67890007] v\r\n" + tag + " OK selected\r\n"
	})
	extBReady(c, strings.Fields(qresyncCaps), []string{"QRESYNC"}, false)
	_, err := c.SelectSync("INBOX", &SyncSelectOptions{QResync: &QResyncOptions{
		UIDValidity: 67890007,
		ModSeq:      90060115194045000,
		KnownUIDs:   imap.UIDSetRange(1, 29997),
		SeqMatch: &SeqMatchData{
			SeqNums: imap.SeqSetNum(5000, 7500),
			UIDs:    imap.UIDSetNum(15000, 22500),
		},
	}}).Wait(extBContext(t))
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT INBOX (QRESYNC (67890007 90060115194045000 1:29997 (5000,7500 15000,22500)))"
	if got := server.LastLine(); !strings.HasSuffix(got, want) {
		t.Fatalf("command line = %q, want suffix %q", got, want)
	}
}

func TestSelectSyncFailureLeavesNoMailboxSelected(t *testing.T) {
	c, _ := extBDial(t, func(tag, line string) string {
		return tag + " NO no such mailbox\r\n"
	})
	extBReady(c, strings.Fields(qresyncCaps), []string{"QRESYNC"}, true)
	if _, err := c.SelectSync("Missing", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 1, ModSeq: 1}}).Wait(extBContext(t)); err == nil {
		t.Fatal("expected an error")
	}
	if c.State() != StateAuthenticated {
		t.Fatalf("state after a failed SELECT = %q, want authenticated", c.State())
	}
}

func TestSelectSyncRequiresEnableQResync(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string { return tag + " OK selected\r\n" })
	extBReady(c, strings.Fields(qresyncCaps), nil, false)
	_, err := c.SelectSync("INBOX", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 1, ModSeq: 1}}).Wait(extBContext(t))
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v, want ErrCapabilityNotAdvertised", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("a command was sent despite the missing ENABLE: %q", server.Lines())
	}
}

func TestSelectSyncRejectsIllegalArguments(t *testing.T) {
	tests := []struct {
		name    string
		options *SyncSelectOptions
	}{
		{"zero UIDVALIDITY", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 0, ModSeq: 1}}},
		{"zero mod-sequence", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 1, ModSeq: 0}}},
		{"mod-sequence above 63 bits", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 1, ModSeq: MaxModSeq + 1}}},
		{"dynamic known UIDs", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 1, ModSeq: 1, KnownUIDs: imap.UIDSetRange(1, 0)}}},
		{"unbalanced sequence match data", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 1, ModSeq: 1,
			SeqMatch: &SeqMatchData{SeqNums: imap.SeqSetNum(1, 2), UIDs: imap.UIDSetNum(9)}}}},
		{"dynamic sequence match data", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 1, ModSeq: 1,
			SeqMatch: &SeqMatchData{SeqNums: imap.SeqSetRange(1, 0), UIDs: imap.UIDSetNum(9)}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, server := extBDial(t, func(tag, line string) string { return tag + " OK selected\r\n" })
			extBReady(c, strings.Fields(qresyncCaps), []string{"QRESYNC"}, false)
			if _, err := c.SelectSync("INBOX", test.options).Wait(extBContext(t)); err == nil {
				t.Fatal("expected the command to be refused locally")
			}
			if len(server.Lines()) != 0 {
				t.Fatalf("an illegal QRESYNC parameter reached the wire: %q", server.Lines())
			}
		})
	}
}

func TestSelectSyncCondStoreEnablesImplicitly(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string {
		return "* OK [HIGHESTMODSEQ 715194045007] m\r\n" + tag + " OK [READ-WRITE] CONDSTORE is now enabled\r\n"
	})
	extBReady(c, []string{"IMAP4REV1", "CONDSTORE"}, nil, false)
	if c.CondStoreEnabled() {
		t.Fatal("CONDSTORE reported as enabled before any enabling command")
	}
	if _, err := c.SelectSync("INBOX", &SyncSelectOptions{CondStore: true}).Wait(extBContext(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(server.LastLine(), "SELECT INBOX (CONDSTORE)") {
		t.Fatalf("command line = %q", server.LastLine())
	}
	if !c.CondStoreEnabled() {
		t.Fatal("SELECT (CONDSTORE) is a CONDSTORE enabling command (RFC 7162 section 3.1) but the session did not record it")
	}
}

func TestReadVanishedRejectsWildcard(t *testing.T) {
	c, _ := extBDial(t, func(tag, line string) string {
		return "* VANISHED (EARLIER) 1:*\r\n" + tag + " OK selected\r\n"
	})
	extBReady(c, strings.Fields(qresyncCaps), []string{"QRESYNC"}, false)
	if _, err := c.SelectSync("INBOX", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 1, ModSeq: 1}}).Wait(extBContext(t)); err == nil {
		t.Fatal("a VANISHED set containing \"*\" was accepted; the caller cannot tell which UIDs are gone")
	}
}

func TestVanishedEarlierIsDistinctFromExpunge(t *testing.T) {
	c, _ := extBDial(t, func(tag, line string) string {
		return "* OK [UIDVALIDITY 1] v\r\n* VANISHED 7,9\r\n" + tag + " OK selected\r\n"
	})
	extBReady(c, strings.Fields(qresyncCaps), []string{"QRESYNC"}, false)
	data, err := c.SelectSync("INBOX", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 1, ModSeq: 1}}).Wait(extBContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Vanished) != 1 || data.Vanished[0].Earlier {
		t.Fatalf("a VANISHED without (EARLIER) was reported as earlier: %#v", data.Vanished)
	}
}
