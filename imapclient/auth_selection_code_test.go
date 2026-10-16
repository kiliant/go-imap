package imapclient

import (
	"errors"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestAuthSelectionRefusalCode(t *testing.T) {
	for _, tc := range []struct {
		name, caps string
		opts       *AuthenticateOptions
		wantCode   bool
	}{
		{"default", "AUTH=GSSAPI", nil, true},
		{"password", "AUTH=OAUTHBEARER AUTH=XOAUTH2", &AuthenticateOptions{CredentialKind: CredentialPassword}, true},
		{"bearer", "AUTH=PLAIN", &AuthenticateOptions{CredentialKind: CredentialBearerToken}, true},
		{"explicit-unadvertised", "AUTH=PLAIN", &AuthenticateOptions{Mechanism: "CRAM-MD5"}, false},
		{"explicit-unsupported", "AUTH=GSSAPI", &AuthenticateOptions{Mechanism: "GSSAPI"}, false},
		{"wrong-kind", "AUTH=PLAIN", &AuthenticateOptions{CredentialKind: "future"}, false},
		{"kind-conflict", "AUTH=XOAUTH2", &AuthenticateOptions{CredentialKind: CredentialPassword, Mechanism: "XOAUTH2"}, false},
		{"token-conflict", "AUTH=PLAIN", &AuthenticateOptions{CredentialKind: CredentialPassword, Token: "secret"}, false},
		{"binding", "AUTH=SCRAM-SHA-256-PLUS", &AuthenticateOptions{CredentialKind: CredentialPassword}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, err := authenticateFirstLine(t, tc.caps, tc.opts)
			var protocol *imap.Error
			if line != "" || !errors.As(err, &protocol) || protocol.Type != imap.ErrorTypeProtocol {
				t.Fatalf("local refusal = %q, %v", line, err)
			}
			if got := protocol.Code == CodeNoCompatibleAuthMechanism; got != tc.wantCode {
				t.Fatalf("code = %q, want dedicated refusal %t", protocol.Code, tc.wantCode)
			}
		})
	}
}
