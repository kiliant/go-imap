//go:build interop_emulated

package harness

import (
	"github.com/kiliant/go-imap/interop/definition"
	"github.com/kiliant/go-imap/interop/servers/james"
)

var emulatedProfiles = []definition.Profile{james.Profile}
