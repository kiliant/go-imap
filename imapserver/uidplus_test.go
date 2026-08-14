package imapserver_test

// UIDPLUS (RFC 4315). Added by T24 after the goimap interop entry showed the
// capability absent from CAPABILITY while APPENDUID and COPYUID codes were
// being emitted anyway.

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
)

var appendUIDPattern = regexp.MustCompile(`\[APPENDUID \d+ (\d+)\]`)

// TestUIDPlusIsAdvertised guards the advertisement itself. The command and the
// response codes are useless without it: a conforming client decides what to
// send from the capability list, so an unadvertised UIDPLUS is an unusable one.
func TestUIDPlusIsAdvertised(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{})
	h.login()
	h.write("b CAPABILITY\r\n")
	var advertised string
	for {
		line, err := h.reader.ReadString('\n')
		if err != nil {
			t.Fatalf("CAPABILITY: %v", err)
		}
		if strings.HasPrefix(line, "* CAPABILITY") {
			advertised = line
		}
		if strings.HasPrefix(line, "b ") {
			break
		}
	}
	if !strings.Contains(advertised, "UIDPLUS") {
		t.Errorf("UIDPLUS not advertised: %q", advertised)
	}
}

// TestUIDExpungeRemovesOnlyTheNamedMessages is the semantic the command exists
// for, and the reason approximating it with plain EXPUNGE would be a data-loss
// bug: three messages are marked \Deleted, and expunging one UID must leave the
// other two in place.
func TestUIDExpungeRemovesOnlyTheNamedMessages(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{})
	h.login()

	uids := make([]string, 0, 3)
	for i := range 3 {
		message := fmt.Sprintf("Subject: m%d\r\n\r\nbody\r\n", i)
		tag := fmt.Sprintf("a%d", i)
		h.write(fmt.Sprintf("%s APPEND INBOX {%d}\r\n", tag, len(message)))
		if line, err := h.reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+") {
			t.Fatalf("continuation %d = %q, %v", i, line, err)
		}
		h.write(message + "\r\n")
		line := readTagged(t, h.reader, tag)
		if !strings.HasPrefix(line, tag+" OK") {
			t.Fatalf("APPEND %d = %q", i, line)
		}
		// APPENDUID is half of what UIDPLUS is for; without it the client would
		// have to SEARCH to learn the UID it just created.
		match := appendUIDPattern.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("APPEND %d returned no APPENDUID: %q", i, line)
		}
		uids = append(uids, match[1])
	}

	h.write("s SELECT INBOX\r\n")
	if line := readTagged(t, h.reader, "s"); !strings.HasPrefix(line, "s OK") {
		t.Fatalf("SELECT = %q", line)
	}
	h.write("d STORE 1:3 +FLAGS.SILENT (\\Deleted)\r\n")
	if line := readTagged(t, h.reader, "d"); !strings.HasPrefix(line, "d OK") {
		t.Fatalf("STORE = %q", line)
	}

	// Expunge the middle message only. All three are deleted, so a server that
	// ignored the UID set would remove every one of them.
	h.write("e UID EXPUNGE " + uids[1] + "\r\n")
	expunged := 0
	for {
		line, err := h.reader.ReadString('\n')
		if err != nil {
			t.Fatalf("UID EXPUNGE: %v", err)
		}
		if strings.Contains(line, "EXPUNGE") && strings.HasPrefix(line, "*") {
			expunged++
		}
		if strings.HasPrefix(line, "e ") {
			if !strings.HasPrefix(line, "e OK") {
				t.Fatalf("UID EXPUNGE = %q", line)
			}
			break
		}
	}
	if expunged != 1 {
		t.Fatalf("UID EXPUNGE reported %d removals, want exactly 1", expunged)
	}

	// The other two must still be there — still \Deleted, still waiting for an
	// EXPUNGE that names them.
	h.write("f SEARCH ALL\r\n")
	var results string
	for {
		line, err := h.reader.ReadString('\n')
		if err != nil {
			t.Fatalf("SEARCH: %v", err)
		}
		if strings.HasPrefix(line, "* SEARCH") {
			results = strings.TrimSpace(line)
		}
		if strings.HasPrefix(line, "f ") {
			break
		}
	}
	if fields := strings.Fields(results); len(fields) != 4 { // "*", "SEARCH", n, n
		t.Fatalf("SEARCH after UID EXPUNGE = %q, want two surviving messages", results)
	}
}

// TestUIDExpungeIsRefusedWithoutTheCapability pins the gate. A backend that
// does not witness UIDPLUS must not have the command silently degrade into a
// full EXPUNGE, which would destroy messages the client never named.
func TestUIDExpungeIsRefusedWithoutTheCapability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Already logged in and selected, against a backend that witnesses nothing.
	clientSide, reader := newUnwitnessedRawSession(t, ctx)

	writeRawCommand(t, clientSide, "E1 UID EXPUNGE 1\r\n")
	if _, tagged := collectUntilTag(t, reader, "E1 "); !strings.HasPrefix(tagged, "E1 BAD") {
		t.Fatalf("UID EXPUNGE without UIDPLUS = %q, want BAD", tagged)
	}
}
