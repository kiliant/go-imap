package imapserver_test

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
)

func TestLoopbackFrameworkLifecycle(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	server := imapserver.New(nil, &imapserver.Options{ServerID: map[string]string{"name": "go-imap"}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ServeConn(ctx, serverSide) }()

	client := imapclient.NewClient(clientSide, nil)
	if err := client.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for _, capability := range []string{"IMAP4REV1", "ENABLE", "ID", "LITERAL-"} {
		if !client.Supports(capability) {
			t.Errorf("greeting omitted %s", capability)
		}
	}
	if err := client.Capability(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Noop(nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := client.ID(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Received || len(id.Fields) != 1 || id.Fields[0].Name != "name" || id.Fields[0].Value == nil || *id.Fields[0].Value != "go-imap" {
		t.Fatalf("ID response = %#v", id)
	}
	if err := client.Logout(ctx, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestLoopbackStartTLSRefreshesCapabilities(t *testing.T) {
	certificate := testCertificate(t)
	server := imapserver.New(nil, &imapserver.Options{TLSConfig: &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverSide, clientSide := net.Pipe()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ServeConn(ctx, serverSide) }()

	clearReader := bufio.NewReader(clientSide)
	if line, err := clearReader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "* OK ") {
		t.Fatalf("greeting = %q, %v", line, err)
	}
	if _, err := clientSide.Write([]byte("A1 STARTTLS\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := clearReader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A1 OK ") {
		t.Fatalf("STARTTLS response = %q, %v", line, err)
	}
	tlsClient := tls.Client(clientSide, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) // controlled test endpoint
	if err := tlsClient.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	tlsReader := bufio.NewReader(tlsClient)
	if _, err := tlsClient.Write([]byte("A2 CAPABILITY\r\n")); err != nil {
		t.Fatal(err)
	}
	capabilityLine, err := tlsReader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(capabilityLine, "STARTTLS") {
		t.Fatalf("cleartext capability survived TLS: %q", capabilityLine)
	}
	if line, err := tlsReader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A2 OK ") {
		t.Fatalf("CAPABILITY completion = %q, %v", line, err)
	}
	if _, err := tlsClient.Write([]byte("A3 NOOP\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := tlsReader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A3 OK ") {
		t.Fatalf("NOOP completion = %q, %v", line, err)
	}
	if _, err := tlsClient.Write([]byte("A4 LOGOUT\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := tlsReader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "* BYE ") {
		t.Fatalf("LOGOUT BYE = %q, %v", line, err)
	}
	if line, err := tlsReader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A4 OK ") {
		t.Fatalf("LOGOUT completion = %q, %v", line, err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestImplicitTLSHandshakeUsesPreAuthTimeout(t *testing.T) {
	certificate := testCertificate(t)
	server := imapserver.New(nil, &imapserver.Options{
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}},
		Limits:    imapserver.Limits{PreAuthTimeout: 20 * time.Millisecond},
	})
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close() // deliberately never starts a TLS handshake
	done := make(chan error, 1)
	go func() {
		done <- server.ServeConn(context.Background(), tls.Server(serverSide, serverTLSConfig(t, certificate)))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled TLS handshake unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("pre-auth timeout did not stop TLS handshake")
	}
}

func serverTLSConfig(t *testing.T, certificate tls.Certificate) *tls.Config {
	t.Helper()
	return &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
}

func TestStartTLSDiscardsBufferedPlaintextCommands(t *testing.T) {
	certificate := testCertificate(t)
	server := imapserver.New(nil, &imapserver.Options{TLSConfig: &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	serverSide, clientSide := net.Pipe()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ServeConn(ctx, serverSide) }()

	clearReader := bufio.NewReader(clientSide)
	if _, err := clearReader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// A2 is deliberately sent in the same cleartext write. The reader's
	// buffering may already hold it when STARTTLS is dispatched, but it must
	// never become a command on the protected connection.
	if _, err := clientSide.Write([]byte("A1 STARTTLS\r\nA2 NOOP\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := clearReader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A1 OK ") {
		t.Fatalf("STARTTLS response = %q, %v", line, err)
	}
	tlsClient := tls.Client(clientSide, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) // controlled test endpoint
	if err := tlsClient.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	tlsReader := bufio.NewReader(tlsClient)
	if _, err := tlsClient.Write([]byte("A3 CAPABILITY\r\n")); err != nil {
		t.Fatal(err)
	}
	line, err := tlsReader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "* CAPABILITY ") || strings.HasPrefix(line, "A2 ") {
		t.Fatalf("buffered cleartext command executed after STARTTLS: %q", line)
	}
	if line, err = tlsReader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A3 OK ") {
		t.Fatalf("CAPABILITY completion = %q, %v", line, err)
	}
	if _, err := tlsClient.Write([]byte("A4 LOGOUT\r\n")); err != nil {
		t.Fatal(err)
	}
	_, _ = tlsReader.ReadString('\n')
	_, _ = tlsReader.ReadString('\n')
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestSynchronisingCommandLiteralsAreCoordinated(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	server := imapserver.New(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ServeConn(ctx, serverSide) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	if _, err := clientSide.Write([]byte("A1 ID ({4}\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+ ") {
		t.Fatalf("first continuation = %q, %v", line, err)
	}
	if _, err := clientSide.Write([]byte("name {5}\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+ ") {
		t.Fatalf("second continuation = %q, %v", line, err)
	}
	if _, err := clientSide.Write([]byte("value)\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "* ID NIL") {
		t.Fatalf("ID response = %q, %v", line, err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A1 OK ") {
		t.Fatalf("ID completion = %q, %v", line, err)
	}
	if _, err := clientSide.Write([]byte("A2 LOGOUT\r\n")); err != nil {
		t.Fatal(err)
	}
	_, _ = reader.ReadString('\n')
	_, _ = reader.ReadString('\n')
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestUnknownNonSynchronisingLiteralRecoversNextCommand(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	server := imapserver.New(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ServeConn(ctx, serverSide) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := clientSide.Write([]byte("A1 FUTURE {3+}\r\nabc\r\nA2 NOOP\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A1 BAD ") {
		t.Fatalf("unknown command response = %q, %v", line, err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A2 OK ") {
		t.Fatalf("pipelined NOOP response = %q, %v", line, err)
	}
	if _, err := clientSide.Write([]byte("A3 LOGOUT\r\n")); err != nil {
		t.Fatal(err)
	}
	_, _ = reader.ReadString('\n')
	_, _ = reader.ReadString('\n')
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

type countingListener struct {
	connections chan net.Conn
	closed      chan struct{}
	accepts     atomic.Int32
	closeOnce   sync.Once
}

func newCountingListener() *countingListener {
	return &countingListener{connections: make(chan net.Conn, 4), closed: make(chan struct{})}
}

func (l *countingListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.connections:
		l.accepts.Add(1)
		return connection, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *countingListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (*countingListener) Addr() net.Addr { return pipeAddr("listener") }

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

func TestServeAppliesConnectionLimitBeforeSpawning(t *testing.T) {
	listener := newCountingListener()
	server := imapserver.New(nil, &imapserver.Options{Limits: imapserver.Limits{MaxConnections: 1, MaxConnectionsPerIP: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx, listener) }()

	serverOne, clientOne := net.Pipe()
	defer clientOne.Close()
	listener.connections <- serverOne
	reader := bufio.NewReader(clientOne)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	serverTwo, clientTwo := net.Pipe()
	defer clientTwo.Close()
	listener.connections <- serverTwo
	_ = clientTwo.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := bufio.NewReader(clientTwo).ReadString('\n'); err == nil {
		t.Fatal("connection over the cap received a greeting")
	}
	if got := listener.accepts.Load(); got != 2 {
		t.Fatalf("accepted connections = %d", got)
	}
	cancel()
	_ = clientOne.Close()
	select {
	case err := <-serveDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop")
	}
}

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
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
