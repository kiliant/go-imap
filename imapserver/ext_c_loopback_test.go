package imapserver_test

import (
	"bufio"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// SORT's answer is an order, so the assertions are about sequence, not
// membership — a SORT that returned the right messages in mailbox order would
// be wrong and a set comparison would not notice.

func TestLoopbackSort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	// The seeded messages have subjects "seeded a", "seeded b", "seeded c" in
	// arrival order, so a reverse subject sort must invert them.
	writeRawCommand(t, clientSide, "C1 SORT (SUBJECT) UTF-8 ALL\r\n")
	untagged, tagged := collectUntilTag(t, reader, "C1 ")
	if !strings.HasPrefix(tagged, "C1 OK") {
		t.Fatalf("SORT failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* SORT"); strings.TrimSpace(line) != "* SORT 1 2 3" {
		t.Errorf("SORT (SUBJECT) = %q, want ascending 1 2 3", line)
	}

	writeRawCommand(t, clientSide, "C2 SORT (REVERSE SUBJECT) UTF-8 ALL\r\n")
	untagged, tagged = collectUntilTag(t, reader, "C2 ")
	if !strings.HasPrefix(tagged, "C2 OK") {
		t.Fatalf("reverse SORT failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* SORT"); strings.TrimSpace(line) != "* SORT 3 2 1" {
		t.Errorf("SORT (REVERSE SUBJECT) = %q, want descending 3 2 1", line)
	}

	// UID SORT returns UIDs in the same order.
	writeRawCommand(t, clientSide, "C3 UID SORT (REVERSE ARRIVAL) UTF-8 ALL\r\n")
	untagged, tagged = collectUntilTag(t, reader, "C3 ")
	if !strings.HasPrefix(tagged, "C3 OK") {
		t.Fatalf("UID SORT failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* SORT"); strings.TrimSpace(line) != "* SORT 3 2 1" {
		t.Errorf("UID SORT (REVERSE ARRIVAL) = %q", line)
	}
}

// ORDEREDSUBJECT groups by base subject. The seeded messages have three
// distinct subjects, so each is its own thread.
func TestLoopbackThread(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "C1 THREAD ORDEREDSUBJECT UTF-8 ALL\r\n")
	untagged, tagged := collectUntilTag(t, reader, "C1 ")
	if !strings.HasPrefix(tagged, "C1 OK") {
		t.Fatalf("THREAD failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* THREAD"); strings.TrimSpace(line) != "* THREAD (1) (2) (3)" {
		t.Errorf("THREAD = %q, want three single-message threads", line)
	}
}

// An algorithm the backend cannot compute is refused rather than answered with
// a different algorithm's results, which would silently mis-thread the client's
// view.
func TestLoopbackThreadRefusesUnsupportedAlgorithm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "C1 THREAD REFERENCES UTF-8 ALL\r\n")
	if _, tagged := collectUntilTag(t, reader, "C1 "); !strings.HasPrefix(tagged, "C1 NO") {
		t.Errorf("REFERENCES threading was not refused: %q", tagged)
	}
}

// SORT and THREAD are advertised only when the backend witnesses them.
func TestGroupCCapabilitiesRequireBackendWitness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	clientSide, reader := newUnwitnessedRawSession(t, ctx)

	writeRawCommand(t, clientSide, "C1 CAPABILITY\r\n")
	untagged, _ := collectUntilTag(t, reader, "C1 ")
	line := findResponse(t, untagged, "* CAPABILITY")
	for _, witnessed := range []string{"SORT", "SORT=DISPLAY", "THREAD", "SEARCH=FUZZY"} {
		if strings.Contains(line, " "+witnessed) {
			t.Errorf("%s advertised without a backend witness: %q", witnessed, line)
		}
	}
	writeRawCommand(t, clientSide, "C2 SORT (SUBJECT) UTF-8 ALL\r\n")
	if _, tagged := collectUntilTag(t, reader, "C2 "); !strings.HasPrefix(tagged, "C2 BAD") {
		t.Errorf("SORT accepted without backend support: %q", tagged)
	}
}

// PARTIAL windows an ESEARCH result. RFC 9394.
func TestLoopbackSearchPartial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "C1 SEARCH RETURN (PARTIAL 1:2) ALL\r\n")
	untagged, tagged := collectUntilTag(t, reader, "C1 ")
	if !strings.HasPrefix(tagged, "C1 OK") {
		t.Fatalf("SEARCH RETURN (PARTIAL) failed: %q", tagged)
	}
	line := findResponse(t, untagged, "* ESEARCH")
	if !strings.Contains(line, "PARTIAL (1:2 1:2)") {
		t.Errorf("PARTIAL = %q, want the window and its messages", line)
	}
	// PARTIAL alone must not also produce ALL, or the windowing saves nothing.
	if strings.Contains(line, " ALL ") {
		t.Errorf("PARTIAL implied ALL: %q", line)
	}

	// A negative range counts back from the end.
	writeRawCommand(t, clientSide, "C2 SEARCH RETURN (PARTIAL -1:-1) ALL\r\n")
	untagged, _ = collectUntilTag(t, reader, "C2 ")
	if line := findResponse(t, untagged, "* ESEARCH"); !strings.Contains(line, "PARTIAL (-1:-1 3)") {
		t.Errorf("negative PARTIAL = %q, want the last message", line)
	}

	// A window past the end is NIL, not an omission: the client must be able
	// to tell "nothing there" from "the server ignored PARTIAL".
	writeRawCommand(t, clientSide, "C3 SEARCH RETURN (PARTIAL 50:60) ALL\r\n")
	untagged, _ = collectUntilTag(t, reader, "C3 ")
	if line := findResponse(t, untagged, "* ESEARCH"); !strings.Contains(line, "PARTIAL (50:60 NIL)") {
		t.Errorf("out-of-range PARTIAL = %q, want NIL", line)
	}

	// A range mixing signs has no meaning and is refused.
	writeRawCommand(t, clientSide, "C4 SEARCH RETURN (PARTIAL 1:-2) ALL\r\n")
	if _, tagged := collectUntilTag(t, reader, "C4 "); !strings.HasPrefix(tagged, "C4 BAD") {
		t.Errorf("mixed-sign PARTIAL accepted: %q", tagged)
	}
}

// MULTISEARCH searches mailboxes the connection has not selected, and names the
// mailbox and UIDVALIDITY so the UIDs mean something. RFC 7377.
func TestLoopbackMultiSearch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "C1 ESEARCH IN (INBOX) ALL\r\n")
	untagged, tagged := collectUntilTag(t, reader, "C1 ")
	if !strings.HasPrefix(tagged, "C1 OK") {
		t.Fatalf("ESEARCH failed: %q", tagged)
	}
	line := findResponse(t, untagged, "* ESEARCH")
	for _, want := range []string{"MAILBOX", "UIDVALIDITY", "UID", "ALL"} {
		if !strings.Contains(line, want) {
			t.Errorf("ESEARCH response has no %s: %q", want, line)
		}
	}

	// A mailbox with no matches produces no response at all, which is how a
	// client distinguishes it from one that was not searched.
	writeRawCommand(t, clientSide, "C2 CREATE Empty\r\n")
	collectUntilTag(t, reader, "C2 ")
	writeRawCommand(t, clientSide, "C3 ESEARCH IN (Empty) ALL\r\n")
	untagged, tagged = collectUntilTag(t, reader, "C3 ")
	if !strings.HasPrefix(tagged, "C3 OK") {
		t.Fatalf("ESEARCH on an empty mailbox failed: %q", tagged)
	}
	for _, line := range untagged {
		if strings.HasPrefix(line, "* ESEARCH") {
			t.Errorf("empty mailbox produced an ESEARCH response: %q", line)
		}
	}
}

// BINARY[] delivers the section decoded, which is the point of RFC 3516.
func TestLoopbackBinaryFetch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "C1 FETCH 1 (BINARY.SIZE[])\r\n")
	untagged, tagged := collectUntilTag(t, reader, "C1 ")
	if !strings.HasPrefix(tagged, "C1 OK") {
		t.Fatalf("BINARY.SIZE fetch failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* 1 FETCH"); !strings.Contains(line, "BINARY.SIZE[]") {
		t.Errorf("BINARY.SIZE = %q", line)
	}
}

// MULTIAPPEND stores several messages in one command. Each literal's length is
// only knowable once the previous one is off the wire, so this exercises the
// interleaved parse the extension needed. RFC 3502.
func TestLoopbackMultiAppend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	first := "Subject: multi one\r\n\r\none\r\n"
	second := "Subject: multi two\r\n\r\ntwo\r\n"
	writeRawCommand(t, clientSide, "C1 APPEND INBOX {"+strconv.Itoa(len(first))+"}\r\n")
	expectContinuation(t, reader)
	writeRawCommand(t, clientSide, first+" {"+strconv.Itoa(len(second))+"}\r\n")
	expectContinuation(t, reader)
	writeRawCommand(t, clientSide, second+"\r\n")
	_, tagged := collectUntilTag(t, reader, "C1 ")
	if !strings.HasPrefix(tagged, "C1 OK") {
		t.Fatalf("MULTIAPPEND failed: %q", tagged)
	}
	// RFC 3502 extends APPENDUID to a UID set, so both messages are named.
	if !strings.Contains(tagged, "APPENDUID") {
		t.Errorf("MULTIAPPEND did not report APPENDUID: %q", tagged)
	}

	writeRawCommand(t, clientSide, "C2 STATUS INBOX (MESSAGES)\r\n")
	untagged, _ := collectUntilTag(t, reader, "C2 ")
	if line := findResponse(t, untagged, "* STATUS"); !strings.Contains(line, "MESSAGES 5") {
		t.Errorf("STATUS after MULTIAPPEND = %q, want the 3 seeded plus 2", line)
	}
}

// CATENATE builds a message from client text and server-side URLs, without the
// client having to fetch and re-upload the parts. RFC 4469.
func TestLoopbackCatenate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	header := "Subject: catenated\r\n\r\n"
	writeRawCommand(t, clientSide, "C1 APPEND INBOX CATENATE (TEXT {"+strconv.Itoa(len(header))+"}\r\n")
	expectContinuation(t, reader)
	// A URL part naming a message already on the server, then the closing
	// parenthesis.
	writeRawCommand(t, clientSide, header+" URL \"imap://alice@example.com/INBOX/;UID=1\")\r\n")
	_, tagged := collectUntilTag(t, reader, "C1 ")
	if !strings.HasPrefix(tagged, "C1 OK") {
		t.Fatalf("CATENATE failed: %q", tagged)
	}

	// The stored message is the header followed by the referenced message's
	// bytes, which is what distinguishes CATENATE from an ordinary append.
	writeRawCommand(t, clientSide, "C2 UID FETCH 4 (BODY.PEEK[])\r\n")
	untagged, tagged := collectUntilTag(t, reader, "C2 ")
	if !strings.HasPrefix(tagged, "C2 OK") {
		t.Fatalf("FETCH of the catenated message failed: %q", tagged)
	}
	joined := strings.Join(untagged, "\n")
	if !strings.Contains(joined, "catenated") {
		t.Errorf("catenated message lacks its own header: %q", joined)
	}
	if !strings.Contains(joined, "seeded a") {
		t.Errorf("catenated message lacks the referenced part: %q", joined)
	}
}

// expectContinuation reads a "+" continuation request.
func expectContinuation(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "+") {
		t.Fatalf("expected a continuation request, got %q", line)
	}
}
