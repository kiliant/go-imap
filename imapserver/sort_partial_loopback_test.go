package imapserver_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// TestSortReturnPartial pins RFC 5267 section 4.4's PARTIAL for SORT under
// CONTEXT=SORT, as RFC 9394 extends it: the window is taken from the sorted
// order, reported with the normalized range, and NIL when it holds nothing.
// Ascending runs are written as ranges and every other order is spelled out
// (RFC 5267 section 3.2). The cases include imaptest's sort-partial script.
func TestSortReturnPartial(t *testing.T) {
	w := r4Open(t, r4Memory(), nil)
	for i := 1; i <= 5; i++ {
		raw := fmt.Sprintf("Subject: m%d\r\n\r\nbody\r\n", i)
		sweepWant(t, w.cmd("A", fmt.Sprintf(`APPEND INBOX "0%d-Jan-2024 10:00:00 +0000" {%d+}`+"\r\n%s", i, len(raw), raw)), "OK")
	}
	sweepWant(t, w.cmd("S", "SELECT INBOX"), "OK")

	for _, tc := range []struct{ command, want string }{
		{"SORT RETURN (PARTIAL 1:1) (ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") PARTIAL (1:1 1)`},
		{"SORT RETURN (PARTIAL 2:4) (ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") PARTIAL (2:4 2:4)`},
		{"SORT RETURN (PARTIAL 4:2) (ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") PARTIAL (2:4 2:4)`},
		{"SORT RETURN (PARTIAL 2:4) (REVERSE ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") PARTIAL (2:4 4,3,2)`},
		{"SORT RETURN (PARTIAL 1:6) (ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") PARTIAL (1:6 1:5)`},
		{"SORT RETURN (PARTIAL 1:6) (REVERSE ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") PARTIAL (1:6 5,4,3,2,1)`},
		{"SORT RETURN (PARTIAL -1:-2) (ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") PARTIAL (-2:-1 4:5)`},
		{"SORT RETURN (PARTIAL 6:9) (ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") PARTIAL (6:9 NIL)`},
		{"UID SORT RETURN (PARTIAL 1:2) (REVERSE ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") UID PARTIAL (1:2 5,4)`},
		{"SORT RETURN (COUNT PARTIAL 1:2) (ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") COUNT 5 PARTIAL (1:2 1:2)`},
		{"SORT RETURN (ALL) (ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") ALL 1:5`},
		{"SORT RETURN (ALL) (REVERSE ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") ALL 5,4,3,2,1`},
		// RFC 5267 forbids a client to send these; like SEARCH (#206), the
		// server accepts them, reporting both items and the last range.
		{"SORT RETURN (PARTIAL 1:2 ALL) (ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") ALL 1:5 PARTIAL (1:2 1:2)`},
		{"SORT RETURN (PARTIAL 1:2 PARTIAL 3:4) (ARRIVAL) US-ASCII ALL", `* ESEARCH (TAG "P") PARTIAL (3:4 3:4)`},
	} {
		got := w.cmd("P", tc.command)
		sweepWant(t, got, "OK")
		if !r4Has(got, "^"+regexp.QuoteMeta(tc.want)+"$") {
			t.Errorf("%s = %q, want %s", tc.command, got, tc.want)
		}
	}
	for _, command := range []string{
		"SORT RETURN (PARTIAL 1) (ARRIVAL) US-ASCII ALL",
		"SORT RETURN (PARTIAL 1:*) (ARRIVAL) US-ASCII ALL",
		"SORT RETURN (PARTIAL *:1) (ARRIVAL) US-ASCII ALL",
		"SORT RETURN (PARTIAL 0:1) (ARRIVAL) US-ASCII ALL",
		"SORT RETURN (PARTIAL -1:2) (ARRIVAL) US-ASCII ALL",
		"SORT RETURN (PARTIAL) (ARRIVAL) US-ASCII ALL",
	} {
		got := w.cmd("B", command)
		if !strings.HasPrefix(r4Tagged(got), "B BAD ") {
			t.Errorf("%s = %q, want BAD", command, got)
		}
		// The refusal must leave the connection synchronized.
		sweepWant(t, w.cmd("N", "NOOP"), "OK")
	}
}
