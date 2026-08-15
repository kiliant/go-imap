package imapserver_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
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

// UIDONLY is all-or-nothing: once enabled, no sequence number may reach the
// client in any response, and no command may name one. RFC 9586.
func TestLoopbackUIDOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSessionIn(t, ctx, true)

	writeRawCommand(t, clientSide, "D1 ENABLE UIDONLY\r\n")
	untagged, tagged := collectUntilTag(t, reader, "D1 ")
	if !strings.HasPrefix(tagged, "D1 OK") {
		t.Fatalf("ENABLE UIDONLY failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* ENABLED"); !strings.Contains(line, "UIDONLY") {
		t.Errorf("ENABLED = %q, want UIDONLY", line)
	}
	writeRawCommand(t, clientSide, "D2 SELECT INBOX\r\n")
	collectUntilTag(t, reader, "D2 ")

	// A sequence-number command is refused rather than reinterpreted as UIDs,
	// which would act on different messages.
	writeRawCommand(t, clientSide, "D3 FETCH 1 (FLAGS)\r\n")
	if _, tagged := collectUntilTag(t, reader, "D3 "); !strings.HasPrefix(tagged, "D3 BAD") {
		t.Errorf("sequence-number FETCH accepted under UIDONLY: %q", tagged)
	}

	// The UID form answers with UIDFETCH, and the UID is the response's
	// subject rather than a repeated item.
	writeRawCommand(t, clientSide, "D4 UID FETCH 1 (FLAGS)\r\n")
	untagged, tagged = collectUntilTag(t, reader, "D4 ")
	if !strings.HasPrefix(tagged, "D4 OK") {
		t.Fatalf("UID FETCH failed: %q", tagged)
	}
	line := findResponse(t, untagged, "* UIDFETCH")
	if !strings.HasPrefix(line, "* UIDFETCH 1 (") {
		t.Errorf("UIDFETCH = %q, want the UID as the subject", line)
	}
	if strings.Contains(line, "UID ") {
		t.Errorf("UIDFETCH repeated the UID as an item: %q", line)
	}
	for _, l := range untagged {
		if strings.Contains(l, " FETCH (") && !strings.HasPrefix(l, "* UIDFETCH") {
			t.Errorf("a sequence-numbered FETCH reached a UIDONLY client: %q", l)
		}
	}

	// Removals become VANISHED, since an untagged EXPUNGE carries a sequence
	// number and cannot be sent at all.
	writeRawCommand(t, clientSide, "D5 UID STORE 1 +FLAGS (\\Deleted)\r\n")
	collectUntilTag(t, reader, "D5 ")
	writeRawCommand(t, clientSide, "D6 EXPUNGE\r\n")
	untagged, tagged = collectUntilTag(t, reader, "D6 ")
	if !strings.HasPrefix(tagged, "D6 OK") {
		t.Fatalf("EXPUNGE failed: %q", tagged)
	}
	if line := findResponse(t, untagged, "* VANISHED"); !strings.HasSuffix(strings.TrimSpace(line), "1") {
		t.Errorf("VANISHED = %q, want the removed UID", line)
	}
	for _, l := range untagged {
		if strings.HasSuffix(strings.TrimSpace(l), "EXPUNGE") {
			t.Errorf("an untagged EXPUNGE reached a UIDONLY client: %q", l)
		}
	}
}

// NOTIFY's whole point: a client learns about a mailbox it has not selected,
// from a change made by a different connection. RFC 5465.
func TestLoopbackNotifyReportsUnselectedMailbox(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	backend := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})

	watcher, clientSide, reader := newRawSessionOn(t, ctx, server)
	writeRawCommand(t, clientSide, "N1 CREATE Watched\r\n")
	collectUntilTag(t, reader, "N1 ")

	// The watcher selects INBOX and registers interest in a *different*
	// mailbox, which is what the selection-scoped Updater could never report.
	writeRawCommand(t, clientSide, "N2 SELECT INBOX\r\n")
	collectUntilTag(t, reader, "N2 ")
	writeRawCommand(t, clientSide, "N3 NOTIFY SET (MAILBOXES Watched (MessageNew))\r\n")
	if _, tagged := collectUntilTag(t, reader, "N3 "); !strings.HasPrefix(tagged, "N3 OK") {
		t.Fatalf("NOTIFY SET failed: %q", tagged)
	}
	_ = watcher

	// A second connection appends to the watched mailbox.
	other, _ := openLoopbackClient(t, ctx, server, &imapclient.Options{AllowInsecureAuth: true})
	if err := other.Login(ctx, "alice", "secret", nil); err != nil {
		t.Fatal(err)
	}
	body := "Subject: notified\r\n\r\nhi\r\n"
	if _, err := other.Append(ctx, "Watched", nil, int64(len(body)), strings.NewReader(body)).Wait(ctx); err != nil {
		t.Fatal(err)
	}

	// The watcher hears about it on its next command.
	writeRawCommand(t, clientSide, "N4 NOOP\r\n")
	untagged, tagged := collectUntilTag(t, reader, "N4 ")
	if !strings.HasPrefix(tagged, "N4 OK") {
		t.Fatalf("NOOP failed: %q", tagged)
	}
	var reported bool
	for _, line := range untagged {
		if strings.HasPrefix(line, "* STATUS") && strings.Contains(line, "Watched") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("no NOTIFY report for the unselected mailbox: %v", untagged)
	}

	// NOTIFY NONE stops delivery.
	writeRawCommand(t, clientSide, "N5 NOTIFY NONE\r\n")
	if _, tagged := collectUntilTag(t, reader, "N5 "); !strings.HasPrefix(tagged, "N5 OK") {
		t.Fatalf("NOTIFY NONE failed: %q", tagged)
	}
	if _, err := other.Append(ctx, "Watched", nil, int64(len(body)), strings.NewReader(body)).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	writeRawCommand(t, clientSide, "N6 NOOP\r\n")
	untagged, _ = collectUntilTag(t, reader, "N6 ")
	for _, line := range untagged {
		if strings.HasPrefix(line, "* STATUS") && strings.Contains(line, "Watched") {
			t.Errorf("NOTIFY NONE did not stop delivery: %q", line)
		}
	}
}

// TestLoopbackNotifyVocabulary covers the two halves of the shared NOTIFY
// vocabulary: that the wire's case-insensitive atoms reach a backend in the one
// canonical spelling, and that a name the backend cannot serve is refused.
//
// Both matter because the failure they prevent is silent. Event names are atoms,
// so "MESSAGENEW" and "MessageNew" are the same event; a backend comparing
// against [imap.NotifyEventMessageNew] would have matched only one of them and
// simply never delivered for the other. Likewise an unknown event: the client is
// told SET succeeded and reads the ensuing silence as "nothing changed".
func TestLoopbackNotifyVocabulary(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	backend := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})
	_, clientSide, reader := newRawSessionOn(t, ctx, server)

	writeRawCommand(t, clientSide, "V1 CREATE Watched\r\n")
	collectUntilTag(t, reader, "V1 ")
	writeRawCommand(t, clientSide, "V2 SELECT INBOX\r\n")
	collectUntilTag(t, reader, "V2 ")

	// Upper-case on the wire, canonical inside: the backend only accepts the
	// registration because the framework folded the name before it arrived.
	writeRawCommand(t, clientSide, "V3 NOTIFY SET (MAILBOXES Watched (MESSAGENEW FLAGCHANGE))\r\n")
	if _, tagged := collectUntilTag(t, reader, "V3 "); !strings.HasPrefix(tagged, "V3 OK") {
		t.Fatalf("upper-case event names were not folded to the canonical spelling: %q", tagged)
	}

	// Mixed case is the registered spelling and must work identically.
	writeRawCommand(t, clientSide, "V4 NOTIFY SET (MAILBOXES Watched (MessageNew))\r\n")
	if _, tagged := collectUntilTag(t, reader, "V4 "); !strings.HasPrefix(tagged, "V4 OK") {
		t.Fatalf("canonical event name rejected: %q", tagged)
	}

	// An event the backend does not publish is BADEVENT, not a silent accept.
	writeRawCommand(t, clientSide, "V5 NOTIFY SET (MAILBOXES Watched (AnnotationChange))\r\n")
	_, tagged := collectUntilTag(t, reader, "V5 ")
	if !strings.Contains(tagged, "BADEVENT") {
		t.Errorf("unsupported event = %q, want BADEVENT — a silent accept leaves the client watching for nothing", tagged)
	}

	// An unknown specifier is refused too, but as BAD rather than NO [BADEVENT].
	// RFC 5465 section 6 enumerates the specifiers in the grammar while events
	// are an extensible registry, so an unknown specifier is malformed syntax and
	// an unknown event is a request the server declines. The client acts on the
	// difference: retry a smaller event list, or stop sending that syntax.
	writeRawCommand(t, clientSide, "V6 NOTIFY SET (NOSUCHSPECIFIER (MessageNew))\r\n")
	_, tagged = collectUntilTag(t, reader, "V6 ")
	if !strings.HasPrefix(tagged, "V6 BAD") {
		t.Errorf("unsupported specifier = %q, want BAD", tagged)
	}
	if strings.Contains(tagged, "BADEVENT") {
		t.Errorf("unsupported specifier reported BADEVENT = %q; that code is for the event list", tagged)
	}
}

// newRawSessionOn opens a logged-in raw connection to an existing server.
func newRawSessionOn(t *testing.T, ctx context.Context, server *imapserver.Server) (*imapserver.Server, net.Conn, *bufio.Reader) {
	t.Helper()
	serverSide, clientSide := net.Pipe()
	go func() { _ = server.ServeConn(ctx, serverSide, nil) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	writeRawCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
	collectUntilTag(t, reader, "A1 ")
	t.Cleanup(func() { _ = clientSide.Close() })
	return server, clientSide, reader
}
