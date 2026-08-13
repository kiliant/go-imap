package imapserver_test

import (
	"bufio"
	"context"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// CONDSTORE and QRESYNC are asserted at the wire. The response shapes here —
// MODSEQ inside a FETCH item list, MODIFIED on a tagged OK, VANISHED (EARLIER)
// — are exactly what a client-level assertion would smooth over.

var modSeqPattern = regexp.MustCompile(`MODSEQ \((\d+)\)`)

func TestLoopbackCondStoreEnableAndModSeq(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSessionIn(t, ctx, true)

	writeRawCommand(t, clientSide, "B0 SELECT INBOX\r\n")
	collectUntilTag(t, reader, "B0 ")

	// Before CONDSTORE is enabled, FETCH carries no MODSEQ.
	writeRawCommand(t, clientSide, "B1 FETCH 1 (FLAGS)\r\n")
	untagged, tagged := collectUntilTag(t, reader, "B1 ")
	if !strings.HasPrefix(tagged, "B1 OK") {
		t.Fatalf("FETCH failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* 1 FETCH"); strings.Contains(line, "MODSEQ") {
		t.Errorf("MODSEQ reported before CONDSTORE was enabled: %q", line)
	}

	// RFC 7162 section 3.1.8's other route to enabling CONDSTORE: using a
	// CONDSTORE parameter enables it for the rest of the session, no ENABLE
	// needed. SELECT (CONDSTORE) is that parameter.
	writeRawCommand(t, clientSide, "B2 SELECT INBOX (CONDSTORE)\r\n")
	if _, tagged := collectUntilTag(t, reader, "B2 "); !strings.HasPrefix(tagged, "B2 OK") {
		t.Fatalf("SELECT (CONDSTORE) failed: %q", tagged)
	}

	// RFC 7162 section 3.1.4.1: once enabled, every FETCH response carries
	// MODSEQ whether or not the client asked for it.
	writeRawCommand(t, clientSide, "B3 FETCH 1 (FLAGS)\r\n")
	untagged, _ = collectUntilTag(t, reader, "B3 ")
	if line := findResponse(t, untagged, "* 1 FETCH"); !strings.Contains(line, "MODSEQ") {
		t.Errorf("FETCH after SELECT (CONDSTORE) has no MODSEQ: %q", line)
	}
}

// ENABLE is the other route, and is valid only before a mailbox is selected.
func TestLoopbackCondStoreEnableCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSessionIn(t, ctx, true)

	writeRawCommand(t, clientSide, "B1 ENABLE CONDSTORE\r\n")
	untagged, tagged := collectUntilTag(t, reader, "B1 ")
	if !strings.HasPrefix(tagged, "B1 OK") {
		t.Fatalf("ENABLE CONDSTORE failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* ENABLED"); !strings.Contains(line, "CONDSTORE") {
		t.Errorf("ENABLED = %q, want CONDSTORE", line)
	}
	writeRawCommand(t, clientSide, "B2 SELECT INBOX\r\n")
	collectUntilTag(t, reader, "B2 ")
	writeRawCommand(t, clientSide, "B3 FETCH 1 (FLAGS)\r\n")
	untagged, _ = collectUntilTag(t, reader, "B3 ")
	if line := findResponse(t, untagged, "* 1 FETCH"); !strings.Contains(line, "MODSEQ") {
		t.Errorf("FETCH after ENABLE CONDSTORE has no MODSEQ: %q", line)
	}
}

// A mailbox that tracks modification sequences reports HIGHESTMODSEQ on SELECT
// instead of NOMODSEQ.
func TestLoopbackCondStoreSelectReportsHighestModSeq(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "B1 SELECT INBOX (CONDSTORE)\r\n")
	untagged, tagged := collectUntilTag(t, reader, "B1 ")
	if !strings.HasPrefix(tagged, "B1 OK") {
		t.Fatalf("SELECT (CONDSTORE) failed: %q", tagged)
	}
	var sawHighest bool
	for _, line := range untagged {
		if strings.Contains(line, "HIGHESTMODSEQ") {
			sawHighest = true
		}
		if strings.Contains(line, "NOMODSEQ") {
			t.Errorf("mailbox reported NOMODSEQ while tracking modseqs: %q", line)
		}
	}
	if !sawHighest {
		t.Errorf("SELECT (CONDSTORE) did not report HIGHESTMODSEQ: %v", untagged)
	}
}

// CHANGEDSINCE restricts a FETCH to messages modified after the given
// modification sequence. RFC 7162 section 3.1.4.
func TestLoopbackCondStoreChangedSince(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSessionIn(t, ctx, true)
	writeRawCommand(t, clientSide, "B1 ENABLE CONDSTORE\r\n")
	collectUntilTag(t, reader, "B1 ")
	writeRawCommand(t, clientSide, "B1b SELECT INBOX\r\n")
	collectUntilTag(t, reader, "B1b ")

	// Touch message 3 so it has the newest modification sequence.
	writeRawCommand(t, clientSide, "B2 STORE 3 +FLAGS (\\Flagged)\r\n")
	untagged, tagged := collectUntilTag(t, reader, "B2 ")
	if !strings.HasPrefix(tagged, "B2 OK") {
		t.Fatalf("STORE failed: %q", tagged)
	}
	touched := parseModSeq(t, findResponse(t, untagged, "* 3 FETCH"))

	// Everything strictly older than that modseq is excluded.
	writeRawCommand(t, clientSide, "B3 FETCH 1:* (FLAGS) (CHANGEDSINCE "+strconv.FormatUint(touched-1, 10)+")\r\n")
	untagged, tagged = collectUntilTag(t, reader, "B3 ")
	if !strings.HasPrefix(tagged, "B3 OK") {
		t.Fatalf("FETCH CHANGEDSINCE failed: %q", tagged)
	}
	var fetched []string
	for _, line := range untagged {
		if strings.Contains(line, "FETCH") {
			fetched = append(fetched, line)
		}
	}
	if len(fetched) != 1 || !strings.HasPrefix(fetched[0], "* 3 FETCH") {
		t.Errorf("CHANGEDSINCE returned %v, want only message 3", fetched)
	}
}

// A conditional STORE that rejects a message is a successful command reporting
// MODIFIED, not a failure. RFC 7162 section 3.1.3.
func TestLoopbackCondStoreUnchangedSinceReportsModified(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSessionIn(t, ctx, true)
	writeRawCommand(t, clientSide, "B1 ENABLE CONDSTORE\r\n")
	collectUntilTag(t, reader, "B1 ")
	writeRawCommand(t, clientSide, "B1b SELECT INBOX\r\n")
	collectUntilTag(t, reader, "B1b ")

	writeRawCommand(t, clientSide, "B2 STORE 2 +FLAGS (\\Flagged)\r\n")
	untagged, _ := collectUntilTag(t, reader, "B2 ")
	touched := parseModSeq(t, findResponse(t, untagged, "* 2 FETCH"))

	// Message 2's modseq now exceeds this bound, so it must be left alone
	// while the command still succeeds.
	writeRawCommand(t, clientSide, "B3 STORE 1:2 (UNCHANGEDSINCE "+strconv.FormatUint(touched-1, 10)+") +FLAGS (\\Answered)\r\n")
	untagged, tagged := collectUntilTag(t, reader, "B3 ")
	if !strings.HasPrefix(tagged, "B3 OK") {
		t.Fatalf("conditional STORE should succeed, got %q", tagged)
	}
	if !strings.Contains(tagged, "MODIFIED") {
		t.Errorf("tagged OK has no MODIFIED code: %q", tagged)
	}
	for _, line := range untagged {
		if strings.HasPrefix(line, "* 2 FETCH") && strings.Contains(line, "Answered") {
			t.Errorf("message 2 was modified despite UNCHANGEDSINCE: %q", line)
		}
	}
	// Message 1 was within the bound and must have been stored.
	var storedFirst bool
	for _, line := range untagged {
		if strings.HasPrefix(line, "* 1 FETCH") && strings.Contains(line, "Answered") {
			storedFirst = true
		}
	}
	if !storedFirst {
		t.Errorf("message 1 was inside UNCHANGEDSINCE but was not stored: %v", untagged)
	}
}

// A conditional STORE is refused when the session may not use CONDSTORE at all.
func TestLoopbackCondStoreRefusedWithoutWitness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	clientSide, reader := newUnwitnessedRawSession(t, ctx)

	writeRawCommand(t, clientSide, "B1 STORE 1 (UNCHANGEDSINCE 1) +FLAGS (\\Seen)\r\n")
	if _, tagged := collectUntilTag(t, reader, "B1 "); !strings.HasPrefix(tagged, "B1 BAD") {
		t.Errorf("UNCHANGEDSINCE accepted without CONDSTORE: %q", tagged)
	}
}

// The QRESYNC round trip: a client that was away is told what vanished while it
// was gone, as VANISHED (EARLIER). RFC 7162 section 3.2.
func TestLoopbackQResyncReportsVanished(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSessionIn(t, ctx, true)

	writeRawCommand(t, clientSide, "B1 ENABLE QRESYNC\r\n")
	untagged, tagged := collectUntilTag(t, reader, "B1 ")
	if !strings.HasPrefix(tagged, "B1 OK") {
		t.Fatalf("ENABLE QRESYNC failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* ENABLED"); !strings.Contains(line, "QRESYNC") {
		t.Errorf("ENABLED = %q, want QRESYNC", line)
	}

	writeRawCommand(t, clientSide, "B2 SELECT INBOX\r\n")
	untagged, _ = collectUntilTag(t, reader, "B2 ")
	uidValidity := parseCodeNumber(t, untagged, "UIDVALIDITY")
	before := parseCodeNumber(t, untagged, "HIGHESTMODSEQ")

	// Remove message 1 while "connected", then resynchronise from the modseq
	// observed before the removal.
	writeRawCommand(t, clientSide, "B3 STORE 1 +FLAGS (\\Deleted)\r\n")
	collectUntilTag(t, reader, "B3 ")
	writeRawCommand(t, clientSide, "B4 EXPUNGE\r\n")
	collectUntilTag(t, reader, "B4 ")

	writeRawCommand(t, clientSide, "B5 SELECT INBOX (QRESYNC ("+
		strconv.FormatUint(uidValidity, 10)+" "+strconv.FormatUint(before, 10)+"))\r\n")
	untagged, tagged = collectUntilTag(t, reader, "B5 ")
	if !strings.HasPrefix(tagged, "B5 OK") {
		t.Fatalf("SELECT QRESYNC failed: %q", tagged)
	}
	line := findResponse(t, untagged, "* VANISHED")
	if !strings.Contains(line, "(EARLIER)") {
		t.Errorf("resynchronisation VANISHED is not marked EARLIER: %q", line)
	}
	if !strings.HasSuffix(strings.TrimSpace(line), "1") {
		t.Errorf("VANISHED = %q, want the removed UID 1", line)
	}
}

// RFC 7162 section 3.2.5: QRESYNC must be enabled before it is used as a
// selection parameter, since the client has to be ready for VANISHED first.
func TestLoopbackQResyncRequiresEnableFirst(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "B1 SELECT INBOX (QRESYNC (1 1))\r\n")
	if _, tagged := collectUntilTag(t, reader, "B1 "); !strings.HasPrefix(tagged, "B1 BAD") {
		t.Errorf("SELECT QRESYNC accepted before ENABLE: %q", tagged)
	}
}

// A UIDVALIDITY change makes every UID the client holds meaningless, so the
// resynchronisation is skipped rather than answered against the wrong mailbox.
// RFC 7162 section 3.2.5.1.
func TestLoopbackQResyncStaleUIDValidityReportsNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSessionIn(t, ctx, true)

	writeRawCommand(t, clientSide, "B1 ENABLE QRESYNC\r\n")
	collectUntilTag(t, reader, "B1 ")
	writeRawCommand(t, clientSide, "B1b SELECT INBOX\r\n")
	collectUntilTag(t, reader, "B1b ")
	writeRawCommand(t, clientSide, "B2 STORE 1 +FLAGS (\\Deleted)\r\n")
	collectUntilTag(t, reader, "B2 ")
	writeRawCommand(t, clientSide, "B3 EXPUNGE\r\n")
	collectUntilTag(t, reader, "B3 ")

	writeRawCommand(t, clientSide, "B4 SELECT INBOX (QRESYNC (999999 1))\r\n")
	untagged, tagged := collectUntilTag(t, reader, "B4 ")
	if !strings.HasPrefix(tagged, "B4 OK") {
		t.Fatalf("SELECT QRESYNC failed: %q", tagged)
	}
	for _, line := range untagged {
		if strings.HasPrefix(line, "* VANISHED") {
			t.Errorf("stale UIDVALIDITY still produced a resynchronisation: %q", line)
		}
	}
}

func parseModSeq(t *testing.T, line string) uint64 {
	t.Helper()
	match := modSeqPattern.FindStringSubmatch(line)
	if match == nil {
		t.Fatalf("no MODSEQ in %q", line)
	}
	value, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func parseCodeNumber(t *testing.T, lines []string, code string) uint64 {
	t.Helper()
	pattern := regexp.MustCompile(`\[` + code + ` (\d+)\]`)
	for _, line := range lines {
		if match := pattern.FindStringSubmatch(line); match != nil {
			value, err := strconv.ParseUint(match[1], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
	}
	t.Fatalf("no %s response code in %v", code, lines)
	return 0
}

// newUnwitnessedRawSession is a raw session against a backend that witnesses no
// optional capability, so CONDSTORE is neither advertised nor usable.
func newUnwitnessedRawSession(t *testing.T, ctx context.Context) (net.Conn, *bufio.Reader) {
	t.Helper()
	server := newUnwitnessedServer(t)
	serverSide, clientSide := net.Pipe()
	go func() { _ = server.ServeConn(ctx, serverSide) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	writeRawCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
	collectUntilTag(t, reader, "A1 ")
	writeRawCommand(t, clientSide, "A2 SELECT INBOX\r\n")
	collectUntilTag(t, reader, "A2 ")
	t.Cleanup(func() { _ = clientSide.Close() })
	return clientSide, reader
}
