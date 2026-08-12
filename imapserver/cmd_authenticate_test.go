package imapserver

import (
	"slices"
	"testing"
)

func TestSecureDefaultAuthenticationCapabilitiesAreTruthful(t *testing.T) {
	server := New(moveSupportBackend{}, nil)
	state := newSessionState(false)
	capabilities := deriveCapabilities(&state, server)
	if !slices.Contains(capabilities, "LOGINDISABLED") {
		t.Fatalf("secure default omitted LOGINDISABLED: %v", capabilities)
	}
	if slices.Contains(capabilities, "AUTH=PLAIN") || slices.Contains(capabilities, "AUTH=LOGIN") {
		t.Fatalf("secure default advertised cleartext authentication: %v", capabilities)
	}

	server = New(moveSupportBackend{}, &Options{AllowInsecureAuth: true})
	capabilities = deriveCapabilities(&state, server)
	if slices.Contains(capabilities, "LOGINDISABLED") {
		t.Fatalf("explicit insecure authentication still advertised LOGINDISABLED: %v", capabilities)
	}
	if !slices.Contains(capabilities, "AUTH=PLAIN") || !slices.Contains(capabilities, "AUTH=LOGIN") {
		t.Fatalf("explicit insecure authentication omitted mechanisms: %v", capabilities)
	}
}
