package imapserver

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

type bearerCapabilityBackend struct {
	Backend
	witness func(context.Context, string, *CapabilitySupportOptions) bool
}

func (b bearerCapabilityBackend) SupportsCapability(ctx context.Context, name string, options *CapabilitySupportOptions) bool {
	return b.witness(ctx, name, options)
}

func TestBearerCapabilitiesRequireWitness(t *testing.T) {
	for _, names := range [][]string{nil, {"AUTH=XOAUTH2"}, {"AUTH=OAUTHBEARER"}, {"AUTH=XOAUTH2", "AUTH=OAUTHBEARER"}} {
		for _, secure := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/tls=%t", names, secure), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				backend := bearerCapabilityBackend{witness: func(got context.Context, name string, options *CapabilitySupportOptions) bool {
					if got != ctx || options != nil {
						t.Errorf("witness lost command context or default options")
					}
					return slices.Contains(names, name)
				}}
				server := New(backend, &Options{AllowInsecureAuth: true})
				state := newSessionState(secure)
				capabilities := deriveCapabilitiesContext(ctx, &state, server)
				for _, name := range []string{"AUTH=XOAUTH2", "AUTH=OAUTHBEARER"} {
					want := secure && slices.Contains(names, name)
					if got := slices.Contains(capabilities, name); got != want {
						t.Errorf("%s advertised=%t, want %t; %v", name, got, want, capabilities)
					}
				}
				for _, name := range []string{"AUTH=PLAIN", "AUTH=LOGIN", "SASL-IR"} {
					if !slices.Contains(capabilities, name) {
						t.Errorf("password authentication lost %s: %v", name, capabilities)
					}
				}
				state.state = stateAuthenticated
				capabilities = deriveCapabilitiesContext(ctx, &state, server)
				for _, name := range []string{"AUTH=XOAUTH2", "AUTH=OAUTHBEARER"} {
					if slices.Contains(capabilities, name) {
						t.Errorf("authenticated session advertised %s", name)
					}
				}
			})
		}
	}
	// A backend without the optional interface cannot consent to bearer SASL.
	state := newSessionState(true)
	capabilities := deriveCapabilities(&state, New(struct{ Backend }{}, nil))
	for _, name := range []string{"AUTH=XOAUTH2", "AUTH=OAUTHBEARER"} {
		if slices.Contains(capabilities, name) {
			t.Errorf("backend without CapabilitySupport advertised %s", name)
		}
	}
}
