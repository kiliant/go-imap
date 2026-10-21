package imapserver_test

import (
	"strings"
	"testing"
)

// RFC 5465 section 3: the STATUS responses a NOTIFY SET STATUS writes before
// its tagged OK MUST carry HIGHESTMODSEQ when FlagChange or AnnotationChange is
// registered and the server supports CONDSTORE or QRESYNC, whether or not the
// client has enabled CONDSTORE. Event STATUS responses carry it only after
// enabling (section 5.2), which the existing overflow test pins.
func TestNotifyInitialStatusHighestModSeq(t *testing.T) {
	w := r4Open(t, r4Memory(), nil)
	sweepWant(t, w.cmd("C", "CREATE Other"), "OK")
	r4Append(w, "Other", "", "one")
	for _, tc := range []struct {
		events string
		want   bool
	}{
		{"MessageNew MessageExpunge FlagChange", true},
		{"MessageNew MessageExpunge", false},
	} {
		got := w.cmd("N", "NOTIFY SET STATUS (mailboxes Other ("+tc.events+"))")
		sweepWant(t, got, "OK")
		status := ""
		for _, line := range got {
			if strings.HasPrefix(line, "* STATUS Other ") {
				status = line
			}
		}
		if status == "" {
			t.Fatalf("NOTIFY SET STATUS (%s) wrote no STATUS: %q", tc.events, got)
		}
		if has := strings.Contains(status, "HIGHESTMODSEQ "); has != tc.want {
			t.Errorf("NOTIFY SET STATUS (%s) = %q; HIGHESTMODSEQ present %v, want %v", tc.events, status, has, tc.want)
		}
		if !strings.Contains(status, "UIDVALIDITY ") {
			t.Errorf("initial STATUS lacks UIDVALIDITY: %q", status)
		}
	}
	sweepWant(t, w.cmd("X", "NOTIFY NONE"), "OK")
}
