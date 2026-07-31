package imapclient

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func collectFetch(t *testing.T, cmd *SyncFetchCommand) []*imap.FetchMessageData {
	t.Helper()
	ctx := extBContext(t)
	var all []*imap.FetchMessageData
	for {
		data, err := cmd.Next(ctx)
		if errors.Is(err, io.EOF) {
			return all
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		all = append(all, data)
	}
}

// TestFetchUIDSyncResync is the single-command incremental resynchronisation of
// RFC 7162 section 3.2.6: one UID FETCH reports both the flag changes and the
// expunges since a known mod-sequence.
func TestFetchUIDSyncResync(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string {
		return "* VANISHED (EARLIER) 300:310,405,411\r\n" +
			"* 1 FETCH (UID 404 MODSEQ (65402) FLAGS (\\Seen))\r\n" +
			"* 2 FETCH (UID 406 MODSEQ (75403) FLAGS (\\Deleted))\r\n" +
			tag + " OK FETCH completed\r\n"
	})
	extBReady(c, strings.Fields(qresyncCaps), []string{"QRESYNC", "CONDSTORE"}, true)

	cmd := c.FetchUIDSync(imap.UIDSetRange(300, 500), &SyncFetchOptions{ChangedSince: 12345, ReportVanished: true}, imap.FetchItemFlags)
	messages := collectFetch(t, cmd)
	if err := cmd.Wait(extBContext(t)); err != nil {
		t.Fatal(err)
	}

	want := "UID FETCH 300:500 (FLAGS) (CHANGEDSINCE 12345 VANISHED)"
	if got := server.LastLine(); !strings.HasSuffix(got, want) {
		t.Fatalf("command line = %q, want suffix %q", got, want)
	}
	vanished := cmd.Vanished()
	if len(vanished) != 1 || !vanished[0].Earlier || vanished[0].UIDs.String() != "300:310,405,411" {
		t.Fatalf("Vanished = %#v", vanished)
	}
	if len(messages) != 2 {
		t.Fatalf("got %d FETCH responses", len(messages))
	}
	if uid := fetchedUID(t, messages[0]); uid != 404 {
		t.Errorf("first UID = %d", uid)
	}
	if modSeq := fetchedModSeq(t, messages[1]); modSeq != 75403 {
		t.Errorf("second MODSEQ = %d", modSeq)
	}
	if !c.CondStoreEnabled() {
		t.Error("a FETCH with CHANGEDSINCE is a CONDSTORE enabling command (RFC 7162 section 3.1)")
	}
}

// TestFetchUIDSyncWholeMailboxSet covers the "1:*" set, which is what a client
// resynchronising an entire mailbox sends. "*" is a list-wildcard rather than
// an ATOM-CHAR, so it needs encoding that does not run through the plain atom
// production.
func TestFetchUIDSyncWholeMailboxSet(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string { return tag + " OK done\r\n" })
	extBReady(c, strings.Fields(qresyncCaps), []string{"QRESYNC", "CONDSTORE"}, true)
	cmd := c.FetchUIDSync(imap.UIDSetRange(1, 0), &SyncFetchOptions{ChangedSince: 7, ReportVanished: true}, imap.FetchItemUID, imap.FetchItemFlags)
	collectFetch(t, cmd)
	if err := cmd.Wait(extBContext(t)); err != nil {
		t.Fatal(err)
	}
	want := "UID FETCH 1:* (UID FLAGS) (CHANGEDSINCE 7 VANISHED)"
	if got := server.LastLine(); !strings.HasSuffix(got, want) {
		t.Fatalf("command line = %q, want suffix %q", got, want)
	}
}

// TestFetchSyncModSeqIsNotTruncated checks the whole 63-bit range survives the
// wire, the parser and the public type. A mod-sequence silently wrapped to a
// smaller value would make a client re-fetch nothing or everything.
func TestFetchSyncModSeqIsNotTruncated(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string {
		return "* 1 FETCH (UID 4 MODSEQ (9223372036854775807))\r\n" + tag + " OK done\r\n"
	})
	extBReady(c, strings.Fields(qresyncCaps), []string{"CONDSTORE"}, true)
	cmd := c.FetchSync(imap.SeqSetNum(1), &SyncFetchOptions{ChangedSince: MaxModSeq}, imap.FetchItemModSeq)
	messages := collectFetch(t, cmd)
	if err := cmd.Wait(extBContext(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(server.LastLine(), "FETCH 1 (MODSEQ) (CHANGEDSINCE 9223372036854775807)") {
		t.Fatalf("command line = %q", server.LastLine())
	}
	if len(messages) != 1 {
		t.Fatalf("got %d FETCH responses", len(messages))
	}
	if modSeq := fetchedModSeq(t, messages[0]); modSeq != MaxModSeq {
		t.Fatalf("MODSEQ = %d, want %d", modSeq, MaxModSeq)
	}
}

func TestFetchSyncRejectsIllegalModifiers(t *testing.T) {
	tests := []struct {
		name    string
		caps    string
		enabled []string
		run     func(*Client) *SyncFetchCommand
	}{
		{
			name:    "VANISHED on a sequence-number FETCH",
			caps:    qresyncCaps,
			enabled: []string{"QRESYNC"},
			run: func(c *Client) *SyncFetchCommand {
				return c.FetchSync(imap.SeqSetNum(1), &SyncFetchOptions{ChangedSince: 5, ReportVanished: true}, imap.FetchItemFlags)
			},
		},
		{
			name:    "VANISHED without CHANGEDSINCE",
			caps:    qresyncCaps,
			enabled: []string{"QRESYNC"},
			run: func(c *Client) *SyncFetchCommand {
				return c.FetchUIDSync(imap.UIDSetNum(1), &SyncFetchOptions{ReportVanished: true}, imap.FetchItemFlags)
			},
		},
		{
			name: "VANISHED without ENABLE QRESYNC",
			caps: qresyncCaps,
			run: func(c *Client) *SyncFetchCommand {
				return c.FetchUIDSync(imap.UIDSetNum(1), &SyncFetchOptions{ChangedSince: 5, ReportVanished: true}, imap.FetchItemFlags)
			},
		},
		{
			name: "CHANGEDSINCE without CONDSTORE",
			caps: "IMAP4REV1",
			run: func(c *Client) *SyncFetchCommand {
				return c.FetchUIDSync(imap.UIDSetNum(1), &SyncFetchOptions{ChangedSince: 5}, imap.FetchItemFlags)
			},
		},
		{
			name:    "CHANGEDSINCE above 63 bits",
			caps:    qresyncCaps,
			enabled: []string{"CONDSTORE"},
			run: func(c *Client) *SyncFetchCommand {
				return c.FetchUIDSync(imap.UIDSetNum(1), &SyncFetchOptions{ChangedSince: MaxModSeq + 1}, imap.FetchItemFlags)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, server := extBDial(t, func(tag, line string) string { return tag + " OK done\r\n" })
			extBReady(c, strings.Fields(test.caps), test.enabled, true)
			cmd := test.run(c)
			if _, err := cmd.Next(extBContext(t)); err == nil {
				t.Fatal("expected the command to be refused locally")
			}
			if len(server.Lines()) != 0 {
				t.Fatalf("an illegal FETCH modifier reached the wire: %q", server.Lines())
			}
		})
	}
}

func TestStoreUIDSyncUnchangedSince(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string {
		return "* 1 FETCH (UID 4 MODSEQ (12121231000))\r\n" + tag + " OK Conditional Store completed\r\n"
	})
	extBReady(c, strings.Fields(qresyncCaps), []string{"CONDSTORE"}, true)
	modSeq := uint64(12121230045)
	data, err := c.StoreUIDSync(imap.UIDSetNum(6, 4, 8), []imap.Flag{imap.FlagDeleted},
		&SyncStoreOptions{Op: StoreFlagsAdd, Silent: true, UnchangedSince: &modSeq}).Wait(extBContext(t))
	if err != nil {
		t.Fatal(err)
	}
	want := "UID STORE 4,6,8 (UNCHANGEDSINCE 12121230045) +FLAGS.SILENT (\\Deleted)"
	if got := server.LastLine(); !strings.HasSuffix(got, want) {
		t.Fatalf("command line = %q, want suffix %q", got, want)
	}
	if data.HasModified() {
		t.Errorf("failures reported for a store that succeeded: %#v", data)
	}
	if !c.CondStoreEnabled() {
		t.Error("a STORE with UNCHANGEDSINCE is a CONDSTORE enabling command")
	}
}

// TestStoreUIDSyncZeroUnchangedSince covers RFC 7162 Example 8: UNCHANGEDSINCE 0
// is a legal probe that always fails if the metadata item exists, so zero must
// be distinguishable from "no modifier".
func TestStoreUIDSyncZeroUnchangedSince(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string { return tag + " OK done\r\n" })
	extBReady(c, strings.Fields(qresyncCaps), []string{"CONDSTORE"}, true)
	zero := uint64(0)
	if _, err := c.StoreUIDSync(imap.UIDSetNum(12), []imap.Flag{"$MDNSent"},
		&SyncStoreOptions{Op: StoreFlagsAdd, Silent: true, UnchangedSince: &zero}).Wait(extBContext(t)); err != nil {
		t.Fatal(err)
	}
	want := "UID STORE 12 (UNCHANGEDSINCE 0) +FLAGS.SILENT ($MDNSent)"
	if got := server.LastLine(); !strings.HasSuffix(got, want) {
		t.Fatalf("command line = %q, want suffix %q", got, want)
	}
}

func TestStoreSyncNilOptionsOmitsModifier(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string { return tag + " OK done\r\n" })
	extBReady(c, strings.Fields(qresyncCaps), nil, true)
	if _, err := c.StoreSync(imap.SeqSetNum(1), []imap.Flag{imap.FlagSeen}, nil).Wait(extBContext(t)); err != nil {
		t.Fatal(err)
	}
	if got := server.LastLine(); !strings.HasSuffix(got, "STORE 1 FLAGS (\\Seen)") {
		t.Fatalf("command line = %q", got)
	}
}

// TestStoreSyncSurfacesModifiedOnNo checks that the MODIFIED failure list is
// delivered as a set of message identifiers rather than being collapsed into a
// generic error. RFC 7162 section 3.1 permits MODIFIED on a NO as well as on an
// OK; this is the NO half.
func TestStoreSyncSurfacesModifiedOnNo(t *testing.T) {
	t.Run("sequence numbers", func(t *testing.T) {
		c, _ := extBDial(t, func(tag, line string) string {
			return "* 5 FETCH (MODSEQ (320162350))\r\n" + tag + " NO [MODIFIED 7,9] Conditional STORE failed\r\n"
		})
		extBReady(c, strings.Fields(qresyncCaps), []string{"CONDSTORE"}, true)
		modSeq := uint64(320162338)
		data, err := c.StoreSync(imap.SeqSetNum(7, 5, 9), []imap.Flag{imap.FlagDeleted},
			&SyncStoreOptions{Op: StoreFlagsAdd, Silent: true, UnchangedSince: &modSeq}).Wait(extBContext(t))
		if err == nil {
			t.Fatal("expected the tagged NO to surface as an error")
		}
		var ierr *imap.Error
		if !errors.As(err, &ierr) || ierr.Code != imap.CodeModified {
			t.Fatalf("err = %v, want an imap.Error with the MODIFIED code", err)
		}
		if data == nil || data.ModifiedSeqNums.String() != "7,9" {
			t.Fatalf("ModifiedSeqNums = %#v", data)
		}
		if !data.ModifiedUIDs.IsEmpty() {
			t.Errorf("a sequence-number STORE reported UIDs: %#v", data.ModifiedUIDs)
		}
	})
	t.Run("UIDs", func(t *testing.T) {
		c, _ := extBDial(t, func(tag, line string) string {
			return tag + " NO [MODIFIED 101,110:111] Conditional STORE failed\r\n"
		})
		extBReady(c, strings.Fields(qresyncCaps), []string{"CONDSTORE"}, true)
		modSeq := uint64(12121230045)
		data, err := c.StoreUIDSync(imap.UIDSetRange(100, 150), []imap.Flag{imap.FlagDeleted},
			&SyncStoreOptions{Op: StoreFlagsAdd, Silent: true, UnchangedSince: &modSeq}).Wait(extBContext(t))
		if err == nil {
			t.Fatal("expected the tagged NO to surface as an error")
		}
		if data == nil || data.ModifiedUIDs.String() != "101,110:111" {
			t.Fatalf("ModifiedUIDs = %#v", data)
		}
		if !data.ModifiedSeqNums.IsEmpty() {
			t.Errorf("a UID STORE reported sequence numbers: %#v", data.ModifiedSeqNums)
		}
		if !data.HasModified() {
			t.Error("HasModified reported no failures")
		}
	})
}

func TestStoreSyncRejectsUnchangedSinceWithoutCondStore(t *testing.T) {
	c, server := extBDial(t, func(tag, line string) string { return tag + " OK done\r\n" })
	extBReady(c, []string{"IMAP4REV1"}, nil, true)
	modSeq := uint64(1)
	_, err := c.StoreUIDSync(imap.UIDSetNum(1), []imap.Flag{imap.FlagSeen}, &SyncStoreOptions{UnchangedSince: &modSeq}).Wait(extBContext(t))
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v, want ErrCapabilityNotAdvertised", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("a command was sent without the capability: %q", server.Lines())
	}
}

// TestReadSyncFetchPreservesUnknownItems checks that a FETCH item this library
// does not model still reaches the caller instead of being dropped, and that an
// unexpected literal in a buffered response does not wedge the reader.
func TestReadSyncFetchPreservesUnknownItems(t *testing.T) {
	c, _ := extBDial(t, func(tag, line string) string {
		return "* OK [UIDVALIDITY 1] v\r\n" +
			"* 1 FETCH (UID 4 MODSEQ (7) X-VENDOR-THING {5}\r\nhello X-OTHER (a b))\r\n" +
			tag + " OK selected\r\n"
	})
	extBReady(c, strings.Fields(qresyncCaps), []string{"QRESYNC"}, false)
	data, err := c.SelectSync("INBOX", &SyncSelectOptions{QResync: &QResyncOptions{UIDValidity: 1, ModSeq: 1}}).Wait(extBContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Fetched) != 1 {
		t.Fatalf("Fetched = %#v", data.Fetched)
	}
	items := data.Fetched[0].Items
	raw, ok := items["X-VENDOR-THING"]
	if !ok || len(raw) != 1 {
		t.Fatalf("unknown item dropped: %#v", items)
	}
	value, ok := raw[0].(*imap.FetchDataRaw)
	if !ok {
		t.Fatalf("unknown item type %T", raw[0])
	}
	body, err := io.ReadAll(value.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "hello") {
		t.Errorf("raw value = %q, want the literal payload preserved", body)
	}
	if _, ok := items["X-OTHER"]; !ok {
		t.Errorf("second unknown item dropped: %#v", items)
	}
}
