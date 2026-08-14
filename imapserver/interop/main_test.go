//go:build interop

package interop_test

import (
	"os"
	"testing"

	serverinterop "github.com/kiliant/go-imap/imapserver/interop"
	"github.com/kiliant/go-imap/interop/definition"
	"github.com/kiliant/go-imap/interop/harness"
)

// The profile is passed in rather than discovered. harness.Profiles() is the
// root module's container set, and this entry is deliberately not in it: the
// registry importing this package is exactly the module cycle the package
// documentation explains.
func TestMain(m *testing.M) {
	os.Exit(harness.Run(m, []definition.Profile{serverinterop.Profile}))
}
