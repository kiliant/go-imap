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

// runAuthenticate drives a minimal fake server over net.Pipe that captures
// the AUTHENTICATE command line (including any SASL-IR initial response)
// and then immediately fails the command, so the test can inspect exactly
// what Authenticate put on the wire without needing to complete a full
// mechanism exchange. capabilities is the greeting's CAPABILITY list.
func runAuthenticate(t *testing.T, capabilities string, username, password string, opts *AuthenticateOptions) (line string, authErr error) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	lines := make(chan string, 1)
	go func() {
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY " + capabilities + "] ready\r\n"))
		reader := bufio.NewReader(serverConn)
		_ = serverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		got, _ := reader.ReadString('\n')
		lines <- got
		if got == "" {
			return
		}
		fields := strings.Fields(got)
		if len(fields) == 0 {
			return
		}
		_, _ = serverConn.Write([]byte(fields[0] + " NO rejected\r\n"))
	}()
	c := NewClient(clientConn, &Options{AllowInsecureAuth: true})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	authErr = c.Authenticate(ctx, username, password, opts)
	line = <-lines
	return line, authErr
}

// decodeInitialResponse extracts and base64-decodes the SASL-IR initial
// response from a captured "<tag> AUTHENTICATE <mechanism> <initial>\r\n"
// line.
func decodeInitialResponse(t *testing.T, line string) []byte {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[1] != "AUTHENTICATE" {
		t.Fatalf("unexpected AUTHENTICATE line: %q", line)
	}
	if fields[3] == "=" {
		return []byte{}
	}
	payload, err := base64.StdEncoding.DecodeString(fields[3])
	if err != nil {
		t.Fatalf("invalid base64 initial response %q: %v", fields[3], err)
	}
	return payload
}

// TestPrepareCredentialsDefaultUnchanged pins the regression guard: with
// PrepareCredentials false (including the implicit false of a nil opts),
// the bytes PLAIN sends for a password containing U+00B5 MICRO SIGN are
// exactly what they were before this option existed -- the raw octets,
// not the NFKC-normalized U+03BC GREEK SMALL LETTER MU.
func TestPrepareCredentialsDefaultUnchanged(t *testing.T) {
	const password = "interop-pw-\u00b5"

	for _, opts := range []*AuthenticateOptions{
		nil,
		{Mechanism: "PLAIN"},
		{Mechanism: "PLAIN", PrepareCredentials: false},
	} {
		line, _ := runAuthenticate(t, "AUTH=PLAIN SASL-IR", "alice", password, opts)
		payload := decodeInitialResponse(t, line)
		want := "\x00alice\x00" + password
		if string(payload) != want {
			t.Fatalf("PLAIN payload = %q, want unchanged %q (opts=%+v)", payload, want, opts)
		}
	}
}

// TestPrepareCredentialsEnabledPlainTransformsPassword checks that, with
// PrepareCredentials true, a PLAIN password of "interop-pw-\u00b5" reaches
// the wire as its SASLprep/NFKC form "interop-pw-\u03bc", observable in the
// base64 initial response.
func TestPrepareCredentialsEnabledPlainTransformsPassword(t *testing.T) {
	const rawPassword = "interop-pw-\u00b5"
	const wantPassword = "interop-pw-\u03bc"

	line, _ := runAuthenticate(t, "AUTH=PLAIN SASL-IR", "alice", rawPassword,
		&AuthenticateOptions{Mechanism: "PLAIN", PrepareCredentials: true})
	payload := decodeInitialResponse(t, line)
	want := "\x00alice\x00" + wantPassword
	if string(payload) != want {
		t.Fatalf("PLAIN payload = %q, want prepared %q", payload, want)
	}
	if strings.Contains(string(payload), rawPassword) {
		t.Fatalf("PLAIN payload still contains the raw, unprepared password: %q", payload)
	}
}

// TestPrepareCredentialsEnabledSCRAMTransformsPassword asserts the prepared
// value at the seam that feeds every built-in password mechanism
// (prepareCredentials in auth.go), rather than duplicating SCRAM's PBKDF2
// and HMAC maths just to observe the derived proof. This is the same
// function call selectSASL/builtinSASL makes immediately before
// constructing imapsasl.SCRAMSHA256, so it pins exactly what that
// mechanism will receive.
func TestPrepareCredentialsEnabledSCRAMTransformsPassword(t *testing.T) {
	const rawPassword = "interop-pw-\u00b5"
	const wantPassword = "interop-pw-\u03bc"

	_, preparedPassword, err := prepareCredentials(&AuthenticateOptions{PrepareCredentials: true}, "alice", rawPassword)
	if err != nil {
		t.Fatalf("prepareCredentials: unexpected error: %v", err)
	}
	if preparedPassword != wantPassword {
		t.Fatalf("prepareCredentials password = %q, want %q", preparedPassword, wantPassword)
	}
}

// TestPrepareCredentialsUsernamePreparedForSCRAM checks RFC 5802 section
// 5.1's requirement that SCRAM prepares the user name too, not only the
// password: with PrepareCredentials true, the client-first message's "n="
// attribute -- sent as the SASL-IR initial response -- carries the
// SASLprep-prepared user name.
func TestPrepareCredentialsUsernamePreparedForSCRAM(t *testing.T) {
	const rawUsername = "u\u00b5ser"
	const wantUsername = "u\u03bcser"

	line, _ := runAuthenticate(t, "AUTH=SCRAM-SHA-256 SASL-IR", rawUsername, "top-secret",
		&AuthenticateOptions{Mechanism: "SCRAM-SHA-256", PrepareCredentials: true})
	payload := string(decodeInitialResponse(t, line))
	// The client-first-message is "n,,n=<username>,r=<nonce>": strip the
	// GS2 header and the trailing nonce attribute to isolate "n=".
	idx := strings.Index(payload, "n=")
	if idx < 0 {
		t.Fatalf("SCRAM client-first message has no n= attribute: %q", payload)
	}
	rest := payload[idx+len("n="):]
	end := strings.Index(rest, ",r=")
	if end < 0 {
		t.Fatalf("SCRAM client-first message has no r= attribute: %q", payload)
	}
	gotUsername := rest[:end]
	if gotUsername != wantUsername {
		t.Fatalf("SCRAM n= attribute = %q, want prepared %q (payload=%q)", gotUsername, wantUsername, payload)
	}
	if strings.Contains(payload, rawUsername) {
		t.Fatalf("SCRAM client-first message still contains the raw, unprepared username: %q", payload)
	}
}

// TestPrepareCredentialsASCIINoOp proves enabling PrepareCredentials does
// not disturb an ordinary ASCII credential: the PLAIN wire bytes are
// identical whether the option is on or off.
func TestPrepareCredentialsASCIINoOp(t *testing.T) {
	lineOff, _ := runAuthenticate(t, "AUTH=PLAIN SASL-IR", "alice", "top-secret",
		&AuthenticateOptions{Mechanism: "PLAIN", PrepareCredentials: false})
	lineOn, _ := runAuthenticate(t, "AUTH=PLAIN SASL-IR", "alice", "top-secret",
		&AuthenticateOptions{Mechanism: "PLAIN", PrepareCredentials: true})
	if lineOff != lineOn {
		t.Fatalf("ASCII credential differs with PrepareCredentials on/off:\noff: %q\non:  %q", lineOff, lineOn)
	}
}

// TestPrepareCredentialsProhibitedRefusesBeforeWire checks that a
// prohibited code point (RFC 3454 Table C.2.1, an ASCII control
// character) and, separately, a bidi violation (RFC 3454 Section 6) each
// abort Authenticate with a *imap.Error of type ErrorTypeProtocol before
// a single byte reaches the connection.
func TestPrepareCredentialsProhibitedRefusesBeforeWire(t *testing.T) {
	cases := []struct {
		name     string
		username string
		password string
	}{
		{"prohibited control character in password", "alice", "hunter\u00072"},
		{"bidi violation in password", "alice", "\u06271"}, // RandALCat then a non-RandALCat last character
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer serverConn.Close()
			received := make(chan string, 1)
			go func() {
				_, _ = serverConn.Write([]byte("* OK [CAPABILITY AUTH=PLAIN SASL-IR] ready\r\n"))
				_ = serverConn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
				line, _ := bufio.NewReader(serverConn).ReadString('\n')
				received <- line
			}()
			c := NewClient(clientConn, &Options{AllowInsecureAuth: true})
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := c.WaitGreeting(ctx); err != nil {
				t.Fatal(err)
			}
			err := c.Authenticate(ctx, tc.username, tc.password, &AuthenticateOptions{
				Mechanism:          "PLAIN",
				PrepareCredentials: true,
			})
			var ierr *imap.Error
			if !errors.As(err, &ierr) || ierr.Type != imap.ErrorTypeProtocol {
				t.Fatalf("Authenticate() = %T %[1]v, want *imap.Error{Type: ErrorTypeProtocol}", err)
			}
			if got := <-received; got != "" {
				t.Fatalf("Authenticate wrote to the connection before SASLprep validation: %q", got)
			}
		})
	}
}

// TestPrepareCredentialsErrorDoesNotLeakCredentials guards against a
// SASLprep failure surfacing the username or password anywhere in the
// returned *imap.Error: not in Error(), not in Text, not in CodeArgs. The
// underlying internal/saslprep errors already avoid this; this test pins
// that wrapping in auth.go does not reintroduce the credential, since that
// is an easy thing for someone to "helpfully" add back later (e.g. by
// formatting the original password into a "SASLprep rejected %q" message).
func TestPrepareCredentialsErrorDoesNotLeakCredentials(t *testing.T) {
	const username = "distinctive-username-marker"
	const password = "distinctive-password-marker\u0007"

	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY AUTH=PLAIN SASL-IR] ready\r\n"))
		_ = serverConn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		_, _ = bufio.NewReader(serverConn).ReadString('\n')
	}()
	c := NewClient(clientConn, &Options{AllowInsecureAuth: true})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	err := c.Authenticate(ctx, username, password, &AuthenticateOptions{
		Mechanism:          "PLAIN",
		PrepareCredentials: true,
	})
	var ierr *imap.Error
	if !errors.As(err, &ierr) {
		t.Fatalf("Authenticate() = %T %[1]v, want *imap.Error", err)
	}
	for _, secret := range []string{username, password, "distinctive-username-marker", "distinctive-password-marker"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Error() leaks credential %q: %v", secret, err)
		}
		if strings.Contains(ierr.Text, secret) {
			t.Fatalf("Text leaks credential %q: %q", secret, ierr.Text)
		}
		if strings.Contains(ierr.CodeArgs, secret) {
			t.Fatalf("CodeArgs leaks credential %q: %q", secret, ierr.CodeArgs)
		}
	}
}

// TestPrepareCredentialsTokensUntouched checks that PrepareCredentials
// never transforms an OAuth bearer token: opts.Token is opaque, and with
// the option enabled a token containing U+00B5 must still be transmitted
// unchanged, for both XOAUTH2 and OAUTHBEARER.
func TestPrepareCredentialsTokensUntouched(t *testing.T) {
	const token = "tok-\u00b5-en"

	t.Run("XOAUTH2", func(t *testing.T) {
		line, _ := runAuthenticate(t, "AUTH=XOAUTH2 SASL-IR", "alice", "unused",
			&AuthenticateOptions{Mechanism: "XOAUTH2", Token: token, PrepareCredentials: true})
		payload := string(decodeInitialResponse(t, line))
		want := "user=alice\x01auth=Bearer " + token + "\x01\x01"
		if payload != want {
			t.Fatalf("XOAUTH2 payload = %q, want unchanged token in %q", payload, want)
		}
	})

	t.Run("OAUTHBEARER", func(t *testing.T) {
		line, _ := runAuthenticate(t, "AUTH=OAUTHBEARER SASL-IR", "alice", "unused",
			&AuthenticateOptions{Mechanism: "OAUTHBEARER", Token: token, PrepareCredentials: true})
		payload := string(decodeInitialResponse(t, line))
		if !strings.Contains(payload, "auth=Bearer "+token) {
			t.Fatalf("OAUTHBEARER payload = %q, want unchanged token %q", payload, token)
		}
	})
}

// TestPrepareCredentialsRedactsPreparedForm covers the case where redaction
// and preparation disagree: the octets on the wire are the prepared ones, so
// a server that echoes the authentication identity in its NO text would leak
// the prepared user name if only the caller's original input were redacted.
func TestPrepareCredentialsRedactsPreparedForm(t *testing.T) {
	const (
		username = "alice-\u00b5"
		prepared = "alice-\u03bc"
	)
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY AUTH=PLAIN SASL-IR] ready\r\n"))
		reader := bufio.NewReader(serverConn)
		_ = serverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		got, _ := reader.ReadString('\n')
		fields := strings.Fields(got)
		if len(fields) == 0 {
			return
		}
		_, _ = serverConn.Write([]byte(fields[0] + " NO no such user " + prepared + "\r\n"))
	}()
	c := NewClient(clientConn, &Options{AllowInsecureAuth: true})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	err := c.Authenticate(ctx, username, "hunter2", &AuthenticateOptions{
		Mechanism:          "PLAIN",
		PrepareCredentials: true,
	})
	if err == nil {
		t.Fatal("Authenticate() = nil, want the server's NO")
	}
	if strings.Contains(err.Error(), prepared) {
		t.Fatalf("error leaks the prepared user name %q: %q", prepared, err)
	}
}
