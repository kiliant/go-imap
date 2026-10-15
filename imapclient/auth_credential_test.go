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

// authenticateFirstLine runs Authenticate as alice/top-secret against a server
// advertising capabilities with SASL-IR, and returns the first command line
// the client sent, "" when it sent none, together with Authenticate's error.
// The server answers any command with NO, so the exchange ends at once.
func authenticateFirstLine(t *testing.T, capabilities string, opts *AuthenticateOptions) (string, error) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	lines := make(chan string, 1)
	go func() {
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY IMAP4rev1 SASL-IR " + capabilities + "] ready\r\n"))
		// Close below ends a session that sends nothing with EOF.
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		lines <- line
		if fields := strings.Fields(line); len(fields) > 0 {
			_, _ = serverConn.Write([]byte(fields[0] + " NO [AUTHENTICATIONFAILED] stop\r\n"))
		}
	}()
	c := NewClient(clientConn, &Options{AllowInsecureAuth: true})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		c.Close()
		t.Fatal(err)
	}
	err := c.Authenticate(ctx, "alice", "top-secret", opts)
	c.Close()
	return <-lines, err
}

// sentMechanism returns the mechanism of an AUTHENTICATE command line and its
// decoded initial response.
func sentMechanism(t *testing.T, line string) (string, string) {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[1] != "AUTHENTICATE" {
		t.Fatalf("first command = %q, want AUTHENTICATE", line)
	}
	if len(fields) < 4 {
		return fields[2], ""
	}
	payload, err := base64.StdEncoding.DecodeString(fields[3])
	if err != nil {
		t.Fatalf("initial response %q: %v", fields[3], err)
	}
	return fields[2], string(payload)
}

// TestAuthenticateUnsetCredentialKindSendsPasswordAsBearerToken pins the v1
// default that AuthenticateOptions.CredentialKind exists to opt out of: with
// no Token, a server offering OAUTHBEARER beside PLAIN is sent the password
// as the bearer token. Changing this is a v2 decision.
func TestAuthenticateUnsetCredentialKindSendsPasswordAsBearerToken(t *testing.T) {
	for _, opts := range []*AuthenticateOptions{nil, {}} {
		line, err := authenticateFirstLine(t, "AUTH=OAUTHBEARER AUTH=PLAIN", opts)
		if err == nil {
			t.Fatal("Authenticate() succeeded against a server that answers NO")
		}
		mechanism, payload := sentMechanism(t, line)
		if mechanism != "OAUTHBEARER" || payload != "n,a=alice,\x01auth=Bearer top-secret\x01\x01" {
			t.Fatalf("opts %#v: sent %s %q, want OAUTHBEARER with the password as token", opts, mechanism, payload)
		}
	}
}

func TestAuthenticateCredentialSelection(t *testing.T) {
	tests := []struct {
		name         string
		capabilities string
		opts         *AuthenticateOptions
		want         string
	}{
		{"password skips OAuth for PLAIN", "AUTH=OAUTHBEARER AUTH=XOAUTH2 AUTH=PLAIN", &AuthenticateOptions{CredentialKind: CredentialPassword}, "PLAIN"},
		{"password keeps SCRAM first", "AUTH=OAUTHBEARER AUTH=PLAIN AUTH=SCRAM-SHA-256", &AuthenticateOptions{CredentialKind: CredentialPassword}, "SCRAM-SHA-256"},
		{"password keeps the default order", "AUTH=LOGIN AUTH=PLAIN AUTH=CRAM-MD5", &AuthenticateOptions{CredentialKind: CredentialPassword}, "CRAM-MD5"},
		{"password falls through to LOGIN", "AUTH=XOAUTH2 AUTH=LOGIN", &AuthenticateOptions{CredentialKind: CredentialPassword}, "LOGIN"},
		{"password allows an explicit password mechanism", "AUTH=OAUTHBEARER AUTH=PLAIN AUTH=CRAM-MD5", &AuthenticateOptions{CredentialKind: CredentialPassword, Mechanism: "plain"}, "PLAIN"},
		{"bearer skips SCRAM", "AUTH=SCRAM-SHA-256 AUTH=PLAIN AUTH=OAUTHBEARER", &AuthenticateOptions{CredentialKind: CredentialBearerToken}, "OAUTHBEARER"},
		{"bearer prefers OAUTHBEARER", "AUTH=XOAUTH2 AUTH=OAUTHBEARER", &AuthenticateOptions{CredentialKind: CredentialBearerToken}, "OAUTHBEARER"},
		{"bearer takes XOAUTH2", "AUTH=PLAIN AUTH=XOAUTH2", &AuthenticateOptions{CredentialKind: CredentialBearerToken}, "XOAUTH2"},
		{"bearer allows an explicit bearer mechanism", "AUTH=OAUTHBEARER AUTH=XOAUTH2", &AuthenticateOptions{CredentialKind: CredentialBearerToken, Mechanism: "XOAUTH2"}, "XOAUTH2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			line, err := authenticateFirstLine(t, test.capabilities, test.opts)
			var ierr *imap.Error
			if !errors.As(err, &ierr) || ierr.Code != imap.CodeAuthenticationFailed {
				t.Fatalf("Authenticate() = %v, want the server's NO", err)
			}
			if mechanism, _ := sentMechanism(t, line); mechanism != test.want {
				t.Fatalf("selected %s, want %s", mechanism, test.want)
			}
		})
	}
}

func TestAuthenticateBearerCredentialToken(t *testing.T) {
	for _, test := range []struct {
		token, want string
	}{
		{"", "top-secret"},
		{"oauth-token", "oauth-token"},
	} {
		line, err := authenticateFirstLine(t, "AUTH=PLAIN AUTH=XOAUTH2", &AuthenticateOptions{CredentialKind: CredentialBearerToken, Token: test.token})
		if err == nil {
			t.Fatal("Authenticate() succeeded against a server that answers NO")
		}
		mechanism, payload := sentMechanism(t, line)
		if mechanism != "XOAUTH2" || payload != "user=alice\x01auth=Bearer "+test.want+"\x01\x01" {
			t.Fatalf("Token %q: sent %s %q, want XOAUTH2 with token %q", test.token, mechanism, payload, test.want)
		}
	}
}

// A mechanism the built-ins do not implement is the caller's own, supplied
// through SASL, so a CredentialKind does not second-guess it.
func TestAuthenticateCredentialAllowsCustomMechanism(t *testing.T) {
	for _, kind := range []CredentialKind{CredentialPassword, CredentialBearerToken} {
		line, err := authenticateFirstLine(t, "AUTH=OAUTHBEARER AUTH=PLAIN AUTH=X-CUSTOM", &AuthenticateOptions{
			CredentialKind: kind,
			Mechanism:      "X-CUSTOM",
			SASL: func(string) (*SASLMechanism, error) {
				return &SASLMechanism{Next: func([]byte) ([]byte, error) { return []byte("custom"), nil }}, nil
			},
		})
		if err == nil {
			t.Fatal("Authenticate() succeeded against a server that answers NO")
		}
		if mechanism, payload := sentMechanism(t, line); mechanism != "X-CUSTOM" || payload != "custom" {
			t.Fatalf("CredentialKind %q: sent %s %q, want X-CUSTOM", kind, mechanism, payload)
		}
	}
}

func TestAuthenticateCredentialRefusedBeforeSending(t *testing.T) {
	tests := []struct {
		name         string
		capabilities string
		opts         *AuthenticateOptions
		want         string
	}{
		{"password with a Token", "AUTH=PLAIN", &AuthenticateOptions{CredentialKind: CredentialPassword, Token: "oauth-token"}, "Token"},
		{"password with OAUTHBEARER", "AUTH=OAUTHBEARER AUTH=PLAIN", &AuthenticateOptions{CredentialKind: CredentialPassword, Mechanism: "OAUTHBEARER"}, "OAUTHBEARER"},
		{"password with xoauth2", "AUTH=XOAUTH2 AUTH=PLAIN", &AuthenticateOptions{CredentialKind: CredentialPassword, Mechanism: "xoauth2"}, "XOAUTH2"},
		{"bearer with PLAIN", "AUTH=OAUTHBEARER AUTH=PLAIN", &AuthenticateOptions{CredentialKind: CredentialBearerToken, Mechanism: "PLAIN"}, "PLAIN"},
		{"bearer with SCRAM", "AUTH=OAUTHBEARER AUTH=SCRAM-SHA-256", &AuthenticateOptions{CredentialKind: CredentialBearerToken, Mechanism: "SCRAM-SHA-256"}, "SCRAM-SHA-256"},
		{"unknown kind", "AUTH=PLAIN", &AuthenticateOptions{CredentialKind: "certificate"}, `"certificate"`},
		{"password, OAuth only", "AUTH=OAUTHBEARER AUTH=XOAUTH2", &AuthenticateOptions{CredentialKind: CredentialPassword}, "password"},
		{"bearer, passwords only", "AUTH=SCRAM-SHA-256 AUTH=PLAIN AUTH=LOGIN", &AuthenticateOptions{CredentialKind: CredentialBearerToken}, "bearer-token"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			line, err := authenticateFirstLine(t, test.capabilities, test.opts)
			if line != "" {
				t.Fatalf("client sent %q before refusing", line)
			}
			var ierr *imap.Error
			if !errors.As(err, &ierr) || ierr.Type != imap.ErrorTypeProtocol {
				t.Fatalf("Authenticate() = %T %[1]v, want a protocol *imap.Error", err)
			}
			if !strings.Contains(ierr.Text, test.want) {
				t.Fatalf("error %q does not mention %s", ierr.Text, test.want)
			}
			for _, secret := range []string{"top-secret", "oauth-token"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error %q leaks %q", err, secret)
				}
			}
		})
	}
}

// Every mechanism automatic selection can choose must belong to exactly one
// CredentialKind, or filtering by kind would silently exclude or double-count
// it.
func TestSASLPreferenceIsPartitionedByCredentialKind(t *testing.T) {
	for _, name := range saslPreference {
		if passwordMechanisms[name] == bearerMechanisms[name] {
			t.Errorf("%s: password %t, bearer token %t; want exactly one", name, passwordMechanisms[name], bearerMechanisms[name])
		}
	}
	for name := range passwordMechanisms {
		if bearerMechanisms[name] {
			t.Errorf("%s is both a password and a bearer-token mechanism", name)
		}
	}
}
