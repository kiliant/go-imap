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
//
// Construct with keyed fields only; fields may be added as the harness gains
// support for additional server features.
type Profile struct {
	Name          string
	Image         string
	BuildContext  string
	ContainerPort int
	// AdditionalPorts are extra TCP listener ports the harness publishes on
	// loopback. They are available from Server.AddressForPort.
	AdditionalPorts []int
	Environment     map[string]string
	Arguments       []string
	// ProvisionCommands are argv vectors run with podman exec after the IMAP
	// greeting is live and before the server is returned to the suite.
	ProvisionCommands    [][]string
	ExpectedCapabilities []string
	// MailboxPrefix is the server's personal namespace prefix. Most profiles
	// use the empty string; Courier exposes personal folders under INBOX.
	MailboxPrefix string
	Tier          Tier
	// TLSPort is a container port (present in AdditionalPorts) that speaks
	// implicit-TLS IMAP and requires LOGIN over that instead of ContainerPort.
	// Zero means the profile accepts LOGIN in cleartext on ContainerPort, the
	// harness default. James is the one profile so far that needs this: its
	// demo image refuses LOGIN on a cleartext connection.
	TLSPort int
	_       struct{}
}
