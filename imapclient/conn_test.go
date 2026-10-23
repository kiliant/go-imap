package imapclient

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestGreetingCapabilitiesAndPipelining(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY IMAP4rev1 STARTTLS AUTH=PLAIN] ready\r\n"))
		r := bufio.NewReader(serverConn)
		first, _ := r.ReadString('\n')
		second, _ := r.ReadString('\n')
		firstTag := strings.Fields(first)[0]
		secondTag := strings.Fields(second)[0]
		_, _ = serverConn.Write([]byte(secondTag + " OK done\r\n"))
		_, _ = serverConn.Write([]byte(firstTag + " OK done\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatalf("WaitGreeting() = %v", err)
	}
	for _, capability := range []string{"IMAP4REV1", "STARTTLS", "AUTH=PLAIN"} {
		if !c.Capabilities()[capability] {
			t.Errorf("capability %q missing", capability)
		}
	}
	first, second := c.Noop(nil), c.Noop(nil)
	if err := first.Wait(ctx); err != nil {
		t.Errorf("first Wait() = %v", err)
	}
	if err := second.Wait(ctx); err != nil {
		t.Errorf("second Wait() = %v", err)
	}
}

func TestGreetingPreauthAndBye(t *testing.T) {
	t.Run("preauth", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		go func() { _, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n")) }()
		c := NewClient(clientConn, nil)
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.WaitGreeting(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if got := c.State(); got != StateAuthenticated {
			t.Fatalf("State() = %q", got)
		}
	})
	t.Run("bye", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		go func() { _, _ = serverConn.Write([]byte("* BYE maintenance\r\n")) }()
		c := NewClient(clientConn, nil)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := c.WaitGreeting(ctx, nil)
		var ierr *imap.Error
		if !errors.As(err, &ierr) || ierr.Type != imap.ErrorTypeBye {
			t.Fatalf("WaitGreeting() = %T %[1]v", err)
		}
	})
}

func TestWaitCancellationPoisonsConnection(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		_, _ = serverConn.Write([]byte("* OK ready\r\n"))
		_, _ = bufio.NewReader(serverConn).ReadString('\n')
	}()
	c := NewClient(clientConn, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	cmd := c.Noop(nil)
	// Let the writer finish before cancelling; the server deliberately never
	// completes the command.
	time.Sleep(10 * time.Millisecond)
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if err := cmd.Wait(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() = %v", err)
	}
	if got := c.State(); got != StateLogout {
		t.Fatalf("State() = %q", got)
	}
}

func TestLocalStateRejectionAndRedactedTrace(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		_, _ = serverConn.Write([]byte("* OK ready\r\n"))
		_, _ = bufio.NewReader(serverConn).ReadString('\n')
	}()
	var trace []string
	c := NewClient(clientConn, &Options{Trace: func(event TraceEvent) { trace = append(trace, event.Data) }})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	bad := c.beginCommand("SELECT", stateSelected, nil, nil)
	err := bad.Wait(ctx)
	var ierr *imap.Error
	if !errors.As(err, &ierr) || ierr.Type != imap.ErrorTypeProtocol {
		t.Fatalf("local rejection = %v", err)
	}
	cmd := c.beginCommand("LOGIN", stateNotAuthenticated, func(enc *imapwire.Encoder) {
		enc.SP().String("alice").SP().String("top-secret")
	}, nil)
	if cmd.tag == "" {
		t.Fatal("LOGIN was not issued")
	}
	for _, event := range trace {
		if strings.Contains(event, "alice") || strings.Contains(event, "top-secret") {
			t.Fatalf("trace leaked credentials: %q", event)
		}
	}
}

func TestUnilateralVanished(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	updates := make(chan VanishedData, 2)
	go func() {
		_, _ = serverConn.Write([]byte("* OK ready\r\n"))
		_, _ = serverConn.Write([]byte("* VANISHED 41,43\r\n"))
		_, _ = serverConn.Write([]byte("* VANISHED (EARLIER) 44:46\r\n"))
	}()
	c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
		Vanished: func(data VanishedData) { updates <- data },
	}})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-updates:
		if data.Earlier || !data.UIDs.Equal(imap.UIDSet{{Start: 41, Stop: 41}, {Start: 43, Stop: 43}}) {
			t.Fatalf("first VANISHED = %#v", data)
		}
	case <-ctx.Done():
		t.Fatal("did not receive VANISHED")
	}
	select {
	case data := <-updates:
		if !data.Earlier || !data.UIDs.Equal(imap.UIDSetRange(44, 46)) {
			t.Fatalf("EARLIER VANISHED = %#v", data)
		}
	case <-ctx.Done():
		t.Fatal("did not receive VANISHED (EARLIER)")
	}
}

func TestUnilateralFetchFlags(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	updates := make(chan *imap.FetchMessageData, 1)
	go func() {
		_, _ = serverConn.Write([]byte("* OK ready\r\n"))
		_, _ = serverConn.Write([]byte("* 9 FETCH (FLAGS (\\Seen custom))\r\n"))
	}()
	c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
		Fetch: func(data *imap.FetchMessageData) { updates <- data },
	}})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-updates:
		if data.SeqNum != 9 {
			t.Fatalf("SeqNum = %d", data.SeqNum)
		}
		items := data.Items[imap.FetchDataKey("FLAGS")]
		if len(items) != 1 {
			t.Fatalf("FLAGS items = %#v", items)
		}
		flags, ok := items[0].(imap.FetchDataFlags)
		if !ok || len(flags) != 2 || flags[0] != imap.FlagSeen || flags[1] != imap.Flag("custom") {
			t.Fatalf("FLAGS = %#v", items[0])
		}
	case <-ctx.Done():
		t.Fatal("did not receive FETCH flag update")
	}
}

// TestUnilateralFetchAllItems covers #221. An unsolicited FETCH with a body
// section, which is what a NOTIFY MessageNew registration with fetch
// attributes produces (RFC 5465 section 5.2), reaches the handler with every
// item instead of failing the connection. So does one without FLAGS.
func TestUnilateralFetchAllItems(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	updates := make(chan *imap.FetchMessageData, 4)
	go func() {
		r := bufio.NewReader(serverConn)
		_, _ = serverConn.Write([]byte("* OK ready\r\n"))
		line, _ := r.ReadString('\n')
		tag, _, _ := strings.Cut(line, " ")
		// imapserver's output for NOTIFY SET (selected (MessageNew
		// (UID BODY.PEEK[HEADER.FIELDS (Subject)]))), as quoted in #221.
		_, _ = serverConn.Write([]byte("* 1 EXISTS\r\n" +
			"* 1 FETCH (BODY[HEADER.FIELDS (Subject)] {19}\r\nSubject: pushed\r\n\r\n UID 1)\r\n" +
			"* 2 FETCH (UID 7 MODSEQ (12) X-FUTURE {3}\r\nabc)\r\n" +
			tag + " OK done\r\n"))
	}()
	c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
		Fetch: func(data *imap.FetchMessageData) { updates <- data },
	}})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Noop(nil).Wait(ctx); err != nil {
		t.Fatalf("NOOP after unsolicited FETCH = %v", err)
	}
	// Both responses were delivered before the tagged OK, once each.
	if len(updates) != 2 {
		t.Fatalf("Fetch called %d times, want 2", len(updates))
	}

	first := <-updates
	if first.SeqNum != 1 || len(first.Items) != 2 {
		t.Fatalf("first FETCH = %#v, want seq 1 with two items", first)
	}
	if uid := first.Items["UID"]; len(uid) != 1 || uid[0] != imap.FetchDataUID(1) {
		t.Fatalf("first UID = %#v", uid)
	}
	var section *imap.FetchDataBodySection
	for _, values := range first.Items {
		if v, ok := values[0].(*imap.FetchDataBodySection); ok {
			section = v
		}
	}
	if section == nil || len(section.HeaderFields) != 1 || section.HeaderFields[0] != "Subject" {
		t.Fatalf("first body section = %#v", section)
	}
	// The callback has returned; the section is still readable.
	body, err := io.ReadAll(section.Literal)
	if err != nil || string(body) != "Subject: pushed\r\n\r\n" {
		t.Fatalf("body section = %q, %v", body, err)
	}

	second := <-updates
	if second.SeqNum != 2 || len(second.Items) != 3 {
		t.Fatalf("second FETCH = %#v, want seq 2 with three items", second)
	}
	if uid := second.Items["UID"]; len(uid) != 1 || uid[0] != imap.FetchDataUID(7) {
		t.Fatalf("second UID = %#v", uid)
	}
	if modseq := second.Items["MODSEQ"]; len(modseq) != 1 || modseq[0] != imap.FetchDataModSeq(12) {
		t.Fatalf("second MODSEQ = %#v", modseq)
	}
	// A raw value is in memory too, unlike a FETCH command's.
	raw, ok := second.Items["X-FUTURE"][0].(*imap.FetchDataRaw)
	if !ok {
		t.Fatalf("second X-FUTURE = %#v", second.Items["X-FUTURE"])
	}
	if b, err := io.ReadAll(raw.Reader); err != nil || string(b) != "{3}\r\nabc" {
		t.Fatalf("raw X-FUTURE = %q, %v", b, err)
	}
}

// runUnilateralStream sends stream before the tagged OK of a NOOP and returns
// the NOOP's result.
func runUnilateralStream(t *testing.T, handler *UnilateralDataHandler, stream string) error {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close() })
	go func() {
		r := bufio.NewReader(serverConn)
		_, _ = serverConn.Write([]byte("* OK ready\r\n"))
		line, _ := r.ReadString('\n')
		tag, _, _ := strings.Cut(line, " ")
		_, _ = serverConn.Write([]byte(stream + tag + " OK done\r\n"))
	}()
	c := NewClient(clientConn, &Options{UnilateralData: handler})
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	return c.Noop(nil).Wait(ctx)
}

// A literal over the in-memory limit is consumed but not kept: its item is
// delivered with an empty reader, and the items after it still arrive.
func TestUnilateralFetchOversizedLiteral(t *testing.T) {
	size := imapwire.DefaultMaxBufferedLiteralSize + 1
	updates := make(chan *imap.FetchMessageData, 2)
	stream := fmt.Sprintf("* 1 FETCH (BODY[] {%d}\r\n%s UID 9)\r\n", size, strings.Repeat("x", size))
	if err := runUnilateralStream(t, &UnilateralDataHandler{
		Fetch: func(data *imap.FetchMessageData) { updates <- data },
	}, stream); err != nil {
		t.Fatalf("NOOP after an oversized unsolicited literal = %v", err)
	}
	data := <-updates
	if uid := data.Items["UID"]; len(uid) != 1 || uid[0] != imap.FetchDataUID(9) {
		t.Fatalf("UID after the oversized literal = %#v", data.Items)
	}
	section, ok := data.Items["BODY[]"][0].(*imap.FetchDataBodySection)
	if !ok {
		t.Fatalf("BODY[] = %#v", data.Items["BODY[]"])
	}
	if b, err := io.ReadAll(section.Literal); err != nil || len(b) != 0 {
		t.Fatalf("oversized BODY[] kept %d octets, %v; want none", len(b), err)
	}
}

// With a Fetch callback the response is decoded as strictly as a FETCH
// command's result, so a malformed item fails the connection.
func TestUnilateralFetchIsStrict(t *testing.T) {
	err := runUnilateralStream(t, &UnilateralDataHandler{Fetch: func(*imap.FetchMessageData) {}}, "* 1 FETCH (UID 0)\r\n")
	var ierr *imap.Error
	if !errors.As(err, &ierr) || ierr.Type != imap.ErrorTypeProtocol {
		t.Fatalf("NOOP after FETCH (UID 0) = %v, want a protocol error", err)
	}
}

// TestUnilateralFetchWithoutHandler checks that an unsolicited FETCH is
// skipped, literals and malformed items included, when no Fetch callback is
// installed.
func TestUnilateralFetchWithoutHandler(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	exists := make(chan uint32, 1)
	go func() {
		r := bufio.NewReader(serverConn)
		_, _ = serverConn.Write([]byte("* OK ready\r\n"))
		line, _ := r.ReadString('\n')
		tag, _, _ := strings.Cut(line, " ")
		_, _ = serverConn.Write([]byte("* 1 FETCH (BODY[] {5}\r\nhello BINARY[1] ~{3}\r\na\x00b UID 1)\r\n" +
			"* 2 FETCH (UID 0 EMAILID (bad!id) INTERNALDATE \"garbage\")\r\n" +
			"* 4 EXISTS\r\n" +
			tag + " OK done\r\n"))
	}()
	c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
		Exists: func(n uint32) { exists <- n },
	}})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Noop(nil).Wait(ctx); err != nil {
		t.Fatalf("NOOP after unsolicited FETCH = %v", err)
	}
	if n := <-exists; n != 4 {
		t.Fatalf("EXISTS after the FETCH = %d, want 4", n)
	}
}

func TestStartTLSDiscardsCapabilitiesAndRequeries(t *testing.T) {
	certificate := testCertificate(t)
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY IMAP4rev1 STARTTLS CLEARCAP] ready\r\n"))
		clear := bufio.NewReader(serverConn)
		line, _ := clear.ReadString('\n')
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte(tag + " OK begin TLS\r\n"))
		secure := tls.Server(serverConn, &tls.Config{Certificates: []tls.Certificate{certificate}})
		if err := secure.Handshake(); err != nil {
			return
		}
		requests := bufio.NewReader(secure)
		line, _ = requests.ReadString('\n')
		tag = strings.Fields(line)[0]
		_, _ = secure.Write([]byte("* CAPABILITY IMAP4rev1 TLSCAP\r\n"))
		_, _ = secure.Write([]byte(tag + " OK capabilities\r\n"))
	}()
	c := NewClient(clientConn, &Options{InsecureSkipVerify: true, ReadTimeout: 2 * time.Second})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.startTLS(ctx, "mail.example.test:143"); err != nil {
		t.Fatalf("startTLS() = %v", err)
	}
	if caps := c.Capabilities(); len(caps) != 0 {
		t.Fatalf("capabilities after upgrade = %#v", caps)
	}
	if got := c.dec.Options().ReadTimeout; got != 2*time.Second {
		t.Fatalf("post-STARTTLS ReadTimeout = %v, want %v", got, 2*time.Second)
	}
	if err := c.requestCapability(ctx); err != nil {
		t.Fatalf("CAPABILITY after STARTTLS = %v", err)
	}
	caps := c.Capabilities()
	if !caps["TLSCAP"] || caps["CLEARCAP"] {
		t.Fatalf("capabilities after TLS = %#v", caps)
	}
}

func TestTLSConfigCannotBypassExplicitInsecureOption(t *testing.T) {
	config := tlsConfig("mail.example.test:993", &Options{TLSConfig: &tls.Config{InsecureSkipVerify: true}})
	if config.InsecureSkipVerify {
		t.Fatal("TLSConfig bypassed explicit InsecureSkipVerify option")
	}
	if config.MinVersion < tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d", config.MinVersion)
	}
	if config.ServerName != "mail.example.test" {
		t.Fatalf("ServerName = %q", config.ServerName)
	}
}

func TestMidCommandByeAndLogout(t *testing.T) {
	t.Run("mid-command", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		go func() {
			_, _ = serverConn.Write([]byte("* OK ready\r\n"))
			_, _ = bufio.NewReader(serverConn).ReadString('\n')
			_, _ = serverConn.Write([]byte("* BYE shutdown\r\n"))
		}()
		c := NewClient(clientConn, nil)
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.WaitGreeting(ctx, nil); err != nil {
			t.Fatal(err)
		}
		err := c.Noop(nil).Wait(ctx)
		var ierr *imap.Error
		if !errors.As(err, &ierr) || ierr.Type != imap.ErrorTypeBye {
			t.Fatalf("NOOP after BYE = %T %[1]v", err)
		}
	})
	t.Run("logout", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		go func() {
			_, _ = serverConn.Write([]byte("* OK ready\r\n"))
			line, _ := bufio.NewReader(serverConn).ReadString('\n')
			tag := strings.Fields(line)[0]
			_, _ = serverConn.Write([]byte("* BYE logging out\r\n"))
			_, _ = serverConn.Write([]byte(tag + " OK done\r\n"))
		}()
		c := NewClient(clientConn, nil)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.WaitGreeting(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if err := c.Logout(ctx, nil); err != nil {
			t.Fatalf("Logout() = %v", err)
		}
	})
}

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "mail.example.test"},
		DNSNames:     []string{"mail.example.test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestUnilateralStatusResponseAlertAndInProgress pins that an untagged status
// response no command claims reaches the caller with its code. Before T27 it
// was dropped, losing ALERT text RFC 3501 section 7.1 says must reach the
// user and every INPROGRESS notification (RFC 9585). The greeting is not
// delivered: it is the connection's own status, not unilateral data.
func TestUnilateralStatusResponseAlertAndInProgress(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	updates := make(chan *StatusResponse, 3)
	go func() {
		_, _ = serverConn.Write([]byte("* OK [ALERT] greeting is not unilateral\r\n"))
		_, _ = serverConn.Write([]byte("* OK [ALERT] System shutdown in 10 minutes\r\n"))
		_, _ = serverConn.Write([]byte("* OK [INPROGRESS (\"A1\" 5 10)] Searching\r\n"))
		_, _ = serverConn.Write([]byte("* NO [X-FUTURE some args] unknown code\r\n"))
	}()
	c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
		StatusResponse: func(data *StatusResponse) { updates <- data },
	}})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var got []*StatusResponse
	for len(got) < 3 {
		select {
		case data := <-updates:
			got = append(got, data)
		case <-ctx.Done():
			t.Fatalf("received %d status responses, want 3", len(got))
		}
	}
	if got[0].Status != "OK" || got[0].Code != imap.CodeAlert || got[0].Text != "System shutdown in 10 minutes" {
		t.Fatalf("ALERT = %#v", got[0])
	}
	progress, err := ParseInProgressArgs(got[1].CodeArgs)
	if got[1].Code != "INPROGRESS" || err != nil || progress.Tag != "A1" || progress.Progress != 5 || progress.Goal != 10 {
		t.Fatalf("INPROGRESS = %#v (%v, %v)", got[1], progress, err)
	}
	if got[2].Status != "NO" || got[2].Code != "X-FUTURE" || got[2].CodeArgs != "some args" {
		t.Fatalf("unknown code = %#v", got[2])
	}
}

// TestUnilateralMailboxStatusAndList pins the delivery of NOTIFY's events for
// mailboxes other than the selected one (RFC 5465 section 5): an unsolicited
// STATUS for a change, an unsolicited LIST for MailboxName. Both were
// discarded before T27.
func TestUnilateralMailboxStatusAndList(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	statuses := make(chan *StatusData, 1)
	lists := make(chan *ListData, 1)
	go func() {
		_, _ = serverConn.Write([]byte("* OK ready\r\n"))
		_, _ = serverConn.Write([]byte("* STATUS Archive (MESSAGES 12 UIDNEXT 40)\r\n"))
		_, _ = serverConn.Write([]byte("* LIST (\\NonExistent) \"/\" Gone\r\n"))
	}()
	c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
		Status: func(data *StatusData) { statuses <- data },
		List:   func(data *ListData) { lists <- data },
	}})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case data := <-statuses:
		if n, ok := data.Number(imap.StatusItemMessages); data.Mailbox != "Archive" || !ok || n != 12 {
			t.Fatalf("STATUS = %#v", data)
		}
	case <-ctx.Done():
		t.Fatal("did not receive the unsolicited STATUS")
	}
	select {
	case data := <-lists:
		if data.Mailbox != "Gone" || data.Delimiter != '/' || !imap.ContainsAttr(data.Attrs, imap.MailboxAttrNonExistent) {
			t.Fatalf("LIST = %#v", data)
		}
	case <-ctx.Done():
		t.Fatal("did not receive the unsolicited LIST")
	}
}

// TestSelectClaimsItsOwnList pins RFC 9051 section 6.3.2: an IMAP4rev2
// server answers SELECT with a LIST for the selected mailbox. That LIST is
// part of the selection, not a NOTIFY event; a LIST for another mailbox
// arriving at the same time still is one.
func TestSelectClaimsItsOwnList(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	lists := make(chan *ListData, 4)
	go func() {
		r := bufio.NewReader(serverConn)
		_, _ = serverConn.Write([]byte("* PREAUTH ready\r\n"))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("* 3 EXISTS\r\n" +
			"* LIST () \"/\" Elsewhere\r\n" +
			"* LIST () \"/\" INBOX\r\n" +
			"* FLAGS (\\Seen)\r\n" +
			"* OK [UIDVALIDITY 7] ok\r\n" +
			tag + " OK [READ-WRITE] selected\r\n"))
	}()
	c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
		List: func(data *ListData) { lists <- data },
	}})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.enabled["IMAP4REV2"] = struct{}{}
	c.mu.Unlock()
	status, err := c.Select("inbox", nil).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.NumMessages != 3 || status.UIDValidity != 7 {
		t.Fatalf("status = %#v", status)
	}
	close(lists)
	var got []string
	for data := range lists {
		got = append(got, data.Mailbox)
	}
	if len(got) != 1 || got[0] != "Elsewhere" {
		t.Fatalf("List handler got %q, want only the other mailbox's event", got)
	}
}

// TestUnilateralListOldName covers the NOTIFY rename event: LIST extended data
// carries the previous name (RFC 5465 section 5.4). Other extended items beside
// it do not disturb it, and extended data the client cannot parse costs the old
// name, not the response. The other items themselves are covered in
// list_extended_test.go.
func TestUnilateralListOldName(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	lists := make(chan *ListData, 4)
	go func() {
		_, _ = serverConn.Write([]byte("* OK ready\r\n" +
			"* LIST () \"/\" New (\"OLDNAME\" (\"Old\"))\r\n" +
			"* LIST (\\HasChildren) \"/\" Parent (\"CHILDINFO\" (\"SUBSCRIBED\") \"OLDNAME\" (\"P&AOQ-rent\"))\r\n" +
			"* LIST () \"/\" Odd (\"OLDNAME\")\r\n" +
			"* LIST () \"/\" Plain\r\n"))
	}()
	c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
		List: func(data *ListData) { lists <- data },
	}})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	want := []struct{ mailbox, oldName string }{
		{"New", "Old"},
		{"Parent", "Pärent"},
		{"Odd", ""},
		{"Plain", ""},
	}
	for _, w := range want {
		select {
		case data := <-lists:
			if data.Mailbox != w.mailbox || data.OldName != w.oldName || data.Delimiter != '/' {
				t.Fatalf("LIST = %#v, want mailbox %q with OldName %q", data, w.mailbox, w.oldName)
			}
			if w.mailbox == "Parent" && !imap.ContainsAttr(data.Attrs, imap.MailboxAttrHasChildren) {
				t.Fatalf("attributes lost beside extended data: %#v", data)
			}
		case <-ctx.Done():
			t.Fatalf("did not receive the LIST for %s", w.mailbox)
		}
	}
}

// TestSelectClaimsItsOwnListByOldName covers RFC 9051 section 6.3.2's
// non-canonical case: the server lists the canonical name with the name the
// client sent as OLDNAME, and that LIST still belongs to the SELECT.
func TestSelectClaimsItsOwnListByOldName(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	lists := make(chan *ListData, 4)
	go func() {
		r := bufio.NewReader(serverConn)
		_, _ = serverConn.Write([]byte("* PREAUTH ready\r\n"))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("* 0 EXISTS\r\n" +
			"* LIST () \"/\" Archive (\"OLDNAME\" (\"Archive/\"))\r\n" +
			"* OK [UIDVALIDITY 3] ok\r\n" +
			tag + " OK [READ-WRITE] selected\r\n"))
	}()
	c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
		List: func(data *ListData) { lists <- data },
	}})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.enabled["IMAP4REV2"] = struct{}{}
	c.mu.Unlock()
	if _, err := c.Select("Archive/", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	close(lists)
	for data := range lists {
		t.Fatalf("the SELECT's own LIST reached the List handler: %#v", data)
	}
}
