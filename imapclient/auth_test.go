package imapclient

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

func TestLoginRefusesDowngradeBeforeCredentials(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	requests := make(chan string, 1)
	go func() {
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY IMAP4rev1 LOGINDISABLED] ready\r\n"))
		_ = serverConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		requests <- line
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	err := c.Login(ctx, "alice", "top-secret", nil)
	var ierr *imap.Error
	if !errors.As(err, &ierr) || ierr.Type != imap.ErrorTypeProtocol {
		t.Fatalf("Login() = %T %[1]v", err)
	}
	if got := <-requests; got != "" {
		t.Fatalf("LOGIN sent credentials despite LOGINDISABLED: %q", got)
	}
}

func TestAuthenticatePlainCleartextRefusedBeforeCredentials(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	requests := make(chan string, 1)
	go func() {
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN SASL-IR] ready\r\n"))
		_ = serverConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		requests <- line
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	err := c.Authenticate(ctx, "alice", "top-secret", nil)
	if err == nil || !strings.Contains(err.Error(), "cleartext") {
		t.Fatalf("Authenticate() = %v", err)
	}
	if got := <-requests; got != "" {
		t.Fatalf("AUTHENTICATE sent credentials over cleartext: %q", got)
	}
}

func TestAuthenticateSASLIRAndPostAuthCapability(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN SASL-IR] ready\r\n"))
		reader := bufio.NewReader(serverConn)
		line, _ := reader.ReadString('\n')
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[1] != "AUTHENTICATE" || fields[2] != "PLAIN" {
			return
		}
		payload, _ := base64.StdEncoding.DecodeString(fields[3])
		if string(payload) != "\x00alice\x00top-secret" {
			return
		}
		_, _ = serverConn.Write([]byte(fields[0] + " OK authenticated\r\n"))
		line, _ = reader.ReadString('\n')
		fields = strings.Fields(line)
		_, _ = serverConn.Write([]byte("* CAPABILITY IMAP4rev1 POSTAUTH\r\n"))
		_, _ = serverConn.Write([]byte(fields[0] + " OK capabilities\r\n"))
		_, _ = reader.ReadByte()
	}()
	var trace []string
	c := NewClient(clientConn, &Options{AllowInsecureAuth: true, Trace: func(event TraceEvent) { trace = append(trace, event.Data) }})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Authenticate(ctx, "alice", "top-secret", nil); err != nil {
		t.Fatalf("Authenticate() = %v", err)
	}
	if c.State() != StateAuthenticated || !c.Capabilities()["POSTAUTH"] {
		t.Fatalf("state/capabilities after authentication = %q %#v", c.State(), c.Capabilities())
	}
	for _, event := range trace {
		if strings.Contains(event, "alice") || strings.Contains(event, "top-secret") {
			t.Fatalf("trace leaked authentication data: %q", event)
		}
	}
}

func TestAuthenticateEmptyInitialResponseUsesEquals(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY AUTH=CUSTOM SASL-IR] ready\r\n"))
		reader := bufio.NewReader(serverConn)
		line, _ := reader.ReadString('\n')
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[3] != "=" {
			return
		}
		_, _ = serverConn.Write([]byte(fields[0] + " OK authenticated\r\n"))
		line, _ = reader.ReadString('\n')
		fields = strings.Fields(line)
		_, _ = serverConn.Write([]byte("* CAPABILITY IMAP4rev1\r\n"))
		_, _ = serverConn.Write([]byte(fields[0] + " OK capabilities\r\n"))
		_, _ = reader.ReadByte()
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	err := c.Authenticate(ctx, "", "", &AuthenticateOptions{
		Mechanism: "CUSTOM",
		SASL: func(string) (*SASLMechanism, error) {
			return &SASLMechanism{Next: func([]byte) ([]byte, error) { return []byte{}, nil }}, nil
		},
	})
	if err != nil {
		t.Fatalf("Authenticate() = %v", err)
	}
}

func TestAuthenticateContinuationWithoutSASLIR(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY AUTH=PLAIN] ready\r\n"))
		reader := bufio.NewReader(serverConn)
		line, _ := reader.ReadString('\n')
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[1] != "AUTHENTICATE" || fields[2] != "PLAIN" {
			return
		}
		_, _ = serverConn.Write([]byte("+ \r\n"))
		line, _ = reader.ReadString('\n')
		payload, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line))
		if string(payload) != "\x00alice\x00top-secret" {
			return
		}
		_, _ = serverConn.Write([]byte(fields[0] + " OK authenticated\r\n"))
		line, _ = reader.ReadString('\n')
		fields = strings.Fields(line)
		_, _ = serverConn.Write([]byte("* CAPABILITY IMAP4rev1\r\n"))
		_, _ = serverConn.Write([]byte(fields[0] + " OK capabilities\r\n"))
		_, _ = reader.ReadByte()
	}()
	c := NewClient(clientConn, &Options{AllowInsecureAuth: true})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Authenticate(ctx, "alice", "top-secret", nil); err != nil {
		t.Fatalf("Authenticate() = %v", err)
	}
}

func TestAuthenticationErrorRedactsCredentialsAndPreservesCode(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY IMAP4rev1] ready\r\n"))
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte(tag + " NO [AUTHENTICATIONFAILED] alice top-secret rejected\r\n"))
	}()
	c := NewClient(clientConn, &Options{AllowInsecureAuth: true})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	err := c.Login(ctx, "alice", "top-secret", nil)
	var ierr *imap.Error
	if !errors.As(err, &ierr) || ierr.Code != imap.CodeAuthenticationFailed {
		t.Fatalf("Login() = %T %[1]v", err)
	}
	if strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("authentication error leaked credentials: %v", err)
	}
}
