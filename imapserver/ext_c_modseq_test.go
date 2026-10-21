package imapserver_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSortThreadMultiSearchModSeq(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, conn, reader := newGroupARawSession(t, ctx)
	// Learn the per-message clocks without depending on memory's initial counter.
	writeRawCommand(t, conn, "s SELECT INBOX (CONDSTORE)\r\n")
	collectUntilTag(t, reader, "s ")
	writeRawCommand(t, conn, "f FETCH 1:3 (UID MODSEQ)\r\n")
	lines, _ := collectUntilTag(t, reader, "f ")
	var highest uint64
	for _, line := range lines {
		if m := modSeqPattern.FindStringSubmatch(line); m != nil {
			n, _ := strconv.ParseUint(m[1], 10, 64)
			highest = max(highest, n)
		}
	}
	if highest == 0 {
		t.Fatal("missing message clocks")
	}
	// Only message 1 now matches this threshold. A second change outside the
	// result makes the mailbox's HIGHESTMODSEQ an incorrect aggregate.
	writeRawCommand(t, conn, "u STORE 1 +FLAGS.SILENT (\\Flagged)\r\n")
	collectUntilTag(t, reader, "u ")
	writeRawCommand(t, conn, "v STORE 3 +FLAGS.SILENT (\\Answered)\r\n")
	collectUntilTag(t, reader, "v ")
	writeRawCommand(t, conn, "w FETCH 1 (MODSEQ)\r\n")
	lines, _ = collectUntilTag(t, reader, "w ")
	m := modSeqPattern.FindStringSubmatch(findResponse(t, lines, "* 1 FETCH"))
	if m == nil {
		t.Fatal("no MODSEQ for changed message")
	}
	matchModSeq := m[1]
	for i, tc := range []struct{ command, want string }{
		{"SORT (ARRIVAL) US-ASCII MODSEQ 1", fmt.Sprintf("* SORT 1 2 3 (MODSEQ %d)", highest+2)},
		{"UID SORT (REVERSE ARRIVAL) US-ASCII MODSEQ 1", fmt.Sprintf("* SORT 3 2 1 (MODSEQ %d)", highest+2)},
		{fmt.Sprintf("SORT (ARRIVAL) US-ASCII FLAGGED MODSEQ %d", highest+1), "* SORT 1 (MODSEQ " + matchModSeq + ")"},
		{"SORT (ARRIVAL) US-ASCII MODSEQ 9223372036854775807", "* SORT"},
		{"SORT (ARRIVAL) US-ASCII ALL", "* SORT 1 2 3"},
		{"THREAD ORDEREDSUBJECT US-ASCII MODSEQ 1", "* THREAD (1)(2)(3)"},
		{fmt.Sprintf("UID THREAD ORDEREDSUBJECT US-ASCII FLAGGED MODSEQ %d", highest+1), "* THREAD (1)"},
		{"THREAD ORDEREDSUBJECT US-ASCII MODSEQ 9223372036854775807", "* THREAD"},
		{"ESEARCH IN (mailboxes INBOX) MODSEQ 1", "ALL 1:3"},
		{fmt.Sprintf("ESEARCH IN (mailboxes INBOX) FLAGGED MODSEQ %d", highest+1), "ALL 1"},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			tag := fmt.Sprintf("c%d", i)
			writeRawCommand(t, conn, tag+" "+tc.command+"\r\n")
			lines, tagged := collectUntilTag(t, reader, tag+" ")
			if !strings.HasPrefix(tagged, tag+" OK") {
				t.Fatalf("%s: %q", tc.command, tagged)
			}
			if strings.HasPrefix(tc.want, "*") {
				if len(lines) != 1 || lines[0] != tc.want {
					t.Errorf("%s: %v, want %q", tc.command, lines, tc.want)
				}
			} else if !strings.Contains(strings.Join(lines, " "), tc.want) {
				t.Errorf("%s: %v, want %q", tc.command, lines, tc.want)
			}
		})
	}
}
