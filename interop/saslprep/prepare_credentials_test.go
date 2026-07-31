//go:build interop

package saslprep

import (
	"fmt"
	"os"
	"testing"

	"github.com/kiliant/go-imap/interop/harness"
)

// prepareInteropAccount is one of the two SASLprep-discriminating accounts.
// Every probe below sends the identical client-side input password,
// saslprepPasswordRaw: what differs between accounts is only the byte
// sequence already stored on the server. That is what lets the expected
// outcome of each cell depend solely on which account is targeted and
// whether AuthenticateOptions.PrepareCredentials is set — never on any
// belief about a server's own normalization policy.
type prepareInteropAccount struct {
	// label matches the "Stored password" column of the task's 2x2 table.
	label    string
	username string
}

var prepareInteropAccounts = []prepareInteropAccount{
	{label: "raw(U+00B5)", username: saslprepUsernameRaw},
	{label: "nfkc(U+03BC)", username: saslprepUsernameNFKC},
}

// prepareInteropExpected is the 2x2 table from the task description:
//
//	stored password   PrepareCredentials=false   PrepareCredentials=true
//	raw µ (U+00B5)     authenticates              rejected
//	NFKC μ (U+03BC)    rejected                   authenticates
//
// The raw row is the regression guard: it is the project default
// (PrepareCredentials false) proving unchanged behavior against the
// raw-octet-storing servers actually in this matrix. The NFKC row is the
// only evidence that turning the option on does anything useful at all: an
// ASCII password would authenticate identically either way, which is
// exactly why this diagnostic exists instead of a password fixture that
// happens to be easy to type.
func prepareInteropExpected(accountLabel string, prepareCredentials bool) probeOutcome {
	raw := accountLabel == "raw(U+00B5)"
	if raw == prepareCredentials {
		return outcomeRejected
	}
	return outcomeAuthenticated
}

type prepareInteropCell struct {
	mechanism string
	account   string
	prepare   bool
	outcome   probeOutcome
	want      probeOutcome
}

// TestPrepareCredentialsInterop is a hard-assertion conformance check, not a
// diagnostic: unlike TestSASLprepDiagnostic, every cell here has a single
// correct outcome, because this package provisioned both the raw-octet and
// the NFKC-octet stored forms itself. A cell that comes out wrong is a bug
// in imapclient.AuthenticateOptions.PrepareCredentials (or in this harness's
// provisioning), not a fact about Dovecot or Stalwart.
func TestPrepareCredentialsInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			harness.AssertProfile(t, server.Profile, harness.CapabilitiesFor(server.Profile.Name))

			mechanisms := saslprepMechanismsFor(server.Profile.Name)
			if len(mechanisms) == 0 {
				t.Skipf("no SASLprep diagnostic accounts are provisioned on %s", server.Profile.Name)
			}

			var results []prepareInteropCell
			for _, mechanism := range mechanisms {
				mechanism := mechanism
				for _, account := range prepareInteropAccounts {
					account := account
					for _, prepare := range []bool{false, true} {
						prepare := prepare
						cell := prepareInteropCell{
							mechanism: mechanism,
							account:   account.label,
							prepare:   prepare,
							want:      prepareInteropExpected(account.label, prepare),
						}
						name := fmt.Sprintf("%s/%s/PrepareCredentials=%t", mechanism, account.label, prepare)
						t.Run(name, func(t *testing.T) {
							outcome := authenticateProbe(t, server, mechanism, account.username, saslprepPasswordRaw, prepare)
							cell.outcome = outcome
							if outcome == outcomeSkipped {
								// Stalwart advertises only AUTH=PLAIN and
								// AUTH=OAUTHBEARER: its SCRAM-SHA-256 rows
								// must skip, gated on the live capability
								// set, exactly like TestSASLprepDiagnostic.
								// This is never a failure and is still
								// recorded in the report below.
								t.Skipf("%s does not advertise AUTH=%s", server.Profile.Name, mechanism)
								return
							}
							if outcome != cell.want {
								t.Errorf("%s %s account=%s PrepareCredentials=%t: got %s, want %s",
									server.Profile.Name, mechanism, account.label, prepare, outcome, cell.want)
							}
						})
						results = append(results, cell)
					}
				}
			}

			report := formatPrepareInteropMatrix(results)
			// Same convention as TestSASLprepDiagnostic: a passing,
			// non-verbose run discards this output, so run with -v (or on
			// failure) to see the matrix.
			t.Logf("\nPrepareCredentials interop matrix (%s):\n%s", server.Profile.Name, report)
			fmt.Fprintf(os.Stderr, "\nPrepareCredentials interop matrix (%s):\n%s", server.Profile.Name, report)
		})
	}
}

func formatPrepareInteropMatrix(results []prepareInteropCell) string {
	var b []byte
	b = fmt.Appendf(b, "  %-16s %-14s %-20s %-15s %s\n", "mechanism", "stored", "PrepareCredentials", "outcome", "want")
	for _, r := range results {
		b = fmt.Appendf(b, "  %-16s %-14s %-20t %-15s %s\n", r.mechanism, r.account, r.prepare, r.outcome.String(), r.want.String())
	}
	return string(b)
}
