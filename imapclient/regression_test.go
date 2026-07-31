package imapclient

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// scriptedServer answers each client line with the next canned reply, so that a
// test can describe a whole exchange without a goroutine of its own. A reply of
// "" sends nothing and waits for the next line.
func scriptedServer(t *testing.T, replies ...string) (*Client, chan<- string) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	extra := make(chan string, 4)
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY IMAP4rev1] ready\r\n"))
		r := bufio.NewReader(serverConn)
		for _, reply := range replies {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			tag := "*"
			if fields := strings.Fields(line); len(fields) > 0 {
				tag = fields[0]
			}
			// A command line ending in a synchronising literal announcement is
			// only half the command: request the payload and read it before
			// answering.
			for {
				size, ok := announcedLiteralSize(line)
				if !ok {
					break
				}
				if _, err := serverConn.Write([]byte("+ send it\r\n")); err != nil {
					return
				}
				if _, err := io.ReadFull(r, make([]byte, size)); err != nil {
					return
				}
				if line, err = r.ReadString('\n'); err != nil {
					return
				}
			}
			if reply != "" {
				_, _ = serverConn.Write([]byte(strings.ReplaceAll(reply, "$TAG", tag)))
			}
			select {
			case more := <-extra:
				_, _ = serverConn.Write([]byte(strings.ReplaceAll(more, "$TAG", tag)))
			default:
			}
		}
		for {
			if _, err := r.ReadString('\n'); err != nil {
				return
			}
		}
	}()
	c := NewClient(clientConn, nil)
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	return c, extra
}

// announcedLiteralSize reports the payload size of a synchronising literal
// announcement ending line, if it has one. LITERAL+ announcements ("{n+}") need
// no continuation and are deliberately not matched.
func announcedLiteralSize(line string) (int, bool) {
	line = strings.TrimRight(line, "\r\n")
	open := strings.LastIndexByte(line, '{')
	if open < 0 || !strings.HasSuffix(line, "}") {
		return 0, false
	}
	size, err := strconv.Atoi(line[open+1 : len(line)-1])
	if err != nil || size < 0 {
		return 0, false
	}
	return size, true
}

func setState(c *Client, state State) {
	c.mu.Lock()
	c.state = state
	c.mu.Unlock()
}

func selectedMailbox(c *Client) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectedMailbox
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A caller that stops reading a FETCH must not take the process down with it.
// The reader goroutine used to send on a channel another goroutine had closed.
func TestFetchAbandonedBeforeCloseDoesNotPanic(t *testing.T) {
	c, _ := scriptedServer(t,
		"* 1 FETCH (UID 11)\r\n* 2 FETCH (UID 12)\r\n* 3 FETCH (UID 13)\r\n$TAG OK done\r\n",
	)
	setState(c, StateSelected)

	set, err := imap.ParseSeqSet("1:3")
	if err != nil {
		t.Fatal(err)
	}
	cmd := c.Fetch(set, imap.FetchItemUID)
	// Deliberately never call Next: the reader blocks on an undelivered
	// response, and Close must unwind it.
	time.Sleep(100 * time.Millisecond)
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Close() = %v", err)
	}
	if _, err := cmd.Next(testContext(t)); err == nil {
		t.Fatal("Next() after Close = nil, want an error")
	}
	time.Sleep(100 * time.Millisecond)
}

// A server may answer a command carrying a synchronising literal with a tagged
// rejection instead of a continuation request. The client used to hold the
// connection mutex across that wait, so the reader could never deliver the
// rejection and the session deadlocked.
func TestLiteralRejectedInsteadOfContinuation(t *testing.T) {
	c, _ := scriptedServer(t,
		"$TAG NO [CANNOT] charset not supported\r\n",
		"$TAG OK NOOP done\r\n",
	)
	setState(c, StateSelected)

	criteria := imap.SearchAnd{imap.SearchString{Key: imap.SearchKeySubject, Value: "hej \u00e4\u00f6"}}
	done := make(chan error, 1)
	go func() { done <- c.Search(criteria, &SearchOptions{Charset: "UTF-8"}).Wait(testContext(t)) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("SEARCH = nil, want the server's rejection")
		}
		var imapErr *imap.Error
		if !errors.As(err, &imapErr) || imapErr.Code != "CANNOT" {
			t.Fatalf("SEARCH = %v, want an *imap.Error carrying CANNOT", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SEARCH with a rejected literal deadlocked")
	}
	// The rejection leaves the stream synchronised, so the session survives.
	if err := c.Noop().Wait(testContext(t)); err != nil {
		t.Fatalf("NOOP after a rejected literal = %v", err)
	}
}

// A password outside ASCII forces a synchronising literal. LOGIN installed no
// continuation handler, so the encoder refused to write one and the command
// failed with a misleading protocol error.
func TestLoginNonASCIIPasswordUsesLiteral(t *testing.T) {
	c, _ := scriptedServer(t,
		"$TAG OK [CAPABILITY IMAP4rev1] logged in\r\n",
		"* CAPABILITY IMAP4rev1\r\n$TAG OK done\r\n",
	)
	c.opts.AllowInsecureAuth = true

	if err := c.Login(testContext(t), "user", "p\u00e4ssw\u00f6rd"); err != nil {
		t.Fatalf("Login with a non-ASCII password = %v", err)
	}
	if got := c.State(); got != StateAuthenticated {
		t.Fatalf("state after LOGIN = %v, want authenticated", got)
	}
}

// A failed SELECT leaves no mailbox selected. Staying in the selected state
// would let the next FETCH silently address the previous mailbox.
func TestFailedSelectLeavesAuthenticatedState(t *testing.T) {
	c, _ := scriptedServer(t,
		"* 3 EXISTS\r\n$TAG OK [READ-WRITE] selected\r\n",
		"$TAG NO no such mailbox\r\n",
	)
	setState(c, StateAuthenticated)

	if _, err := c.Select("INBOX", nil).Wait(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("Missing", nil).Wait(testContext(t)); err == nil {
		t.Fatal("SELECT of a missing mailbox = nil, want an error")
	}
	if got := c.State(); got != StateAuthenticated {
		t.Fatalf("state after a failed SELECT = %v, want authenticated", got)
	}
	if got := selectedMailbox(c); got != "" {
		t.Fatalf("selected mailbox after a failed SELECT = %q, want none", got)
	}
}

// An untagged BYE outside the greeting ends the session. Commands issued after
// it used to wait for a tagged completion the server would never send.
func TestMidSessionByeEndsTheSession(t *testing.T) {
	c, _ := scriptedServer(t, "* BYE server shutting down\r\n")
	setState(c, StateAuthenticated)

	if err := c.Noop().Wait(testContext(t)); err == nil {
		t.Fatal("NOOP answered with BYE = nil, want an error")
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.State() != StateLogout {
		if time.Now().After(deadline) {
			t.Fatalf("state after BYE = %v, want logout", c.State())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.Noop().Wait(testContext(t)); err == nil {
		t.Fatal("NOOP after BYE = nil, want an error")
	}
}

// A FETCH item from an extension this client does not model must reach the
// caller verbatim. It used to be skipped and replaced with an empty reader,
// which is data loss the documented FetchDataRaw contract rules out.
func TestFetchUnknownItemIsPreservedVerbatim(t *testing.T) {
	c, _ := scriptedServer(t,
		"* 1 FETCH (UID 7 X-FUTURE (\"a\" {3}\r\nb\\c NIL 42))\r\n$TAG OK done\r\n",
	)
	setState(c, StateSelected)

	cmd := c.Fetch(imap.SeqSetNum(1), imap.FetchItemUID)
	data, err := cmd.Next(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	items := data.Items[imap.FetchDataKey("X-FUTURE")]
	if len(items) != 1 {
		t.Fatalf("unknown items = %#v", data.Items)
	}
	raw, ok := items[0].(*imap.FetchDataRaw)
	if !ok {
		t.Fatalf("unknown item = %#v", items[0])
	}
	got, err := io.ReadAll(raw.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if want := "(\"a\" {3}\r\nb\\c NIL 42)"; string(got) != want {
		t.Fatalf("raw value = %q, want %q", got, want)
	}
	if err := cmd.Wait(testContext(t)); err != nil {
		t.Fatal(err)
	}
}

func encodeSearch(t *testing.T, criteria imap.SearchCriteria) string {
	t.Helper()
	var sb strings.Builder
	enc := imapwire.NewEncoder(&sb, nil)
	writeSearchCriteria(enc, criteria)
	if err := enc.Flush(); err != nil {
		t.Fatalf("encoding %#v: %v", criteria, err)
	}
	return sb.String()
}

// Without parentheses, the keys of a nested conjunction bind to the enclosing
// OR or NOT rather than to each other, so the server answers a different
// question than the caller asked.
func TestSearchCriteriaGrouping(t *testing.T) {
	seen := imap.SearchKeyword("SEEN")
	flagged := imap.SearchKeyword("FLAGGED")
	draft := imap.SearchKeyword("DRAFT")

	for _, tc := range []struct {
		name     string
		criteria imap.SearchCriteria
		want     string
	}{
		{"empty conjunction", imap.SearchAnd{}, "ALL"},
		{"top-level conjunction is bare", imap.SearchAnd{seen, flagged}, "SEEN FLAGGED"},
		{"single element needs no parentheses", imap.SearchAnd{seen}, "SEEN"},
		{
			"conjunction under OR",
			imap.SearchOr{Left: imap.SearchAnd{seen, flagged}, Right: draft},
			"OR (SEEN FLAGGED) DRAFT",
		},
		{
			"conjunction under NOT",
			imap.SearchAnd{imap.SearchNot{Criteria: imap.SearchAnd{seen, flagged}}},
			"NOT (SEEN FLAGGED)",
		},
		{
			"nested OR",
			imap.SearchOr{
				Left:  imap.SearchOr{Left: seen, Right: flagged},
				Right: draft,
			},
			"OR OR SEEN FLAGGED DRAFT",
		},
		{
			"empty conjunction under NOT",
			imap.SearchAnd{imap.SearchNot{Criteria: imap.SearchAnd{}}},
			"NOT ALL",
		},
		{
			"MODSEQ entry precedes the value",
			imap.SearchAnd{imap.SearchModSeq{EntryName: "/flags/\\draft", EntryType: imap.SearchModSeqMetadataAll, ModSeq: 620162338}},
			`MODSEQ "/flags/\\draft" all 620162338`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := encodeSearch(t, tc.criteria); got != tc.want {
				t.Fatalf("encoded = %q, want %q", got, tc.want)
			}
		})
	}
}

// Criteria the encoder cannot render are rejected before a tag is allocated,
// rather than being silently dropped from the command line.
func TestSearchRejectsUnrenderableCriteria(t *testing.T) {
	c, _ := scriptedServer(t)
	setState(c, StateSelected)
	for _, tc := range []struct {
		name     string
		criteria imap.SearchCriteria
		want     string
	}{
		{"nil OR operand", imap.SearchOr{Left: imap.SearchKeyword("SEEN")}, "requires both operands"},
		{"nil NOT operand", imap.SearchNot{}, "requires an operand"},
		{
			"MODSEQ entry name without a type",
			imap.SearchModSeq{EntryName: "/flags/\\draft", ModSeq: 1},
			"entry name and entry type together",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := c.Search(tc.criteria, nil).Wait(testContext(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SEARCH = %v, want a rejection mentioning %q", err, tc.want)
			}
		})
	}
}

func TestFetchEnvelope(t *testing.T) {
	c, _ := scriptedServer(t,
		"* 1 FETCH (ENVELOPE (\"Wed, 17 Jul 1996 02:23:25 -0700 (PDT)\" "+
			"\"=?UTF-8?q?Caf=C3=A9?= news\" "+
			"((\"Terry Gray\" NIL \"gray\" \"cac.washington.edu\")) "+
			"((\"Terry Gray\" NIL \"gray\" \"cac.washington.edu\")) "+
			"((\"Terry Gray\" NIL \"gray\" \"cac.washington.edu\")) "+
			"((NIL NIL \"imap\" \"cac.washington.edu\")(NIL NIL \"minutes\" \"CNRI.Reston.VA.US\")) "+
			"NIL NIL \"<B27397-0100000@cac.washington.edu>\" "+
			"\"<A17395-0100000@cac.washington.edu>\"))\r\n"+
			"$TAG OK done\r\n",
	)
	setState(c, StateSelected)

	cmd := c.Fetch(imap.SeqSetNum(1), imap.FetchItemEnvelope)
	data, err := cmd.Next(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	items := data.Items[imap.FetchDataKey("ENVELOPE")]
	if len(items) != 1 {
		t.Fatalf("envelope items = %#v", items)
	}
	env, ok := items[0].(*imap.FetchDataEnvelope)
	if !ok || env.Envelope == nil {
		t.Fatalf("envelope = %#v", items[0])
	}
	if got := env.Envelope.Subject; got != "Café news" {
		t.Fatalf("subject = %q, want the decoded encoded-word", got)
	}
	if got := env.Envelope.Date.UTC().Format(time.RFC3339); got != "1996-07-17T09:23:25Z" {
		t.Fatalf("date = %q", got)
	}
	if len(env.Envelope.From) != 1 || env.Envelope.From[0].Addr() != "gray@cac.washington.edu" {
		t.Fatalf("from = %#v", env.Envelope.From)
	}
	if len(env.Envelope.To) != 2 || env.Envelope.To[1].Addr() != "minutes@CNRI.Reston.VA.US" {
		t.Fatalf("to = %#v", env.Envelope.To)
	}
	if env.Envelope.Cc != nil || env.Envelope.Bcc != nil {
		t.Fatalf("NIL address lists decoded as %#v / %#v", env.Envelope.Cc, env.Envelope.Bcc)
	}
	if got := env.Envelope.MessageID; got != "<A17395-0100000@cac.washington.edu>" {
		t.Fatalf("message id = %q", got)
	}
	if err := cmd.Wait(testContext(t)); err != nil {
		t.Fatal(err)
	}
}

func TestFetchBodyStructure(t *testing.T) {
	c, _ := scriptedServer(t,
		"* 1 FETCH (BODYSTRUCTURE ("+
			"(\"TEXT\" \"PLAIN\" (\"CHARSET\" \"UTF-8\") NIL NIL \"7BIT\" 1152 23)"+
			"(\"TEXT\" \"HTML\" (\"CHARSET\" \"UTF-8\") NIL NIL \"QUOTED-PRINTABLE\" 2globalMarker 0)"+
			" \"ALTERNATIVE\" (\"BOUNDARY\" \"b1\") (\"inline\" NIL) NIL NIL))\r\n"+
			"$TAG OK done\r\n",
	)
	setState(c, StateSelected)
	// The second part carries a deliberately malformed size; the command must
	// fail rather than silently mis-parse.
	cmd := c.Fetch(imap.SeqSetNum(1), &imap.FetchItemBodyStructure{Extended: true})
	if _, err := cmd.Next(testContext(t)); err == nil {
		t.Fatal("malformed BODYSTRUCTURE = nil, want an error")
	}
}

func TestFetchBodyStructureMultipart(t *testing.T) {
	c, _ := scriptedServer(t,
		"* 1 FETCH (BODYSTRUCTURE ("+
			"(\"TEXT\" \"PLAIN\" (\"CHARSET\" \"UTF-8\") NIL NIL \"7BIT\" 1152 23)"+
			"(\"IMAGE\" \"PNG\" (\"NAME\" \"a.png\") \"<cid1>\" NIL \"BASE64\" 4096 NIL (\"attachment\" (\"FILENAME\" \"a.png\")) NIL NIL)"+
			" \"MIXED\" (\"BOUNDARY\" \"b1\") NIL NIL NIL))\r\n"+
			"$TAG OK done\r\n",
	)
	setState(c, StateSelected)

	cmd := c.Fetch(imap.SeqSetNum(1), &imap.FetchItemBodyStructure{Extended: true})
	data, err := cmd.Next(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	items := data.Items[imap.FetchDataKey("BODYSTRUCTURE")]
	if len(items) != 1 {
		t.Fatalf("body structure items = %#v", items)
	}
	bs, ok := items[0].(*imap.FetchDataBodyStructure)
	if !ok {
		t.Fatalf("body structure = %#v", items[0])
	}
	mp, ok := bs.BodyStructure.(*imap.BodyStructureMultiPart)
	if !ok {
		t.Fatalf("root = %#v, want a multipart", bs.BodyStructure)
	}
	if mp.MediaType() != "multipart/mixed" || len(mp.Children) != 2 {
		t.Fatalf("root = %q with %d children", mp.MediaType(), len(mp.Children))
	}
	if mp.Extended == nil || mp.Extended.Params["boundary"] != "b1" {
		t.Fatalf("multipart extension = %#v", mp.Extended)
	}
	text, ok := mp.Children[0].(*imap.BodyStructureSinglePart)
	if !ok || text.MediaType() != "text/plain" || text.Size != 1152 {
		t.Fatalf("first child = %#v", mp.Children[0])
	}
	if text.Text == nil || text.Text.NumLines != 23 {
		t.Fatalf("text part lines = %#v", text.Text)
	}
	if text.Params["charset"] != "UTF-8" {
		t.Fatalf("text params = %#v", text.Params)
	}
	image, ok := mp.Children[1].(*imap.BodyStructureSinglePart)
	if !ok || image.MediaType() != "image/png" {
		t.Fatalf("second child = %#v", mp.Children[1])
	}
	if got := image.Filename(); got != "a.png" {
		t.Fatalf("filename = %q", got)
	}
	if d := image.Disposition(); d == nil || d.Value != "attachment" {
		t.Fatalf("disposition = %#v", image.Disposition())
	}
	if err := cmd.Wait(testContext(t)); err != nil {
		t.Fatal(err)
	}
}

// The non-extended BODY item shares the BODY keyword with a body section. Only
// the octet that follows tells them apart.
func TestFetchNonExtendedBodyStructure(t *testing.T) {
	c, _ := scriptedServer(t,
		"* 1 FETCH (BODY (\"TEXT\" \"PLAIN\" (\"CHARSET\" \"US-ASCII\") NIL NIL \"7BIT\" 2279 48))\r\n"+
			"$TAG OK done\r\n",
	)
	setState(c, StateSelected)

	cmd := c.Fetch(imap.SeqSetNum(1), &imap.FetchItemBodyStructure{})
	data, err := cmd.Next(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	items := data.Items[imap.FetchDataKey("BODY")]
	if len(items) != 1 {
		t.Fatalf("body items = %#v", data.Items)
	}
	bs, ok := items[0].(*imap.FetchDataBodyStructure)
	if !ok {
		t.Fatalf("body = %#v", items[0])
	}
	sp, ok := bs.BodyStructure.(*imap.BodyStructureSinglePart)
	if !ok || sp.MediaType() != "text/plain" {
		t.Fatalf("body structure = %#v", bs.BodyStructure)
	}
	if sp.Extended != nil {
		t.Fatalf("non-extended BODY carried extension fields: %#v", sp.Extended)
	}
	if err := cmd.Wait(testContext(t)); err != nil {
		t.Fatal(err)
	}
}

// A message/rfc822 part nests a whole envelope and body structure inside the
// enclosing one.
func TestFetchBodyStructureMessageRFC822(t *testing.T) {
	c, _ := scriptedServer(t,
		"* 1 FETCH (BODYSTRUCTURE (\"MESSAGE\" \"RFC822\" NIL NIL NIL \"7BIT\" 4096 "+
			"(\"Wed, 17 Jul 1996 02:23:25 -0700\" \"inner\" ((NIL NIL \"a\" \"b\")) NIL NIL NIL NIL NIL NIL \"<i@b>\") "+
			"(\"TEXT\" \"PLAIN\" NIL NIL NIL \"7BIT\" 100 5) 60))\r\n"+
			"$TAG OK done\r\n",
	)
	setState(c, StateSelected)

	cmd := c.Fetch(imap.SeqSetNum(1), &imap.FetchItemBodyStructure{Extended: true})
	data, err := cmd.Next(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	bs := data.Items[imap.FetchDataKey("BODYSTRUCTURE")][0].(*imap.FetchDataBodyStructure)
	sp, ok := bs.BodyStructure.(*imap.BodyStructureSinglePart)
	if !ok || sp.Message == nil {
		t.Fatalf("body structure = %#v", bs.BodyStructure)
	}
	if sp.Message.Envelope == nil || sp.Message.Envelope.Subject != "inner" {
		t.Fatalf("nested envelope = %#v", sp.Message.Envelope)
	}
	if sp.Message.NumLines != 60 {
		t.Fatalf("nested line count = %d", sp.Message.NumLines)
	}
	inner, ok := sp.Message.BodyStructure.(*imap.BodyStructureSinglePart)
	if !ok || inner.MediaType() != "text/plain" {
		t.Fatalf("nested body structure = %#v", sp.Message.BodyStructure)
	}
	if err := cmd.Wait(testContext(t)); err != nil {
		t.Fatal(err)
	}
}
