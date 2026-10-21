package imapserver_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestReviewHEmptyMailboxVanished(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		for _, uidOnly := range []bool{false, true} {
			for _, fresh := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/uidonly=%v/fresh=%v", kind, uidOnly, fresh), func(t *testing.T) {
					b := r4Backend(t, kind)
					review6fFill(t, b, 4)
					w := r4Open(t, b, nil)
					enable := "ENABLE QRESYNC"
					if uidOnly {
						enable += " UIDONLY"
					}
					sweepWant(t, w.cmd("E", enable), "OK")
					sweepWant(t, w.cmd("S", "SELECT INBOX"), "OK")
					clock := reviewGClock(t, w)
					sweepWant(t, w.cmd("D", `UID STORE 1:* +FLAGS.SILENT (\Deleted)`), "OK")
					sweepWant(t, w.cmd("X", "UID EXPUNGE 1:*"), "OK")
					if fresh {
						sweepWant(t, w.cmd("L", "LOGOUT"), "OK")
						w = r4Open(t, b, nil)
						sweepWant(t, w.cmd("E", enable), "OK")
						got := w.cmd("S", "EXAMINE INBOX")
						sweepWant(t, got, "OK")
						if !r4Has(got, `^\* 0 EXISTS$`) || !r4Has(got, `\[UIDNEXT 5\]`) {
							t.Fatal(got)
						}
					}
					for _, tc := range []struct{ set, vanished string }{
						{"1:*", "1:4"}, {"3:*", "3:4"}, {"*:2", "2:4"},
						{"2,4:*", "2,4"}, {"*", ""}, {"100:*", ""}, {"1:4", "1:4"},
					} {
						for _, partial := range []string{"", " PARTIAL 1:1"} {
							got := w.cmd("F", fmt.Sprintf("UID FETCH %s (FLAGS) (CHANGEDSINCE %d VANISHED%s)", tc.set, clock, partial))
							sweepWant(t, got, "OK")
							var vanished []string
							for _, line := range got {
								if strings.HasPrefix(line, "* VANISHED") {
									vanished = append(vanished, line)
								}
								if strings.HasPrefix(line, "* ") && (strings.Contains(line, " FETCH ") || strings.Contains(line, " UIDFETCH ")) {
									t.Fatalf("empty mailbox returned message data: %q", got)
								}
							}
							if tc.vanished == "" && len(vanished) != 0 || tc.vanished != "" && (len(vanished) != 1 || vanished[0] != "* VANISHED (EARLIER) "+tc.vanished) {
								t.Errorf("%s%s: %q; want historical %q", tc.set, partial, got, tc.vanished)
							}
						}
					}
				})
			}
		}
	}
}

func TestReviewHSearchSavedUIDKey(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		for _, mode := range []string{"rev1", "rev2", "uidonly"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				b := r4Backend(t, kind)
				review6fFill(t, b, 4)
				w := r4Open(t, b, nil)
				caps := strings.Join(w.cmd("C", "CAPABILITY"), " ")
				if mode == "rev2" {
					sweepWant(t, w.cmd("E", "ENABLE IMAP4rev2"), "OK")
				} else if mode == "uidonly" {
					sweepWant(t, w.cmd("E", "ENABLE UIDONLY"), "OK")
				}
				sweepWant(t, w.cmd("S", "SELECT INBOX"), "OK")
				// Make sequence numbers differ from UIDs before saving the result.
				sweepWant(t, w.cmd("D", `UID STORE 1 +FLAGS.SILENT (\Deleted)`), "OK")
				sweepWant(t, w.cmd("X", "UID EXPUNGE 1"), "OK")
				sweepWant(t, w.cmd("Q", "UID SEARCH RETURN (SAVE) UID 2:3"), "OK")
				for _, tc := range []struct{ command, equivalent, capability string }{
					{"UID SEARCH UID $ SMALLER 4096", "UID SEARCH $ SMALLER 4096", ""},
					{"UID SEARCH OR UID $ UID 4", "UID SEARCH OR $ UID 4", ""},
					{"UID SEARCH NOT UID $", "UID SEARCH NOT $", ""},
					{"UID SEARCH (UID $)", "UID SEARCH ($)", ""},
					{"UID SEARCH RETURN (MIN MAX) UID $", "UID SEARCH RETURN (MIN MAX) $", ""},
					{"ESEARCH IN (selected) UID $ SMALLER 4096", "ESEARCH IN (selected) $ SMALLER 4096", "MULTISEARCH"},
					{"UID SORT (DATE) UTF-8 UID $", "UID SORT (DATE) UTF-8 $", "SORT"},
					{"UID THREAD ORDEREDSUBJECT UTF-8 UID $", "UID THREAD ORDEREDSUBJECT UTF-8 $", "THREAD=ORDEREDSUBJECT"},
				} {
					if tc.capability != "" && !strings.Contains(caps, " "+tc.capability) {
						continue // Maildir does not advertise SORT/THREAD/MULTISEARCH.
					}
					want := w.cmd("F", tc.equivalent)
					sweepWant(t, want, "OK")
					if got := w.cmd("F", tc.command); !reflect.DeepEqual(got, want) {
						t.Errorf("%s: %q; equivalent returned %q", tc.command, got, want)
					}
				}
				got := w.cmd("F", "UID SEARCH UID $ SMALLER 4096")
				sweepWant(t, got, "OK")
				if mode == "rev2" && !r4Has(got, ` UID ALL 2:3`) || mode != "rev2" && !r4Has(got, `^\* SEARCH 2 3$`) {
					t.Fatal(got)
				}
				if mode != "uidonly" {
					want := w.cmd("F", "SEARCH $")
					sweepWant(t, want, "OK")
					if got := w.cmd("F", "SEARCH UID $"); !reflect.DeepEqual(got, want) {
						t.Fatalf("sequence result: %q; want %q", got, want)
					}
				}
				// SEARCH does not overwrite the saved result without SAVE, and an
				// explicitly saved empty result must never expand to all messages.
				sweepWant(t, w.cmd("Q", "UID SEARCH RETURN (SAVE) UID 99"), "OK")
				got = w.cmd("F", "UID SEARCH UID $ SMALLER 4096")
				sweepWant(t, got, "OK")
				if r4Has(got, `^\* SEARCH [0-9]`) || r4Has(got, ` ALL [0-9]`) {
					t.Fatal(got)
				}
			})
		}
	}
}

func TestReviewHSearchResRemainsFrameworkOwned(t *testing.T) {
	b := &sweepFilter{Backend: r4Memory(), token: "SEARCHRES"}
	review6fFill(t, b, 3)
	w := r4Open(t, b, nil)
	if !r4Has(w.cmd("C", "CAPABILITY"), ` SEARCHRES(?: |$)`) {
		t.Fatal("backend filter withdrew framework-owned SEARCHRES")
	}
	sweepWant(t, w.cmd("S", "SELECT INBOX"), "OK")
	sweepWant(t, w.cmd("Q", "UID SEARCH RETURN (SAVE) UID 2:3"), "OK")
	got := w.cmd("F", "UID SEARCH UID $ SMALLER 4096")
	sweepWant(t, got, "OK")
	if !r4Has(got, `^\* SEARCH 2 3$`) {
		t.Fatal(got)
	}
}

func TestReviewHSearchReturnOptionCompatibility(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			b := r4Backend(t, kind)
			review6fFill(t, b, 3)
			w := r4Open(t, b, nil)
			sweepWant(t, w.cmd("S", "SELECT INBOX"), "OK")
			for _, command := range []string{
				"UID SEARCH RETURN (PARTIAL 1:2 ALL) ALL",
				"UID SEARCH RETURN (ALL PARTIAL 1:2) ALL",
				"UID SEARCH RETURN (SAVE ALL ALL) ALL",
			} {
				got := w.cmd("Q", command)
				sweepWant(t, got, "OK")
				if !r4Has(got, ` UID ALL 1:3`) || strings.Count(strings.Join(got, "\n"), " ALL ") != 1 {
					t.Fatalf("%s: %q", command, got)
				}
				if strings.Contains(command, "PARTIAL") && !r4Has(got, ` PARTIAL \(1:2 1:2\)`) {
					t.Fatal(got)
				}
			}
			got := w.cmd("F", "UID SEARCH UID $")
			if !r4Has(got, `^\* SEARCH 1 2 3$`) {
				t.Fatal(got)
			}
		})
	}
}

func TestReviewH3CatenateUserinfoBoundary(t *testing.T) {
	b := r4Memory()
	review6fFill(t, b, 1)
	w := r4Open(t, b, nil)
	sweepWant(t, w.cmd("C", "CREATE Out"), "OK")
	var appended int
	for _, tc := range []struct {
		userinfo string
		resolve  bool
	}{
		{"", true},
		{"alice@", true},
		{"alice;AUTH=*@", true},
		{";AUTH=*@", false},
		{";AUTH=PLAIN@", false},
		{"@", false},
		{"bob;AUTH=*@", false},
	} {
		raw := "imap://" + tc.userinfo + "elsewhere.example:1143/INBOX/;UID=1"
		got := w.cmd("A", fmt.Sprintf(`APPEND Out CATENATE (URL "%s")`, raw))
		if tc.resolve {
			sweepWant(t, got, "OK")
			appended++
		} else {
			sweepWant(t, got, "NO [BADURL")
		}
		// A refused URL must neither commit a message nor consume a UID;
		// subsequent commands must still be framed correctly.
		got = w.cmd("S", "STATUS Out (MESSAGES UIDNEXT)")
		sweepWant(t, got, "OK")
		if !r4Has(got, fmt.Sprintf(`MESSAGES %d UIDNEXT %d`, appended, appended+1)) {
			t.Fatalf("%s: %q", raw, got)
		}
		sweepWant(t, w.cmd("N", "NOOP"), "OK")
	}
}
