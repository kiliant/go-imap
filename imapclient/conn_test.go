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
