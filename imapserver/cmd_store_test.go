package imapserver_test

// STORE argument syntax. Found by T24: Dovecot's imaptest could not set a flag
// on this server at all, because it sends the unparenthesised form that RFC
// 3501 and RFC 9051 both permit and that nothing in this repository generated.

import (
	"strings"
	"testing"

	"github.com/kiliant/go-imap/imapserver"
)

// TestStoreAcceptsBothFlagListForms pins both branches of store-att-flags:
//
//	store-att-flags = (["+" / "-"] "FLAGS" [".SILENT"]) SP
//	                  (flag-list / (flag *(SP flag)))
//
// The parenthesised form was always accepted, so it is here to prove the fix
// did not trade one form for the other.
func TestStoreAcceptsBothFlagListForms(t *testing.T) {
	for _, form := range []struct {
		name    string
		command string
		want    []string
	}{
		{"parenthesised, one flag", `STORE 1 +FLAGS (\Deleted)`, []string{`\Deleted`}},
		{"bare, one flag", `STORE 1 +FLAGS \Deleted`, []string{`\Deleted`}},
		{"bare, silent", `STORE 1 +FLAGS.SILENT \Deleted`, nil},
		{"bare, several flags", `STORE 1 +FLAGS \Answered \Flagged`, []string{`\Answered`, `\Flagged`}},
		{"bare keyword", `STORE 1 +FLAGS $Label1`, []string{"$Label1"}},
		{"parenthesised, several", `STORE 1 +FLAGS (\Answered \Flagged)`, []string{`\Answered`, `\Flagged`}},
		{"bare, set rather than add", `STORE 1 FLAGS \Seen`, []string{`\Seen`}},
		{"bare, remove", `STORE 1 -FLAGS \Seen`, nil},
	} {
		t.Run(form.name, func(t *testing.T) {
			h := newSecurityHarness(t, imapserver.Limits{})
			h.login()
			h.appendMessage("INBOX", "Subject: s\r\n\r\nbody\r\n")
			h.write("s SELECT INBOX\r\n")
			if _, tagged := h.collect("s"); !strings.HasPrefix(tagged, "s OK") {
				t.Fatalf("SELECT = %q", tagged)
			}

			h.write("t " + form.command + "\r\n")
			untagged, tagged := h.collect("t")
			if !strings.HasPrefix(tagged, "t OK") {
				t.Fatalf("%s = %q, want OK", form.command, tagged)
			}
			// A .SILENT store reports nothing, which is the point of it.
			if strings.Contains(form.command, ".SILENT") {
				for _, line := range untagged {
					if strings.Contains(line, "FETCH") {
						t.Errorf(".SILENT store still reported the update: %q", line)
					}
				}
				return
			}
			for _, want := range form.want {
				if !anyLineContains(untagged, want) {
					t.Errorf("%s did not report %s in the FETCH response: %q",
						form.command, want, untagged)
				}
			}
		})
	}
}

// TestStoreRejectsMalformedFlagLists keeps the new branch from being a hole:
// accepting a bare flag list must not mean accepting anything at all.
func TestStoreRejectsMalformedFlagLists(t *testing.T) {
	for _, command := range []string{
		`STORE 1 +FLAGS`,                     // no flags at all
		`STORE 1 +FLAGS (\Deleted`,           // unclosed list
		`STORE 1 +FLAGS \Deleted)`,           // stray close
		`STORE 1 +FLAGS \Answered  \Flagged`, // double space, so an empty flag
		`STORE 1 +BOGUS \Deleted`,            // not a STORE operation
	} {
		t.Run(command, func(t *testing.T) {
			h := newSecurityHarness(t, imapserver.Limits{})
			h.login()
			h.appendMessage("INBOX", "Subject: s\r\n\r\nbody\r\n")
			h.write("s SELECT INBOX\r\n")
			if _, tagged := h.collect("s"); !strings.HasPrefix(tagged, "s OK") {
				t.Fatalf("SELECT = %q", tagged)
			}
			h.write("t " + command + "\r\n")
			if _, tagged := h.collect("t"); !strings.HasPrefix(tagged, "t BAD") {
				t.Errorf("%s = %q, want BAD", command, tagged)
			}
		})
	}
}

func anyLineContains(lines []string, want string) bool {
	for _, line := range lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}
