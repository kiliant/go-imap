package imapserver_test

import (
	"strconv"
	"strings"
	"testing"
)

// TestBinaryAppendWithNULIsFetchable pins imaptest's append-binary script. A
// literal8 APPEND may store a part with NUL octets; RFC 3516 section 6 then
// requires BODY and BODYSTRUCTURE to describe a CTE IMAP can transmit, and
// BINARY to return the original octets. FETCH BODY used to close the
// connection, because no IMAP string can carry NUL.
func TestBinaryAppendWithNULIsFetchable(t *testing.T) {
	w := r4Open(t, r4Memory(), nil)
	payload := "\x00-\xff\n\r\n"
	raw := "From: user@domain.org\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: binary\r\n\r\n" + payload
	sweepWant(t, w.cmd("A", "APPEND INBOX ~{"+strconv.Itoa(len(raw))+"+}\r\n"+raw), "OK")
	sweepWant(t, w.cmd("S", "SELECT INBOX"), "OK")

	got := w.cmd("F", "FETCH 1 (BODYSTRUCTURE BODY.PEEK[1])")
	sweepWant(t, got, "OK")
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, `"application" "octet-stream" NIL NIL NIL "base64"`) {
		t.Errorf("BODYSTRUCTURE does not describe base64: %q", got)
	}
	// "AC3/Cg0K" and a final CRLF; the unit tests compare the octets.
	if !strings.Contains(joined, "BODY[1] {10}") {
		t.Errorf("BODY[1] = %q, want the 10-octet base64 form", got)
	}

	got = w.cmd("B", "FETCH 1 (BINARY.SIZE[1] BINARY.PEEK[1])")
	sweepWant(t, got, "OK")
	if !r4Has(got, `BINARY\.SIZE\[1\] 6`) || !strings.Contains(strings.Join(got, "\n"), "BINARY[1] ~{6}") {
		t.Errorf("BINARY = %q, want the original 6 octets as literal8", got)
	}
	sweepWant(t, w.cmd("N", "NOOP"), "OK")
}
