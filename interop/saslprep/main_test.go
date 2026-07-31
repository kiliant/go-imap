//go:build interop

// Package saslprep does two things, both scoped to the two dedicated
// accounts described in saslprep_test.go:
//
//   - TestSASLprepDiagnostic is a diagnostic, not a conformance check: it
//     empirically determines whether Dovecot and Stalwart, as configured by
//     this harness, apply SASLprep/NFKC normalization to passwords at
//     enrollment or comparison time. See docs/INTEROP.md and CLAUDE.md for
//     the project's skip-vs-fail rule, which this package follows.
//   - TestPrepareCredentialsInterop is a hard-assertion conformance check
//     of imapclient.AuthenticateOptions.PrepareCredentials against the
//     production client: it does not depend on either server's
//     normalization behavior, because both stored password forms are
//     accounts this package provisions itself.
//
// It owns a small profile subset (Dovecot and Stalwart only) rather than the
// full harness registry: it is the only package that needs the
// SASLprep-discriminating accounts, and starting Cyrus/Courier/GreenMail for
// them would be pure cost.
package saslprep

import (
	"os"
	"testing"

	"github.com/kiliant/go-imap/interop/definition"
	"github.com/kiliant/go-imap/interop/harness"
	"github.com/kiliant/go-imap/interop/servers/dovecot"
	"github.com/kiliant/go-imap/interop/servers/stalwart"
)

func TestMain(m *testing.M) {
	os.Exit(harness.Run(m, []definition.Profile{dovecot.Profile, stalwart.Profile}))
}
