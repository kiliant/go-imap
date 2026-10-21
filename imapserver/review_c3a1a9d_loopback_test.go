package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func sweepWant(t *testing.T, got []string, status string) {
	t.Helper()
	if !strings.Contains(r4Tagged(got), " "+status) {
		t.Fatalf("response = %q, want %s", got, status)
	}
}

func sweepLiteral(raw string, binary bool) string {
	prefix := ""
	if binary {
		prefix = "~"
	}
	return fmt.Sprintf("%s{%d+}\r\n%s", prefix, len(raw), raw)
}

func TestSweepRefusedLineFraming(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			b := r4Backend(t, kind)
			review6fFill(t, b, 2)
			w := r4Open(t, b, nil)
			w.cmd("S", "SELECT INBOX")
			w.cmd("D", `STORE 2 +FLAGS.SILENT (\Deleted)`)
			for _, command := range []string{
				"REPLACE 1 INBOX b EXPUNGE", "UID REPLACE 1 INBOX b EXPUNGE",
				`REPLACE 1 INBOX (\Seen) b EXPUNGE`,
				`REPLACE 1 INBOX "21-Oct-2026 12:00:00 +0000" b EXPUNGE`,
				`LIST ""  b NOOP`, `LIST "" ( b NOOP)`, `LSUB ""  b NOOP`,
			} {
				w.write("A " + command + "\r\nP NOOP\r\n")
				got := w.until("P")
				if !r4Has(got, `^A BAD`) {
					t.Fatalf("%s: %q", command, got)
				}
				for _, line := range got {
					if strings.HasPrefix(line, "b ") || strings.HasPrefix(line, "* BAD") || strings.Contains(line, " EXPUNGE") {
						t.Fatalf("%s leaked framing: %q", command, got)
					}
				}
			}
			if got := w.cmd("N", "STATUS INBOX (MESSAGES)"); !r4Has(got, `MESSAGES 2`) {
				t.Fatal(got)
			}
		})
	}
}

func TestSweepUIDOnlyAllCommands(t *testing.T) {
	b := r4Memory()
	review6fFill(t, b, 3)
	w := r4Open(t, b, nil)
	w.cmd("E", "ENABLE UIDONLY QRESYNC")
	w.cmd("S", "SELECT INBOX")
	for _, command := range []string{
		"FETCH 1 (FLAGS)", `STORE 1 +FLAGS (\Seen)`, "SEARCH ALL",
		"SORT (DATE) UTF-8 ALL", "THREAD ORDEREDSUBJECT UTF-8 ALL",
		"COPY 1 INBOX", "MOVE 1 INBOX", "FETCH $ (FLAGS)",
		"REPLACE 1 INBOX " + sweepLiteral("Subject: x\r\n\r\nx", false),
		"UID SEARCH 1:2", "UID SEARCH NOT (OR ALL 1:2)", "UID SEARCH *",
		"UID SORT (DATE) UTF-8 NOT 1:2", "UID THREAD ORDEREDSUBJECT UTF-8 OR ALL 1:2",
		"ESEARCH IN (selected) NOT (1:2)", "ESEARCH IN (mailboxes INBOX) 1:2",
		"SELECT INBOX (QRESYNC (1 1 1:3 (1:3 1:3)))",
	} {
		got := w.cmd("A", command)
		sweepWant(t, got, "BAD [UIDREQUIRED]")
	}
	for _, command := range []string{
		"UID SEARCH ALL", "UID SORT (DATE) UTF-8 ALL", "UID THREAD ORDEREDSUBJECT UTF-8 ALL",
		"ESEARCH IN (selected) ALL", "UID SEARCH RETURN (SAVE) UID 1:2", "UID FETCH $ (FLAGS)",
	} {
		sweepWant(t, w.cmd("A", command), "OK")
	}
}

func TestSweepAppendHeaderNegotiation(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			b := r4Backend(t, kind)
			review6fFill(t, b, 1)
			w := r4Open(t, b, nil)
			raw := "Subject: été\r\n\r\nbody"
			sweepWant(t, w.cmd("A", "APPEND INBOX "+sweepLiteral(raw, false)), "NO")
			sweepWant(t, w.cmd("B", "APPEND INBOX "+sweepLiteral("Subject: ASCII\r\n\r\nété", false)), "OK")
			if kind == "memory" {
				w.cmd("S", "SELECT INBOX")
				for _, command := range []string{
					"REPLACE 1 INBOX " + sweepLiteral(raw, false),
					"APPEND INBOX " + sweepLiteral("Subject: ASCII\r\n\r\nbody", false) + " " + sweepLiteral(raw, false),
					"APPEND INBOX CATENATE (TEXT " + sweepLiteral("Subject: ", false) + " TEXT " + sweepLiteral("été\r\n\r\nbody", false) + ")",
				} {
					sweepWant(t, w.cmd("C", command), "NO")
				}
				if got := w.cmd("N", "STATUS INBOX (MESSAGES UIDNEXT)"); !r4Has(got, `MESSAGES 2 UIDNEXT 3`) {
					t.Fatal(got)
				}
			}
			for _, enable := range []string{"UTF8=ACCEPT", "IMAP4rev2"} {
				other := r4Open(t, b, nil)
				sweepWant(t, other.cmd("E", "ENABLE "+enable), "OK")
				sweepWant(t, other.cmd("A", "APPEND INBOX "+sweepLiteral(raw, false)), "OK")
			}
		})
	}
}

type sweepFilter struct {
	*memory.Backend
	token string
}

func (b *sweepFilter) AllowCapability(_ context.Context, name string, _ *imapserver.CapabilityFilterOptions) bool {
	return name != b.token
}

func TestSweepBinaryAndReplacePayloads(t *testing.T) {
	b := r4Memory()
	review6fFill(t, b, 1)
	w := r4Open(t, b, nil)
	if r4Has(w.cmd("C", "CAPABILITY"), ` UTF8=APPEND(?: |$)`) {
		t.Fatal("obsolete UTF8=APPEND advertised")
	}
	raw := "Subject: binary\r\n\r\na\x00b"
	w.write(fmt.Sprintf("A APPEND INBOX ~{%d}\r\n", len(raw)))
	if line, err := w.line(); err != nil || !strings.HasPrefix(line, "+") {
		t.Fatalf("continuation: %q, %v", line, err)
	}
	w.write(raw + "\r\n")
	sweepWant(t, w.until("A"), "OK")
	sweepWant(t, w.cmd("M", "APPEND INBOX "+sweepLiteral(raw, true)+" "+sweepLiteral(raw, true)), "OK")
	w.cmd("S", "SELECT INBOX")
	sweepWant(t, w.cmd("R", "UID REPLACE 1 INBOX "+sweepLiteral(raw, true)), "OK")
	sweepWant(t, w.cmd("R", "REPLACE 1 INBOX CATENATE (TEXT "+sweepLiteral("Subject: replace\r\n\r\n", false)+" URL \"/INBOX/;UID=3/;SECTION=TEXT\")"), "OK")
	for _, filtered := range []string{"BINARY", "CATENATE"} {
		other := r4Open(t, &sweepFilter{Backend: b, token: filtered}, nil)
		other.cmd("S", "SELECT INBOX")
		payload, status := sweepLiteral(raw, true), "BAD"
		if filtered == "CATENATE" {
			payload, status = "CATENATE (TEXT "+sweepLiteral("Subject: ASCII\r\n\r\nbody", false)+")", "NO"
		}
		for _, command := range []string{"APPEND INBOX ", "REPLACE 1 INBOX "} {
			sweepWant(t, other.cmd("F", command+payload), status)
			sweepWant(t, other.cmd("N", "NOOP"), "OK")
		}
	}
}

func TestSweepFetchPartial(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			b := r4Backend(t, kind)
			review6fFill(t, b, 5)
			w := r4Open(t, b, nil)
			w.cmd("E", "ENABLE CONDSTORE")
			w.cmd("S", "SELECT INBOX")
			w.cmd("D", `UID STORE 2 +FLAGS.SILENT (\Deleted)`)
			w.cmd("X", "UID EXPUNGE 2")
			for _, tc := range []struct {
				window string
				uids   []int
			}{
				{"1:2", []int{1, 3}}, {"-1:-2", []int{4, 5}}, {"3:2", []int{3, 4}}, {"8:9", nil},
			} {
				got := w.cmd("F", "UID FETCH 1:* (UID FLAGS) (PARTIAL "+tc.window+")")
				sweepWant(t, got, "OK")
				count := 0
				for _, line := range got {
					if strings.HasPrefix(line, "* ") && strings.Contains(line, " FETCH ") {
						count++
					}
				}
				if count != len(tc.uids) {
					t.Fatalf("%s: %q", tc.window, got)
				}
				for _, uid := range tc.uids {
					if !r4Has(got, fmt.Sprintf(`UID %d[ )]`, uid)) {
						t.Fatalf("missing UID %d: %q", uid, got)
					}
				}
			}
			// Only UID 1 changed after the recorded clock; windowing to the last
			// two first must leave no results, whichever modifier is written first.
			st := w.cmd("H", "STATUS INBOX (HIGHESTMODSEQ)")
			var clock uint64
			for _, line := range st {
				if i := strings.Index(line, "HIGHESTMODSEQ "); i >= 0 {
					_, _ = fmt.Sscanf(line[i:], "HIGHESTMODSEQ %d", &clock)
				}
			}
			if clock == 0 {
				t.Fatal(st)
			}
			w.cmd("T", `UID STORE 1 +FLAGS.SILENT (\Flagged)`)
			for _, modifiers := range []string{fmt.Sprintf("PARTIAL -1:-2 CHANGEDSINCE %d", clock), fmt.Sprintf("CHANGEDSINCE %d PARTIAL -1:-2", clock)} {
				got := w.cmd("F", "UID FETCH 1:* (FLAGS) ("+modifiers+")")
				sweepWant(t, got, "OK")
				if r4Has(got, `^\* .* FETCH `) {
					t.Fatal(got)
				}
			}
			for _, command := range []string{"FETCH 1:* (FLAGS) (PARTIAL 1:2)", "UID FETCH 1:* (FLAGS) (PARTIAL 0:2)", "UID FETCH 1:* (FLAGS) (PARTIAL 1:-2)", "UID FETCH 1:* (FLAGS) (PARTIAL 1:2 PARTIAL 2:3)"} {
				sweepWant(t, w.cmd("F", command), "BAD")
			}
		})
	}
}

func TestSweepSaveFailureEmptiesResult(t *testing.T) {
	b := r4Memory()
	review6fFill(t, b, 3)
	w := r4Open(t, b, nil)
	w.cmd("S", "SELECT INBOX")
	for _, family := range []string{"SEARCH RETURN (SAVE) ", "UID SEARCH RETURN (SAVE) ", "ESEARCH IN (selected) RETURN (SAVE) "} {
		for _, key := range []string{"BADKEY", "SUBJECT nomatch"} {
			sweepWant(t, w.cmd("A", family+"ALL"), "OK")
			got := w.cmd("B", family+key)
			if key == "BADKEY" {
				sweepWant(t, got, "NO")
			} else {
				sweepWant(t, got, "OK")
			}
			if got := w.cmd("F", "UID FETCH $ (UID)"); r4Has(got, `^\* .* FETCH `) {
				t.Fatalf("stale SAVE after %s: %q", family+key, got)
			}
		}
		sweepWant(t, w.cmd("A", family+"ALL"), "OK")
		sweepWant(t, w.cmd("B", family+"OR ALL"), "BAD")
		if got := w.cmd("F", "UID FETCH $ (UID)"); !r4Has(got, `^\* .* FETCH `) {
			t.Fatalf("BAD discarded SAVE: %q", got)
		}
	}
	w.cmd("A", "SEARCH RETURN (SAVE) ALL")
	sweepWant(t, w.cmd("B", "SEARCH RETURN (SAVE) CHARSET NOSUCH SUBJECT x"), "NO [BADCHARSET")
	if got := w.cmd("F", "UID FETCH $ (UID)"); r4Has(got, `^\* .* FETCH `) {
		t.Fatal(got)
	}
}

func TestSweepLiteralMinusTooBig(t *testing.T) {
	for _, command := range []string{"APPEND INBOX ", "APPEND INBOX {1+}\r\nx ", "APPEND INBOX CATENATE (TEXT ", "APPEND INBOX CATENATE (TEXT {1+}\r\nx TEXT ", "REPLACE 1 INBOX ", "UID REPLACE 1 INBOX ", "REPLACE 1 INBOX CATENATE (TEXT {1+}\r\nx TEXT ", "UID REPLACE 1 INBOX CATENATE (TEXT {1+}\r\nx TEXT "} {
		t.Run(command, func(t *testing.T) {
			b := r4Memory()
			review6fFill(t, b, 1)
			w := r4Open(t, &sweepFilter{Backend: b, token: "LITERAL+"}, nil)
			w.cmd("S", "SELECT INBOX")
			w.write("A " + command + "{4097+}\r\n" + strings.Repeat("x", 4097) + "\r\n")
			sweepWant(t, w.until("A"), "BAD [TOOBIG]")
		})
	}
}

func TestSweepReplaceWaitsForTerminator(t *testing.T) {
	for _, suffix := range []string{" junk\r\n", ""} {
		t.Run(fmt.Sprintf("suffix=%q", suffix), func(t *testing.T) {
			b := r4Memory()
			review6fFill(t, b, 1)
			observer := r4Open(t, b, nil)
			before := observer.cmd("B", "STATUS INBOX (MESSAGES UIDNEXT)")[0]
			w := r4Open(t, b, &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{ReadTimeout: 100 * time.Millisecond}})
			w.cmd("S", "SELECT INBOX")
			w.write("R UID REPLACE 1 INBOX " + sweepLiteral("Subject: x\r\n\r\nx", false) + suffix)
			if suffix == "" {
				if line, err := w.line(); err != nil || !strings.HasPrefix(line, "* BYE") {
					t.Fatalf("timeout: %q, %v", line, err)
				}
			} else {
				sweepWant(t, w.until("R"), "BAD")
			}
			after := observer.cmd("A", "STATUS INBOX (MESSAGES UIDNEXT)")[0]
			if before != after {
				t.Fatalf("invalid REPLACE committed: %s -> %s", before, after)
			}
		})
	}
}

func TestSweepFailedSavePipeline(t *testing.T) {
	b := r4Memory()
	review6fFill(t, b, 3)
	w := r4Open(t, b, nil)
	w.cmd("C", "CREATE Trash")
	w.cmd("S", "SELECT INBOX")
	w.cmd("A", "SEARCH RETURN (SAVE) ALL")
	w.write("B SEARCH RETURN (SAVE) BADKEY\r\nM UID MOVE $ Trash\r\nN STATUS INBOX (MESSAGES)\r\n")
	got := w.until("N")
	if !r4Has(got, `^B NO`) || !r4Has(got, `^M OK`) || !r4Has(got, `MESSAGES 3`) {
		t.Fatalf("failed SAVE pipeline changed messages: %q", got)
	}
}
