// Package definition contains the declarative part of an interoperability
// server profile.  It is deliberately separate from harness so server profile
// packages can be imported by the harness registry without an import cycle.
package definition

import "context"

// Tier describes how expensive a server is to run in the interoperability
// matrix.
type Tier uint8

const (
	TierNativeImage Tier = 1
	TierNativeBuild Tier = 2
	TierEmulated    Tier = 3
	// TierInProcess is a server the harness runs inside the test process, with
	// no container at all. It is by far the cheapest tier despite sorting last:
	// the constant numbers say how the harness obtains a server, not what it
	// costs, and nothing orders by Tier today. Renumbering the other three to
	// restore a cost ordering would change values already recorded in profiles
	// for no behavioural gain.
	TierInProcess Tier = 4
)

// NativeServer is a server started inside the test process by [Profile.Native].
//
// It is a struct of callbacks rather than an interface so that a later addition
// — a diagnostics hook, a restart — is a new field instead of a new method on
// something outside this package to implement.
//
// Construct with keyed fields only; fields may be added in a future release.
type NativeServer struct {
	// Address is the host:port its listener accepted on. Because the harness
	// asks for port 0, this is knowable only after the listener exists, which
	// is why Native returns it rather than the profile declaring it.
	Address string
	// Stop shuts the listener down and releases the backend. Required.
	Stop func(context.Context) error
	// Logs returns whatever the server recorded, standing in for the container
	// path's log capture. Optional; a nil Logs reports no output.
	Logs func() string
	_    struct{}
}

// Profile describes one server in the interoperability matrix.
// Exactly one of Image, BuildContext and Native must be set.
//
// Construct with keyed fields only; fields may be added as the harness gains
// support for additional server features.
type Profile struct {
	Name string
	// Native, when set, runs the server inside the test process instead of a
	// container, and Image, BuildContext, ContainerPort, AdditionalPorts,
	// Environment, Arguments and ProvisionCommands must all be unset — every
	// one of them is an instruction to a container engine.
	//
	// This exists because the matrix has to be able to hold our own server,
	// which is a Go value in this process and not an image anyone publishes.
	Native func(context.Context) (*NativeServer, error)
	// FirstParty marks a profile this repository implements. A capability
	// assertion failing against a third-party server usually means the
	// container changed under us; against a first-party one it means our bug.
	// The harness reports the two differently rather than identically.
	FirstParty    bool
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
