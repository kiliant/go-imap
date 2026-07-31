//go:build interop

package smoke

import (
	"os"
	"testing"

	"github.com/kiliant/go-imap/interop/harness"
)

func TestMain(m *testing.M) {
	os.Exit(harness.Run(m, harness.Profiles()))
}
