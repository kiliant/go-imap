//go:build interop

package saslprep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

// saslprepUsernameRaw is a dedicated account, distinct from
// interop@example.test, so this diagnostic cannot interfere with any other
// interop test. Its password is stored as the exact raw bytes of
// saslprepPasswordRaw.
//
// saslprepUsernameNFKC is a second dedicated account whose password is
// instead stored as the exact bytes of saslprepPasswordNFKC — the NFKC form.
// It emulates a server that applies SASLprep at enrollment, the deployment
// RFC 5802 assumes and which neither Dovecot nor Stalwart is by default (see
// the diagnostic matrix below). Its existence, alongside the raw account, is
// what turns AuthenticateOptions.PrepareCredentials from something that can
// only be reasoned about into something that can be asserted end to end:
// each account authenticates only with the password form it actually
// stores, so the client option's effect — not any server's normalization
// policy — is what a probe result depends on.
const (
	saslprepUsernameRaw  = "interop-prep@example.test"
	saslprepUsernameNFKC = "interop-prep-nfkc@example.test"
)

// The password is configured on the server with U+00B5 MICRO SIGN. RFC 5802
// SCRAM mandates SASLprep, i.e. NFKC, normalization of the password; NFKC
// maps U+00B5 to U+03BC GREEK SMALL LETTER MU. These two constants are the
// minimal pair that discriminates whether a server normalizes at enrollment:
//
//   - raw form: interop/servers/{dovecot,stalwart} store this exact byte
//     sequence, unmodified, under saslprepUsernameRaw. If it authenticates,
//     the server compares (or derives SCRAM keys from) the byte sequence it
//     was given.
//   - NFKC form: what a SASLprep-compliant client would send instead. Both
//     servers also store this exact byte sequence, unmodified, under
//     saslprepUsernameNFKC — a second account, provisioned deliberately so
//     this form has an account it is expected to authenticate against
//     rather than only ever being an expected rejection.
//
// Unicode escapes are used instead of the literal glyphs so no editor,
// linter, or future refactor can silently normalize the source file out
// from under this test.
const (
	saslprepPasswordRaw  = "interop-pw-\u00b5" // MICRO SIGN
	saslprepPasswordNFKC = "interop-pw-\u03bc" // GREEK SMALL LETTER MU
)

func init() {
	if saslprepPasswordRaw == saslprepPasswordNFKC {
		panic("saslprep: raw and NFKC probe passwords must differ byte-for-byte")
	}
}

// probeOutcome classifies one authentication attempt.
type probeOutcome int

const (
	outcomeSkipped probeOutcome = iota
	outcomeAuthenticated
	outcomeRejected
	outcomeClientRefused
)

func (o probeOutcome) String() string {
	switch o {
	case outcomeAuthenticated:
		return "AUTHENTICATED"
	case outcomeRejected:
		return "REJECTED"
	case outcomeClientRefused:
		return "CLIENT-REFUSED"
	default:
		return "SKIPPED"
	}
}

// saslprepForm is one of the two password encodings under test.
type saslprepForm struct {
	label    string
	password string
	// meaning describes what a successful authentication with this form
	// implies about the server, echoing the table in the task description.
	meaning string
}

var saslprepForms = []saslprepForm{
	{label: "raw(U+00B5)", password: saslprepPasswordRaw, meaning: "server compares/derives from raw bytes"},
	{label: "nfkc(U+03BC)", password: saslprepPasswordNFKC, meaning: "server normalized (SASLprep) at enrollment"},
}

// saslprepMechanismsFor returns the mechanisms this diagnostic probes for a
// given server profile. Both Dovecot and Stalwart carry the second account;
// every other profile in the registry does not, and is skipped outright.
func saslprepMechanismsFor(profile string) []string {
	switch profile {
	case "dovecot", "stalwart":
		return []string{"PLAIN", "SCRAM-SHA-256"}
	default:
		return nil
	}
}

type saslprepCell struct {
	mechanism string
	form      string
	outcome   probeOutcome
	meaning   string
}

// TestSASLprepDiagnostic is a probe, not a conformance check: it reports
// which of {raw byte, NFKC-normalized} password forms authenticate against
// each mechanism a live server advertises. It never asserts that one server
// behavior is correct — RFC 5802 mandates SASLprep, but this project is not
// willing to add client-side normalization on the strength of the RFC text
// alone (see CLAUDE.md). It fails only when a single mechanism accepts
// neither form, which cannot be a normalization signal: the account was
// provisioned with the raw form, so at least one of the two must succeed
// unless the account itself is broken.
func TestSASLprepDiagnostic(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			harness.AssertProfile(t, server.Profile, harness.CapabilitiesFor(server.Profile.Name))

			mechanisms := saslprepMechanismsFor(server.Profile.Name)
			if len(mechanisms) == 0 {
				t.Skipf("no SASLprep diagnostic account is provisioned on %s", server.Profile.Name)
			}

			var results []saslprepCell
			for _, mechanism := range mechanisms {
				mechanism := mechanism
				var attempted, authenticated, rejected int
				for _, form := range saslprepForms {
					form := form
					cell := saslprepCell{mechanism: mechanism, form: form.label}
					t.Run(mechanism+"/"+form.label, func(t *testing.T) {
						outcome := attemptSASLprepAuth(t, server, mechanism, form.password)
						cell.outcome = outcome
						if outcome == outcomeAuthenticated {
							cell.meaning = form.meaning
						}
						if outcome == outcomeSkipped {
							t.Skipf("%s does not advertise AUTH=%s", server.Profile.Name, mechanism)
						}
					})
					results = append(results, cell)
					switch cell.outcome {
					case outcomeAuthenticated:
						attempted++
						authenticated++
					case outcomeRejected:
						attempted++
						rejected++
					case outcomeClientRefused:
						attempted++
					}
				}
				if attempted > 0 && authenticated == 0 && rejected == attempted {
					t.Errorf("%s %s: neither the raw nor the NFKC password form authenticated; "+
						"this is not a normalization signal, it means the %s account is not "+
						"provisioned correctly for this mechanism", server.Profile.Name, mechanism, saslprepUsernameRaw)
				}
			}

			report := formatSASLprepMatrix(results)
			// `go test` buffers a passing, non-verbose run's output (both
			// t.Logf and raw os.Stderr writes) and discards it, exactly like
			// harness.Run's own capability table below. Run with -v (or on a
			// failure) to see the matrix; that is the existing convention
			// for diagnostic output in this package, not a gap specific to
			// this test.
			t.Logf("\nSASLprep diagnostic matrix (%s):\n%s", server.Profile.Name, report)
			fmt.Fprintf(os.Stderr, "\nSASLprep diagnostic matrix (%s):\n%s", server.Profile.Name, report)
		})
	}
}

// attemptSASLprepAuth runs one AUTHENTICATE attempt against saslprepUsernameRaw
// on a fresh connection, without PrepareCredentials, and classifies the
// result. Dial failures are hard test failures: they indicate a broken
// harness, not a data point about normalization.
func attemptSASLprepAuth(t *testing.T, server *harness.Server, mechanism, password string) probeOutcome {
	t.Helper()
	return authenticateProbe(t, server, mechanism, saslprepUsernameRaw, password, false)
}

// authenticateProbe runs one AUTHENTICATE attempt on a fresh connection and
// classifies the result. It is the shared foundation for both the
// diagnostic matrix (TestSASLprepDiagnostic, always PrepareCredentials
// false) and the hard-assertion matrix (TestPrepareCredentialsInterop, both
// settings). Dial failures are hard test failures: they indicate a broken
// harness, not a data point about normalization or about the option.
func authenticateProbe(t *testing.T, server *harness.Server, mechanism, username, password string, prepareCredentials bool) probeOutcome {
	t.Helper()
	// The Dovecot base image applies an escalating per-IP delay after each
	// authentication failure; both matrices in this package deliberately
	// send failing attempts, so probes need a generous budget rather than
	// the harness's usual 15s. Probes are run sequentially (no t.Parallel),
	// so the delays this induces do not interleave across mechanisms.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	trace := new(authTrace)
	options := &imapclient.Options{Trace: trace.Add}
	if mechanism == "PLAIN" {
		// The harness exposes these listeners without TLS; this is a
		// deliberate test-only opt-in, matching imapclient/auth_interop_test.go.
		options.AllowInsecureAuth = true
	}
	client, err := imapclient.Dial(ctx, server.Address, options)
	if err != nil {
		server.LogDiagnostics(context.Background(), t, trace)
		t.Fatalf("dial %s: %v", server.Profile.Name, err)
	}
	defer client.Close()

	if !client.Capabilities()["AUTH="+mechanism] {
		return outcomeSkipped
	}

	err = client.Authenticate(ctx, username, password, &imapclient.AuthenticateOptions{
		Mechanism:          mechanism,
		PrepareCredentials: prepareCredentials,
	})
	switch {
	case err == nil:
		if logoutErr := client.Logout(ctx, nil); logoutErr != nil {
			t.Logf("logout after successful %s authentication: %v", mechanism, logoutErr)
		}
		return outcomeAuthenticated
	default:
		var ierr *imap.Error
		if errors.As(err, &ierr) && ierr.Type == imap.ErrorTypeNo {
			return outcomeRejected
		}
		// Anything else (protocol errors, network errors, an unexpected BAD)
		// is not a clean credential rejection. Surface it plainly, but do
		// not let it participate in the "neither form worked" check: it is
		// not evidence about normalization one way or the other.
		server.LogDiagnostics(context.Background(), t, trace)
		t.Logf("%s %s authentication (user=%s, PrepareCredentials=%t) returned an unexpected error (not a clean rejection): %v", server.Profile.Name, mechanism, username, prepareCredentials, err)
		return outcomeClientRefused
	}
}

func formatSASLprepMatrix(results []saslprepCell) string {
	var b []byte
	b = fmt.Appendf(b, "  %-16s %-14s %-15s %s\n", "mechanism", "form", "outcome", "meaning if authenticated")
	for _, r := range results {
		b = fmt.Appendf(b, "  %-16s %-14s %-15s %s\n", r.mechanism, r.form, r.outcome.String(), r.meaning)
	}
	return string(b)
}

// authTrace is a minimal, credential-free wire trace for diagnostics, mirroring
// the pattern in imapclient/auth_interop_test.go. Add runs on the client's
// reader goroutine while String is read from the test goroutine (via
// Server.DumpDiagnostics), so both need the mutex.
type authTrace struct {
	mu      sync.Mutex
	entries []string
}

func (trace *authTrace) Add(event imapclient.TraceEvent) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	trace.entries = append(trace.entries, fmt.Sprintf("%s %s", event.Direction, event.Data))
}

func (trace *authTrace) String() string {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	var b []byte
	for _, entry := range trace.entries {
		b = fmt.Appendf(b, "%s\n", entry)
	}
	return string(b)
}
