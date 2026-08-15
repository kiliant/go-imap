package imapserver_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// Group A is exercised against the real client wherever the client models the
// extension, and at the wire otherwise. The client's own emulation fallbacks
// make a client-only assertion ambiguous — SearchExtended computes MIN, MAX and
// COUNT itself when the server does not advertise ESEARCH — so the capability
// is asserted first and the emulation flag checked, otherwise a server that
// implemented nothing would still pass.

func TestLoopbackESearchReturnOptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, _ := newGroupAClient(t, ctx)
	seedMessages(t, ctx, client, 3)

	if err := client.Capability(ctx, nil); err != nil {
		t.Fatal(err)
	}
	capabilities := client.Capabilities()
	for _, required := range []string{"ESEARCH", "SEARCHRES"} {
		if !capabilities[required] {
			t.Fatalf("server does not advertise %s: %v", required, capabilities)
		}
	}

	data, err := client.SearchExtended(imap.SearchAll, &imapclient.ESearchOptions{
		ReturnOptions: []imapclient.SearchReturnOption{
			imapclient.SearchReturnMin, imapclient.SearchReturnMax, imapclient.SearchReturnCount,
		},
	}).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if data.Emulated {
		t.Fatal("client emulated ESEARCH, so the server response was not exercised")
	}
	if !data.HasMin || data.Min != 1 {
		t.Errorf("MIN = %d (present %v), want 1", data.Min, data.HasMin)
	}
	if !data.HasMax || data.Max != 3 {
		t.Errorf("MAX = %d (present %v), want 3", data.Max, data.HasMax)
	}
	if !data.HasCount || data.Count != 3 {
		t.Errorf("COUNT = %d (present %v), want 3", data.Count, data.HasCount)
	}
}

// An empty RETURN list means ALL, which is what distinguishes it from an absent
// RETURN clause. RFC 4731 section 3.1.
func TestLoopbackESearchEmptyReturnListMeansAll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, _ := newGroupAClient(t, ctx)
	seedMessages(t, ctx, client, 3)

	data, err := client.SearchExtended(imap.SearchAll, &imapclient.ESearchOptions{}).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if data.Emulated {
		t.Fatal("client emulated ESEARCH, so the server response was not exercised")
	}
	if !data.HasAll {
		t.Fatalf("RETURN () did not produce ALL: %+v", data)
	}
	if got := data.All.Dynamic(); got {
		t.Fatalf("ALL is dynamic: %v", data.All)
	}
	var count int
	for _, r := range data.All {
		count += int(r.Stop-r.Start) + 1
	}
	if count != 3 {
		t.Errorf("ALL covers %d messages, want 3", count)
	}
}

// MIN, MAX and ALL describe messages and are omitted on an empty result; COUNT
// describes the result itself and must still be reported as zero.
func TestLoopbackESearchEmptyResultStillReportsCount(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "A3 SEARCH RETURN (MIN MAX COUNT) SUBJECT \"nomatch\"\r\n")
	untagged, tagged := collectUntilTag(t, reader, "A3 ")
	if !strings.HasPrefix(tagged, "A3 OK") {
		t.Fatalf("SEARCH failed: %q", tagged)
	}
	line := findResponse(t, untagged, "* ESEARCH")
	if strings.Contains(line, " MIN ") || strings.Contains(line, " MAX ") || strings.Contains(line, " ALL ") {
		t.Errorf("empty result reported message data: %q", line)
	}
	if !strings.Contains(line, "COUNT 0") {
		t.Errorf("empty result did not report COUNT 0: %q", line)
	}
}

// UID SEARCH must mark its ESEARCH response with the UID token so the client
// knows which number space it is reading. RFC 4731 section 3.2.
func TestLoopbackESearchUIDMarker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "A3 UID SEARCH RETURN (COUNT) ALL\r\n")
	untagged, tagged := collectUntilTag(t, reader, "A3 ")
	if !strings.HasPrefix(tagged, "A3 OK") {
		t.Fatalf("UID SEARCH failed: %q", tagged)
	}
	line := findResponse(t, untagged, "* ESEARCH")
	if !strings.Contains(line, ") UID ") {
		t.Errorf("UID SEARCH response is not marked UID: %q", line)
	}

	writeRawCommand(t, clientSide, "A4 SEARCH RETURN (COUNT) ALL\r\n")
	untagged, _ = collectUntilTag(t, reader, "A4 ")
	if line := findResponse(t, untagged, "* ESEARCH"); strings.Contains(line, ") UID ") {
		t.Errorf("sequence-number SEARCH response is marked UID: %q", line)
	}
}

// RFC 5182: "$" refers to the last saved result, and resolves to the empty set
// before anything has been saved rather than failing.
func TestLoopbackSearchResSaveAndReference(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "A3 FETCH $ (UID)\r\n")
	untagged, tagged := collectUntilTag(t, reader, "A3 ")
	if !strings.HasPrefix(tagged, "A3 OK") {
		t.Fatalf("unsaved $ should succeed as an empty set, got %q", tagged)
	}
	for _, line := range untagged {
		if strings.Contains(line, "FETCH") {
			t.Errorf("unsaved $ matched a message: %q", line)
		}
	}

	writeRawCommand(t, clientSide, "A4 SEARCH RETURN (SAVE) 2:3\r\n")
	if _, tagged := collectUntilTag(t, reader, "A4 "); !strings.HasPrefix(tagged, "A4 OK") {
		t.Fatalf("SEARCH RETURN (SAVE) failed: %q", tagged)
	}
	writeRawCommand(t, clientSide, "A5 FETCH $ (UID)\r\n")
	untagged, tagged = collectUntilTag(t, reader, "A5 ")
	if !strings.HasPrefix(tagged, "A5 OK") {
		t.Fatalf("FETCH $ failed: %q", tagged)
	}
	var fetched int
	for _, line := range untagged {
		if strings.Contains(line, "FETCH") {
			fetched++
		}
	}
	if fetched != 2 {
		t.Errorf("FETCH $ returned %d messages, want the 2 saved", fetched)
	}
}

// RFC 5182 section 2.1: SAVE accompanied only by MIN and/or MAX saves those one
// or two messages, not the whole result.
func TestLoopbackSearchResSaveWithMinMaxSavesOnlyThose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "A3 SEARCH RETURN (MIN MAX SAVE) ALL\r\n")
	if _, tagged := collectUntilTag(t, reader, "A3 "); !strings.HasPrefix(tagged, "A3 OK") {
		t.Fatalf("SEARCH failed: %q", tagged)
	}
	writeRawCommand(t, clientSide, "A4 FETCH $ (UID)\r\n")
	untagged, _ := collectUntilTag(t, reader, "A4 ")
	var fetched int
	for _, line := range untagged {
		if strings.Contains(line, "FETCH") {
			fetched++
		}
	}
	if fetched != 2 {
		t.Errorf("saved set holds %d messages, want only MIN and MAX of 3", fetched)
	}
}

// CHILDREN and SPECIAL-USE attributes are opt-in: a client that did not ask
// must not receive them, or it cannot distinguish "not subscribed" from "not
// reported". RFC 5258 section 3, RFC 6154 section 5.2.
func TestLoopbackListReturnOptionsAreOptIn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	for i, mailbox := range []string{"Parent", "Parent/Child"} {
		tag := "A3" + string(rune('a'+i))
		writeRawCommand(t, clientSide, tag+" CREATE "+mailbox+"\r\n")
		if _, tagged := collectUntilTag(t, reader, tag+" "); !strings.HasPrefix(tagged, tag+" OK") {
			t.Fatalf("CREATE %s failed: %q", mailbox, tagged)
		}
	}

	writeRawCommand(t, clientSide, "A4 LIST \"\" \"*\"\r\n")
	untagged, _ := collectUntilTag(t, reader, "A4 ")
	for _, line := range untagged {
		if strings.Contains(line, "HasChildren") || strings.Contains(line, "HasNoChildren") {
			t.Errorf("plain LIST reported child attributes: %q", line)
		}
	}

	writeRawCommand(t, clientSide, "A5 LIST \"\" \"*\" RETURN (CHILDREN)\r\n")
	untagged, tagged := collectUntilTag(t, reader, "A5 ")
	if !strings.HasPrefix(tagged, "A5 OK") {
		t.Fatalf("LIST RETURN (CHILDREN) failed: %q", tagged)
	}
	var sawParent bool
	for _, line := range untagged {
		if strings.HasSuffix(strings.TrimSpace(line), "Parent") {
			sawParent = true
			if !strings.Contains(line, "HasChildren") {
				t.Errorf("Parent has a child but was not marked: %q", line)
			}
		}
	}
	if !sawParent {
		t.Errorf("LIST did not return Parent: %v", untagged)
	}
}

// RFC 5819: LIST-STATUS delivers an untagged STATUS response per mailbox, not
// data on the LIST line.
func TestLoopbackListStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "A3 LIST \"\" \"INBOX\" RETURN (STATUS (MESSAGES))\r\n")
	untagged, tagged := collectUntilTag(t, reader, "A3 ")
	if !strings.HasPrefix(tagged, "A3 OK") {
		t.Fatalf("LIST RETURN (STATUS) failed: %q", tagged)
	}
	status := findResponse(t, untagged, "* STATUS")
	if !strings.Contains(status, "MESSAGES 3") {
		t.Errorf("STATUS response = %q, want MESSAGES 3", status)
	}
}

// RFC 6154 section 3: CREATE's USE parameter, and the attribute surfacing on a
// later LIST that asks for it.
func TestLoopbackCreateSpecialUse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, clientSide, reader := newGroupARawSession(t, ctx)

	writeRawCommand(t, clientSide, "A3 CREATE Archived (USE (\\Archive))\r\n")
	if _, tagged := collectUntilTag(t, reader, "A3 "); !strings.HasPrefix(tagged, "A3 OK") {
		t.Fatalf("CREATE with USE failed: %q", tagged)
	}
	writeRawCommand(t, clientSide, "A4 LIST \"\" \"*\" RETURN (SPECIAL-USE)\r\n")
	untagged, _ := collectUntilTag(t, reader, "A4 ")
	var marked bool
	for _, line := range untagged {
		if strings.Contains(line, "Archived") && strings.Contains(line, `\Archive`) {
			marked = true
		}
	}
	if !marked {
		t.Errorf("created mailbox is not marked \\Archive: %v", untagged)
	}

	// A second mailbox claiming the same use is refused with USEATTR rather
	// than silently created without the attribute.
	writeRawCommand(t, clientSide, "A5 CREATE Archived2 (USE (\\Archive))\r\n")
	if _, tagged := collectUntilTag(t, reader, "A5 "); !strings.Contains(tagged, "USEATTR") {
		t.Errorf("duplicate use attribute was not refused with USEATTR: %q", tagged)
	}

	// An unknown use attribute is refused before the backend is asked.
	writeRawCommand(t, clientSide, "A6 CREATE Weird (USE (\\NotAUseAttribute))\r\n")
	if _, tagged := collectUntilTag(t, reader, "A6 "); !strings.Contains(tagged, "USEATTR") {
		t.Errorf("unknown use attribute was not refused with USEATTR: %q", tagged)
	}
}

// A capability whose behaviour the backend must implement is advertised only
// when the backend witnesses it. A backend with no CapabilitySupport must not
// see CHILDREN, SPECIAL-USE, CREATE-SPECIAL-USE or WITHIN advertised, while the
// framework-only ESEARCH and SEARCHRES stay available.
func TestGroupACapabilitiesRequireBackendWitness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	server := newUnwitnessedServer(t)
	client, _ := openLoopbackClient(t, ctx, server, &imapclient.Options{AllowInsecureAuth: true})
	if err := client.Login(ctx, "alice", "secret", nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Capability(ctx, nil); err != nil {
		t.Fatal(err)
	}
	capabilities := client.Capabilities()
	for _, witnessed := range []string{"CHILDREN", "SPECIAL-USE", "CREATE-SPECIAL-USE", "WITHIN"} {
		if capabilities[witnessed] {
			t.Errorf("%s advertised without a backend witness", witnessed)
		}
	}
	for _, frameworkOnly := range []string{"ESEARCH", "SEARCHRES"} {
		if !capabilities[frameworkOnly] {
			t.Errorf("%s is framework-only but was not advertised", frameworkOnly)
		}
	}
}

// unwitnessedBackend models a backend written before these extensions existed.
// It delegates by an explicit field rather than embedding: an embedded
// *memory.Backend would promote SupportsCapability and the type would still be
// witnessed, so the test would pass without testing anything.
type unwitnessedBackend struct{ backend imapserver.Backend }

func (b *unwitnessedBackend) Authenticate(ctx context.Context, conn *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	return b.backend.Authenticate(ctx, conn, credentials, options)
}

// newUnwitnessedServer serves a backend that witnesses no optional capability.
func newUnwitnessedServer(t *testing.T) *imapserver.Server {
	t.Helper()
	return imapserver.New(&unwitnessedBackend{backend: memory.New(&memory.Options{
		Users: map[string]string{"alice": "secret"},
	})}, &imapserver.Options{AllowInsecureAuth: true})
}

func newGroupAClient(t *testing.T, ctx context.Context) (*imapclient.Client, <-chan error) {
	t.Helper()
	backend := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})
	client, done := openLoopbackClient(t, ctx, server, &imapclient.Options{AllowInsecureAuth: true})
	if err := client.Login(ctx, "alice", "secret", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	return client, done
}

func seedMessages(t *testing.T, ctx context.Context, client *imapclient.Client, count int) {
	t.Helper()
	for i := range count {
		body := "Subject: seeded " + string(rune('a'+i)) + "\r\n\r\nbody\r\n"
		if _, err := client.Append(ctx, "INBOX", nil, int64(len(body)), strings.NewReader(body)).Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

// newGroupARawSession returns a logged-in, INBOX-selected raw connection with
// three seeded messages. The seeding runs over a separate client connection to
// the same backend, so the raw connection's response stream holds only what the
// test asks for.
func newGroupARawSession(t *testing.T, ctx context.Context) (*imapserver.Server, net.Conn, *bufio.Reader) {
	return newGroupARawSessionIn(t, ctx, false)
}

// newGroupARawSessionIn optionally stops in the authenticated state. ENABLE is
// valid only before a mailbox is selected (RFC 5161), so a test that enables an
// extension has to get there first.
func newGroupARawSessionIn(t *testing.T, ctx context.Context, authenticatedOnly bool) (*imapserver.Server, net.Conn, *bufio.Reader) {
	t.Helper()
	backend := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})

	setup, _ := openLoopbackClient(t, ctx, server, &imapclient.Options{AllowInsecureAuth: true})
	if err := setup.Login(ctx, "alice", "secret", nil); err != nil {
		t.Fatal(err)
	}
	seedMessages(t, ctx, setup, 3)
	if err := setup.Logout(ctx, nil); err != nil {
		t.Fatal(err)
	}

	serverSide, clientSide := net.Pipe()
	go func() { _ = server.ServeConn(ctx, serverSide, nil) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	writeRawCommand(t, clientSide, "A1 LOGIN alice secret\r\n")
	collectUntilTag(t, reader, "A1 ")
	if !authenticatedOnly {
		writeRawCommand(t, clientSide, "A2 SELECT INBOX\r\n")
		collectUntilTag(t, reader, "A2 ")
	}
	t.Cleanup(func() { _ = clientSide.Close() })
	return server, clientSide, reader
}

// collectUntilTag reads to the tagged response for prefix, returning the
// untagged lines seen on the way. Unlike readUntilTag it keeps them, which is
// what these tests assert on.
func collectUntilTag(t *testing.T, reader *bufio.Reader, prefix string) ([]string, string) {
	t.Helper()
	var untagged []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, prefix) {
			return untagged, line
		}
		untagged = append(untagged, strings.TrimRight(line, "\r\n"))
	}
}

func findResponse(t *testing.T, lines []string, prefix string) string {
	t.Helper()
	for _, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no %q response in %v", prefix, lines)
	return ""
}
