package imapserver_test

import (
	"context"
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
