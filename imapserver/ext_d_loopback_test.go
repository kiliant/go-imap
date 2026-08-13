package imapserver_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Group D is administrative surface: quota roots, access control lists,
// annotations and namespaces. Each is asserted at the wire, since the shapes —
// a QUOTAROOT followed by its QUOTA responses, NIL versus an empty namespace
// list, NIL versus an empty annotation value — are precisely where a server
// gets these wrong.

func TestLoopbackQuota(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "D1 GETQUOTAROOT INBOX\r\n")
	untagged, tagged := collectUntilTag(t, reader, "D1 ")
	if !strings.HasPrefix(tagged, "D1 OK") {
		t.Fatalf("GETQUOTAROOT failed: %q", tagged)
	}
	if root := findResponse(t, untagged, "* QUOTAROOT"); !strings.Contains(root, "root") {
		t.Errorf("QUOTAROOT = %q, want the root name", root)
	}
	quota := findResponse(t, untagged, "* QUOTA ")
	for _, want := range []string{"STORAGE", "MESSAGE"} {
		if !strings.Contains(quota, want) {
			t.Errorf("QUOTA response has no %s: %q", want, quota)
		}
	}

	writeRawCommand(t, clientSide, "D2 SETQUOTA root (STORAGE 512)\r\n")
	untagged, tagged = collectUntilTag(t, reader, "D2 ")
	if !strings.HasPrefix(tagged, "D2 OK") {
		t.Fatalf("SETQUOTA failed: %q", tagged)
	}
	// RFC 9208 section 4.1.3 expects the new state reported back, so the client
	// need not guess how the server clamped what it asked for.
	if quota := findResponse(t, untagged, "* QUOTA "); !strings.Contains(quota, "512") {
		t.Errorf("SETQUOTA did not report the new limit: %q", quota)
	}

	// A resource the backend does not serve is refused, not silently dropped.
	writeRawCommand(t, clientSide, "D3 SETQUOTA root (NOSUCHRESOURCE 5)\r\n")
	if _, tagged := collectUntilTag(t, reader, "D3 "); !strings.HasPrefix(tagged, "D3 NO") {
		t.Errorf("SETQUOTA accepted an unknown resource: %q", tagged)
	}
}

func TestLoopbackACL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "D1 MYRIGHTS INBOX\r\n")
	untagged, tagged := collectUntilTag(t, reader, "D1 ")
	if !strings.HasPrefix(tagged, "D1 OK") {
		t.Fatalf("MYRIGHTS failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* MYRIGHTS"); !strings.Contains(line, "INBOX") {
		t.Errorf("MYRIGHTS = %q", line)
	}

	writeRawCommand(t, clientSide, "D2 SETACL INBOX bob lrs\r\n")
	if _, tagged := collectUntilTag(t, reader, "D2 "); !strings.HasPrefix(tagged, "D2 OK") {
		t.Fatalf("SETACL failed: %q", tagged)
	}
	writeRawCommand(t, clientSide, "D3 GETACL INBOX\r\n")
	untagged, _ = collectUntilTag(t, reader, "D3 ")
	acl := findResponse(t, untagged, "* ACL")
	if !strings.Contains(acl, "bob") || !strings.Contains(acl, "lrs") {
		t.Errorf("ACL = %q, want bob with lrs", acl)
	}

	// RFC 4314 section 3.1: a leading "+" adds rights rather than replacing.
	writeRawCommand(t, clientSide, "D4 SETACL INBOX bob +w\r\n")
	collectUntilTag(t, reader, "D4 ")
	writeRawCommand(t, clientSide, "D5 GETACL INBOX\r\n")
	untagged, _ = collectUntilTag(t, reader, "D5 ")
	acl = findResponse(t, untagged, "* ACL")
	if !strings.Contains(acl, "lrsw") {
		t.Errorf("ACL after +w = %q, want the union lrsw", acl)
	}

	// Removing the entry is not the same as granting no rights.
	writeRawCommand(t, clientSide, "D6 DELETEACL INBOX bob\r\n")
	collectUntilTag(t, reader, "D6 ")
	writeRawCommand(t, clientSide, "D7 GETACL INBOX\r\n")
	untagged, _ = collectUntilTag(t, reader, "D7 ")
	if acl := findResponse(t, untagged, "* ACL"); strings.Contains(acl, "bob") {
		t.Errorf("DELETEACL left an entry behind: %q", acl)
	}
}

func TestLoopbackMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "D1 SETMETADATA INBOX (/private/comment \"hello\")\r\n")
	if _, tagged := collectUntilTag(t, reader, "D1 "); !strings.HasPrefix(tagged, "D1 OK") {
		t.Fatalf("SETMETADATA failed: %q", tagged)
	}
	writeRawCommand(t, clientSide, "D2 GETMETADATA INBOX /private/comment\r\n")
	untagged, tagged := collectUntilTag(t, reader, "D2 ")
	if !strings.HasPrefix(tagged, "D2 OK") {
		t.Fatalf("GETMETADATA failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* METADATA"); !strings.Contains(line, "hello") {
		t.Errorf("METADATA = %q, want the stored value", line)
	}

	// NIL removes the entry; it is not a value. RFC 5464 section 4.3.
	writeRawCommand(t, clientSide, "D3 SETMETADATA INBOX (/private/comment NIL)\r\n")
	collectUntilTag(t, reader, "D3 ")
	writeRawCommand(t, clientSide, "D4 GETMETADATA INBOX /private/comment\r\n")
	untagged, _ = collectUntilTag(t, reader, "D4 ")
	for _, line := range untagged {
		if strings.HasPrefix(line, "* METADATA") && strings.Contains(line, "hello") {
			t.Errorf("NIL did not remove the entry: %q", line)
		}
	}

	// An empty string is a present, empty value, which is a different thing.
	writeRawCommand(t, clientSide, "D5 SETMETADATA INBOX (/private/comment \"\")\r\n")
	collectUntilTag(t, reader, "D5 ")
	writeRawCommand(t, clientSide, "D6 GETMETADATA INBOX /private/comment\r\n")
	untagged, _ = collectUntilTag(t, reader, "D6 ")
	if line := findResponse(t, untagged, "* METADATA"); !strings.Contains(line, "/private/comment") {
		t.Errorf("an empty value was not stored: %q", line)
	}

	// Server-scope annotations use an empty mailbox name. METADATA-SERVER.
	writeRawCommand(t, clientSide, "D7 SETMETADATA \"\" (/shared/vendor/x \"y\")\r\n")
	if _, tagged := collectUntilTag(t, reader, "D7 "); !strings.HasPrefix(tagged, "D7 OK") {
		t.Fatalf("server-scope SETMETADATA failed: %q", tagged)
	}
	writeRawCommand(t, clientSide, "D8 GETMETADATA \"\" /shared/vendor/x\r\n")
	untagged, _ = collectUntilTag(t, reader, "D8 ")
	if line := findResponse(t, untagged, "* METADATA"); !strings.Contains(line, "y") {
		t.Errorf("server-scope METADATA = %q", line)
	}
}

// RFC 2342 section 5: an absent namespace class is NIL, not an empty list. The
// two say different things — "no such namespace" versus "one exists but has no
// entries".
func TestLoopbackNamespaceUsesNilForAbsentClasses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "D1 NAMESPACE\r\n")
	untagged, tagged := collectUntilTag(t, reader, "D1 ")
	if !strings.HasPrefix(tagged, "D1 OK") {
		t.Fatalf("NAMESPACE failed: %q", tagged)
	}
	line := findResponse(t, untagged, "* NAMESPACE")
	if !strings.Contains(line, `(("" "/"))`) {
		t.Errorf("NAMESPACE personal class = %q", line)
	}
	if strings.Count(line, "NIL") != 2 {
		t.Errorf("NAMESPACE should report NIL for the two absent classes: %q", line)
	}
}

// UNAUTHENTICATE returns the connection to the not-authenticated state, and
// every trace of the previous identity goes with it.
func TestLoopbackUnauthenticate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "D1 UNAUTHENTICATE\r\n")
	if _, tagged := collectUntilTag(t, reader, "D1 "); !strings.HasPrefix(tagged, "D1 OK") {
		t.Fatalf("UNAUTHENTICATE failed: %q", tagged)
	}
	// A selected-state command must now be refused: the selection is gone with
	// the session.
	writeRawCommand(t, clientSide, "D2 FETCH 1 (UID)\r\n")
	if _, tagged := collectUntilTag(t, reader, "D2 "); !strings.HasPrefix(tagged, "D2 BAD") {
		t.Errorf("FETCH accepted after UNAUTHENTICATE: %q", tagged)
	}
	// And the connection is reusable for a fresh login, which is the point of
	// the extension.
	writeRawCommand(t, clientSide, "D3 LOGIN alice secret\r\n")
	if _, tagged := collectUntilTag(t, reader, "D3 "); !strings.HasPrefix(tagged, "D3 OK") {
		t.Errorf("could not log in again after UNAUTHENTICATE: %q", tagged)
	}
}

// RFC 9738 puts the value inside the advertised token, so "MESSAGELIMIT" alone
// would not be a legal advertisement.
func TestLoopbackMessageLimitsCarryTheirValue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "D1 CAPABILITY\r\n")
	untagged, _ := collectUntilTag(t, reader, "D1 ")
	line := findResponse(t, untagged, "* CAPABILITY")
	for _, want := range []string{"MESSAGELIMIT=", "SAVELIMIT="} {
		if !strings.Contains(line, want) {
			t.Errorf("capability list has no %s: %q", want, line)
		}
	}
	// The bare prefix must never appear on its own.
	for _, field := range strings.Fields(line) {
		if field == "MESSAGELIMIT" || field == "SAVELIMIT" {
			t.Errorf("%s advertised without its value: %q", field, line)
		}
	}
}
