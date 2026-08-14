package imapserver_test

// IMAP4rev2 (RFC 9051) behaviour, enabled per connection by ENABLE.
//
// SERVER-DESIGN.md is categorical about the bar: IMAP4REV2 is advertised only
// when all of the behaviour RFC 9051 incorporates is implemented, because
// "advertising it otherwise is a lie the client cannot detect". These tests are
// what makes the advertisement checkable rather than asserted.
//
// The capability was gated off until T24. The last missing incorporated
// behaviour was UIDPLUS, which had neither UID EXPUNGE nor an advertisement;
// see uidplus_test.go.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kiliant/go-imap/imapserver"
)

// rev2Session is an authenticated connection with IMAP4rev2 enabled.
func rev2Session(t *testing.T) *securityHarness {
	t.Helper()
	h := newSecurityHarness(t, imapserver.Limits{})
	h.login()
	h.write("e ENABLE IMAP4rev2\r\n")
	var enabled string
	for {
		line, err := h.reader.ReadString('\n')
		if err != nil {
			t.Fatalf("ENABLE: %v", err)
		}
		if strings.HasPrefix(line, "* ENABLED") {
			enabled = strings.TrimSpace(line)
		}
		if strings.HasPrefix(line, "e ") {
			if !strings.HasPrefix(line, "e OK") {
				t.Fatalf("ENABLE IMAP4rev2 = %q", line)
			}
			break
		}
	}
	if !strings.Contains(enabled, "IMAP4REV2") {
		t.Fatalf("ENABLE did not confirm IMAP4rev2: %q", enabled)
	}
	return h
}

// collect reads until the tagged line, returning the untagged lines and it.
func (h *securityHarness) collect(tag string) ([]string, string) {
	h.t.Helper()
	var untagged []string
	for {
		line, err := h.reader.ReadString('\n')
		if err != nil {
			h.t.Fatalf("reading response for %q: %v", tag, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, tag+" ") {
			return untagged, line
		}
		untagged = append(untagged, line)
	}
}

// appendMessage stores one message with a synchronising literal, so tests that
// need a non-empty mailbox do not have to reach past the wire to seed it.
func (h *securityHarness) appendMessage(mailbox, body string) {
	h.t.Helper()
	h.write(fmt.Sprintf("ap APPEND %s {%d}\r\n", mailbox, len(body)))
	for {
		line, err := h.reader.ReadString('\n')
		if err != nil {
			h.t.Fatalf("APPEND continuation: %v", err)
		}
		if strings.HasPrefix(line, "+") {
			break
		}
	}
	h.write(body + "\r\n")
	if _, tagged := h.collect("ap"); !strings.HasPrefix(tagged, "ap OK") {
		h.t.Fatalf("APPEND = %q", tagged)
	}
}

// TestRev2IsAdvertisedAndEnableable is the gate itself. Everything below is
// unreachable by a real client unless this holds.
func TestRev2IsAdvertisedAndEnableable(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{})
	h.login()
	h.write("c CAPABILITY\r\n")
	untagged, tagged := h.collect("c")
	if !strings.HasPrefix(tagged, "c OK") {
		t.Fatalf("CAPABILITY = %q", tagged)
	}
	var advertised string
	for _, line := range untagged {
		if strings.HasPrefix(line, "* CAPABILITY") {
			advertised = line
		}
	}
	if !strings.Contains(advertised, "IMAP4REV2") {
		t.Fatalf("IMAP4REV2 not advertised: %q", advertised)
	}
}

// TestRev2SelectOmitsRecentAndUnseen covers RFC 9051 section 7.3.1 and its
// appendix E: RECENT and the OK [UNSEEN] response are removed in rev2. A rev1
// session must still receive both, so this asserts the difference rather than
// the absence.
func TestRev2SelectOmitsRecentAndUnseen(t *testing.T) {
	h := rev2Session(t)
	h.write("s SELECT INBOX\r\n")
	untagged, tagged := h.collect("s")
	if !strings.HasPrefix(tagged, "s OK") {
		t.Fatalf("SELECT = %q", tagged)
	}
	for _, line := range untagged {
		if strings.Contains(line, "RECENT") {
			t.Errorf("rev2 SELECT sent a RECENT response: %q", line)
		}
		if strings.Contains(line, "[UNSEEN") {
			t.Errorf("rev2 SELECT sent an OK [UNSEEN] response: %q", line)
		}
	}
	// The two that rev2 makes mandatory rather than optional.
	var haveValidity, haveNext bool
	for _, line := range untagged {
		haveValidity = haveValidity || strings.Contains(line, "[UIDVALIDITY")
		haveNext = haveNext || strings.Contains(line, "[UIDNEXT")
	}
	if !haveValidity || !haveNext {
		t.Errorf("rev2 SELECT must carry UIDVALIDITY and UIDNEXT: %q", untagged)
	}
}

// TestRev1SelectStillSendsRecent is the other half: enabling rev2 is opt-in and
// must not change what a rev1 session sees.
func TestRev1SelectStillSendsRecent(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{})
	h.login()
	h.write("s SELECT INBOX\r\n")
	untagged, tagged := h.collect("s")
	if !strings.HasPrefix(tagged, "s OK") {
		t.Fatalf("SELECT = %q", tagged)
	}
	var recent bool
	for _, line := range untagged {
		recent = recent || strings.Contains(line, "RECENT")
	}
	if !recent {
		t.Errorf("rev1 SELECT lost its RECENT response: %q", untagged)
	}
}

// TestRev2SearchReturnsESearch covers RFC 9051 section 7.3.4: rev2 removed the
// untagged SEARCH response, and SEARCH answers with ESEARCH whether or not the
// client sent a RETURN clause. A rev2 client parsing strictly does not
// understand "* SEARCH" at all.
func TestRev2SearchReturnsESearch(t *testing.T) {
	h := rev2Session(t)
	// A match is required, not incidental: on an empty result ESEARCH carries
	// only the tag correlator, so an empty mailbox cannot tell a correct ALL
	// from a missing one.
	h.appendMessage("INBOX", "Subject: hi\r\n\r\nbody\r\n")
	h.write("s SELECT INBOX\r\n")
	if _, tagged := h.collect("s"); !strings.HasPrefix(tagged, "s OK") {
		t.Fatalf("SELECT = %q", tagged)
	}
	h.write("f SEARCH ALL\r\n")
	untagged, tagged := h.collect("f")
	if !strings.HasPrefix(tagged, "f OK") {
		t.Fatalf("SEARCH = %q", tagged)
	}
	for _, line := range untagged {
		if strings.HasPrefix(line, "* SEARCH") {
			t.Errorf("rev2 SEARCH used the rev1 response shape, removed by RFC 9051: %q", line)
		}
	}
	var esearch string
	for _, line := range untagged {
		if strings.HasPrefix(line, "* ESEARCH") {
			esearch = line
		}
	}
	if esearch == "" {
		t.Fatalf("rev2 SEARCH did not answer with ESEARCH: %q", untagged)
	}
	// RFC 9051 section 6.4.4: a SEARCH naming no return options is answered as
	// if it had asked for ALL, and RFC 4731 section 3.1 makes the tag
	// correlator mandatory.
	if !strings.Contains(esearch, `(TAG "f")`) {
		t.Errorf("ESEARCH is missing its tag correlator: %q", esearch)
	}
	if !strings.Contains(esearch, "ALL 1") {
		t.Errorf("ESEARCH did not default to ALL: %q", esearch)
	}
}

// TestRev2IncorporatedBehaviour walks the list SERVER-DESIGN.md section 1 gives
// for what RFC 9051 folds in, and requires each to be reachable from a rev2
// session. The advertisement is a claim about exactly this set, so the set is
// what the test enumerates — one command form per incorporated behaviour,
// asserted through the wire rather than by grepping for a capability token,
// because a token can be advertised by code that no command path reaches.
func TestRev2IncorporatedBehaviour(t *testing.T) {
	for _, probe := range []struct {
		behaviour string
		command   string
	}{
		{"STATUS=SIZE", "STATUS INBOX (MESSAGES SIZE)"},
		{"LIST-EXTENDED selection", `LIST (SUBSCRIBED) "" "*"`},
		{"LIST-EXTENDED multi-pattern", `LIST "" ("INBOX" "INBOX/%")`},
		{"LIST-STATUS", `LIST "" "*" RETURN (STATUS (MESSAGES UIDNEXT))`},
		{"NAMESPACE", "NAMESPACE"},
		{"ESEARCH return options", "SEARCH RETURN (MIN MAX COUNT) ALL"},
		{"SEARCHRES", "SEARCH RETURN (SAVE) ALL"},
		{"UIDPLUS UID EXPUNGE", "UID EXPUNGE 1:*"},
		// Into a different mailbox: moving into the selected one is refused on
		// its own merits, which would make this probe prove nothing.
		{"MOVE", "UID MOVE 1:* Archive"},
		{"CHECK accepted as NOOP", "CHECK"},
		{"UNSELECT", "UNSELECT"},
	} {
		t.Run(probe.behaviour, func(t *testing.T) {
			h := rev2Session(t)
			h.write("m CREATE Archive\r\n")
			if _, tagged := h.collect("m"); !strings.HasPrefix(tagged, "m OK") {
				t.Fatalf("CREATE = %q", tagged)
			}
			h.write("s SELECT INBOX\r\n")
			if _, tagged := h.collect("s"); !strings.HasPrefix(tagged, "s OK") {
				t.Fatalf("SELECT = %q", tagged)
			}
			h.write("p " + probe.command + "\r\n")
			_, tagged := h.collect("p")
			if !strings.HasPrefix(tagged, "p OK") {
				t.Errorf("%s is not reachable from a rev2 session: %q", probe.behaviour, tagged)
			}
		})
	}
}

// TestRev2SelectSendsUntaggedList covers RFC 9051 section 6.3.2, which makes an
// untagged LIST response for the selected mailbox mandatory in SELECT and
// EXAMINE — new in rev2, absent from RFC 3501. It is how a rev2 client learns
// the mailbox's attributes and hierarchy delimiter without a second round trip.
func TestRev2SelectSendsUntaggedList(t *testing.T) {
	h := rev2Session(t)
	h.write("s SELECT INBOX\r\n")
	untagged, tagged := h.collect("s")
	if !strings.HasPrefix(tagged, "s OK") {
		t.Fatalf("SELECT = %q", tagged)
	}
	var list bool
	for _, line := range untagged {
		list = list || strings.HasPrefix(line, "* LIST ")
	}
	if !list {
		t.Errorf("rev2 SELECT must send an untagged LIST for the mailbox: %q", untagged)
	}
}

// TestRev1SelectHasNoUntaggedList is the other half: RFC 3501 has no such
// response in SELECT, and a rev1 client is entitled not to see one.
func TestRev1SelectHasNoUntaggedList(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{})
	h.login()
	h.write("s SELECT INBOX\r\n")
	untagged, tagged := h.collect("s")
	if !strings.HasPrefix(tagged, "s OK") {
		t.Fatalf("SELECT = %q", tagged)
	}
	for _, line := range untagged {
		if strings.HasPrefix(line, "* LIST ") {
			t.Errorf("rev1 SELECT sent a LIST response RFC 3501 does not define: %q", line)
		}
	}
}

// TestRev1SearchKeepsUntaggedSearch is the corresponding rev1 guarantee.
func TestRev1SearchKeepsUntaggedSearch(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{})
	h.login()
	h.write("s SELECT INBOX\r\n")
	if _, tagged := h.collect("s"); !strings.HasPrefix(tagged, "s OK") {
		t.Fatalf("SELECT = %q", tagged)
	}
	h.write("f SEARCH ALL\r\n")
	untagged, tagged := h.collect("f")
	if !strings.HasPrefix(tagged, "f OK") {
		t.Fatalf("SEARCH = %q", tagged)
	}
	var plain bool
	for _, line := range untagged {
		plain = plain || strings.HasPrefix(line, "* SEARCH")
	}
	if !plain {
		t.Errorf("rev1 SEARCH must keep the untagged SEARCH response: %q", untagged)
	}
}
