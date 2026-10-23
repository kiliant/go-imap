package imapserver_test

import (
	"slices"
	"strings"
	"testing"
)

// enabledLines returns the untagged ENABLED responses of one ENABLE command.
func enabledLines(t *testing.T, w *r4Wire, tag, names string) []string {
	t.Helper()
	lines := w.cmd(tag, "ENABLE "+names)
	if got := r4Tagged(lines); !strings.HasPrefix(got, tag+" OK") {
		t.Fatalf("ENABLE %s = %q", names, lines)
	}
	var enabled []string
	for _, line := range lines[:len(lines)-1] {
		if line == "* ENABLED" || strings.HasPrefix(line, "* ENABLED ") {
			enabled = append(enabled, line)
		}
	}
	return enabled
}

// Each ENABLED response names only the extensions its own command enabled,
// and a successful ENABLE always sends one (RFC 5161 section 3.1).
func TestEnableReportsOnlyNewlyEnabled(t *testing.T) {
	steps := []struct {
		name  string
		setup []string
		names string
		want  string
	}{
		{name: "duplicate names", names: "CONDSTORE condstore", want: "* ENABLED CONDSTORE"},
		{name: "repeated command", setup: []string{"ENABLE CONDSTORE"}, names: "CONDSTORE", want: "* ENABLED"},
		{name: "unknown name", names: "X-UNKNOWN", want: "* ENABLED"},
		{name: "unknown beside known", names: "X-UNKNOWN CONDSTORE", want: "* ENABLED CONDSTORE"},
		{name: "implied earlier", setup: []string{"ENABLE QRESYNC"}, names: "CONDSTORE", want: "* ENABLED"},
		{name: "implied in the same command", names: "QRESYNC CONDSTORE", want: "* ENABLED QRESYNC CONDSTORE"},
		{name: "partly new", setup: []string{"ENABLE CONDSTORE"}, names: "CONDSTORE QRESYNC", want: "* ENABLED QRESYNC"},
		{name: "after a CONDSTORE enabling command", setup: []string{"SELECT INBOX (CONDSTORE)", "UNSELECT"}, names: "CONDSTORE", want: "* ENABLED"},
		{name: "repeated IMAP4rev2", setup: []string{"ENABLE IMAP4rev2"}, names: "IMAP4rev2", want: "* ENABLED"},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			w := r4Open(t, r4Memory(), nil)
			for _, command := range step.setup {
				if got := r4Tagged(w.cmd("S", command)); !strings.HasPrefix(got, "S OK") {
					t.Fatalf("%s = %q", command, got)
				}
			}
			got := enabledLines(t, w, "E", step.names)
			if !slices.Equal(got, []string{step.want}) {
				t.Fatalf("ENABLE %s sent %q, want [%q]", step.names, got, step.want)
			}
		})
	}
}

// Reporting nothing new does not disable anything: ENABLE stays additive.
func TestEnableRepeatKeepsExtensionsEnabled(t *testing.T) {
	w := r4Open(t, r4Memory(), nil)
	enabledLines(t, w, "E1", "QRESYNC")
	enabledLines(t, w, "E2", "QRESYNC CONDSTORE")
	lines := w.cmd("S", "SELECT INBOX")
	if !r4Has(lines, `^\* OK \[HIGHESTMODSEQ \d+\]`) {
		t.Fatalf("SELECT after repeated ENABLE lost CONDSTORE: %q", lines)
	}
}
