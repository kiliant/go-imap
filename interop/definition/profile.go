// Package definition contains the declarative part of an interoperability
// server profile.  It is deliberately separate from harness so server profile
// packages can be imported by the harness registry without an import cycle.
package definition

// Tier describes how expensive a server is to run in the interoperability
// matrix.
type Tier uint8

const (
	TierNativeImage Tier = 1
	TierNativeBuild Tier = 2
	TierEmulated    Tier = 3
)

// Profile describes one server in the interoperability matrix.
// Exactly one of Image and BuildContext must be set.
type Profile struct {
	Name          string
	Image         string
	BuildContext  string
	ContainerPort int
	Environment   map[string]string
	Arguments     []string
	// ProvisionCommands are argv vectors run with podman exec after the IMAP
	// greeting is live and before the server is returned to the suite.
	ProvisionCommands    [][]string
	ExpectedCapabilities []string
	// MailboxPrefix is the server's personal namespace prefix. Most profiles
	// use the empty string; Courier exposes personal folders under INBOX.
	MailboxPrefix string
	Tier          Tier
}
