package imapserver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

type scramCredentialBackend struct{ Backend }

func (scramCredentialBackend) SCRAMCredentials(context.Context, string, string, *SCRAMCredentialsOptions) (*SCRAMStoredCredentials, error) {
	return nil, nil
}

type scramCapabilityBackend struct {
	scramCredentialBackend
	witness func(context.Context, string, *CapabilitySupportOptions) bool
}

func (b scramCapabilityBackend) SupportsCapability(ctx context.Context, name string, options *CapabilitySupportOptions) bool {
	return b.witness(ctx, name, options)
}

func TestSCRAMCapabilitiesRequirePerMechanismWitness(t *testing.T) {
	names := []string{"AUTH=SCRAM-SHA-1", "AUTH=SCRAM-SHA-256", "AUTH=SCRAM-SHA-1-PLUS", "AUTH=SCRAM-SHA-256-PLUS"}
	for mask := 0; mask < 1<<len(names); mask++ {
		for _, secure := range []bool{false, true} {
			t.Run(fmt.Sprintf("mask=%d/tls=%t", mask, secure), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				backend := scramCapabilityBackend{witness: func(got context.Context, name string, options *CapabilitySupportOptions) bool {
					if got != ctx || options != nil {
						t.Error("witness lost connection context or default options")
					}
					i := slices.Index(names, name)
					return i >= 0 && mask&(1<<i) != 0
				}}
				server := New(backend, nil)
				state := newSessionState(secure)
				capabilities := deriveCapabilitiesContext(ctx, &state, server)
				for i, name := range names {
					want := mask&(1<<i) != 0 && (secure || !strings.HasSuffix(name, "-PLUS"))
					if got := slices.Contains(capabilities, name); got != want {
						t.Errorf("%s advertised=%t, want %t: %v", name, got, want, capabilities)
					}
				}
				state.state = stateAuthenticated
				capabilities = deriveCapabilitiesContext(ctx, &state, server)
				for _, name := range names {
					if slices.Contains(capabilities, name) {
						t.Errorf("authenticated session advertised %s", name)
					}
				}
			})
		}
	}
	for _, backend := range []Backend{
		scramCredentialBackend{}, // Structural support without a witness.
		bearerCapabilityBackend{witness: func(context.Context, string, *CapabilitySupportOptions) bool { return true }}, // Witness without credentials.
	} {
		state := newSessionState(true)
		for _, name := range deriveCapabilities(&state, New(backend, nil)) {
			if strings.HasPrefix(name, "AUTH=SCRAM-") {
				t.Errorf("incomplete backend %T advertised %s", backend, name)
			}
		}
	}
}

func TestSCRAMFeatureAcceptsPlusOnlyAdvertisement(t *testing.T) {
	for _, descriptor := range featureDescriptors {
		if descriptor.ID == featureSCRAM {
			for name := range scramMechanisms {
				if !descriptor.Active(nil, map[string]bool{"AUTH=" + name: true}) {
					t.Errorf("%s did not activate SCRAM options", name)
				}
			}
			if descriptor.Active(nil, nil) {
				t.Error("SCRAM active without an advertised mechanism")
			}
			return
		}
	}
	t.Fatal("SCRAM feature descriptor missing")
}
