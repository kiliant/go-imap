package imapclient

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"strings"
	"testing"
)

// TestAuthenticateSkipsChannelBindingWithoutTLS pins that automatic selection
// passes over a -PLUS variant on a connection with no TLS channel binding,
// rather than choosing it and failing before anything is sent. The plain
// SCRAM chosen instead must say, with the GS2 flag "n", that the client
// cannot bind, so the server does not read it as a downgrade.
func TestAuthenticateSkipsChannelBindingWithoutTLS(t *testing.T) {
	tests := []struct {
		name         string
		capabilities string
		opts         *AuthenticateOptions
		want         string
	}{
		{"SHA-256", "AUTH=SCRAM-SHA-256-PLUS AUTH=SCRAM-SHA-256", nil, "SCRAM-SHA-256"},
		{"SHA-1", "AUTH=SCRAM-SHA-1-PLUS AUTH=SCRAM-SHA-1", nil, "SCRAM-SHA-1"},
		{"both PLUS variants", "AUTH=SCRAM-SHA-256-PLUS AUTH=SCRAM-SHA-1-PLUS AUTH=SCRAM-SHA-1", nil, "SCRAM-SHA-1"},
		{"no plain SCRAM", "AUTH=SCRAM-SHA-256-PLUS AUTH=PLAIN", nil, "PLAIN"},
		{"password kind", "AUTH=SCRAM-SHA-256-PLUS AUTH=SCRAM-SHA-256", &AuthenticateOptions{CredentialKind: CredentialPassword}, "SCRAM-SHA-256"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			line, err := authenticateFirstLine(t, test.capabilities, test.opts)
			if line == "" {
				t.Fatalf("Authenticate() sent nothing: %v", err)
			}
			mechanism, payload := sentMechanism(t, line)
			if mechanism != test.want {
				t.Fatalf("sent %s, want %s", mechanism, test.want)
			}
			if strings.HasPrefix(mechanism, "SCRAM-") && !strings.HasPrefix(payload, "n,,") {
				t.Fatalf("%s initial response %q, want the GS2 flag n", mechanism, payload)
			}
		})
	}
}

// TestAuthenticateChannelBindingRefusedWithoutTLS pins the cases that still
// fail without a channel binding: an explicit -PLUS Mechanism, and automatic
// selection when only -PLUS variants are advertised. Both refuse before
// anything is sent, with the error that names the missing TLS.
func TestAuthenticateChannelBindingRefusedWithoutTLS(t *testing.T) {
	tests := []struct {
		name         string
		capabilities string
		opts         *AuthenticateOptions
	}{
		{"explicit", "AUTH=SCRAM-SHA-256-PLUS AUTH=SCRAM-SHA-256", &AuthenticateOptions{Mechanism: "SCRAM-SHA-256-PLUS"}},
		{"only PLUS advertised", "AUTH=SCRAM-SHA-256-PLUS AUTH=SCRAM-SHA-1-PLUS", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			line, err := authenticateFirstLine(t, test.capabilities, test.opts)
			if line != "" {
				t.Fatalf("sent %q, want nothing", line)
			}
			if err == nil || !strings.Contains(err.Error(), "requires a TLS connection") {
				t.Fatalf("Authenticate() = %v, want the missing-TLS refusal", err)
			}
		})
	}
}

// TestAuthenticatePrefersChannelBindingOverTLS pins the other side: where TLS
// 1.3 provides the tls-exporter binding, automatic selection keeps the -PLUS
// variant first.
func TestAuthenticatePrefersChannelBindingOverTLS(t *testing.T) {
	certificate := testCertificate(t)
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	clientConn, serverConn := net.Pipe()
	server := tls.Server(serverConn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13})
	client := tls.Client(clientConn, &tls.Config{RootCAs: roots, ServerName: "mail.example.test", MinVersion: tls.VersionTLS13})

	line, err := authenticateFirstLineOver(t, client, server, "AUTH=SCRAM-SHA-256-PLUS AUTH=SCRAM-SHA-256", nil)
	if line == "" {
		t.Fatalf("Authenticate() sent nothing: %v", err)
	}
	mechanism, payload := sentMechanism(t, line)
	if mechanism != "SCRAM-SHA-256-PLUS" || !strings.HasPrefix(payload, "p=tls-exporter,,") {
		t.Fatalf("sent %s %q, want SCRAM-SHA-256-PLUS with the tls-exporter binding", mechanism, payload)
	}
}
