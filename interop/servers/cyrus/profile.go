package cyrus

import "github.com/kiliant/go-imap/interop/definition"

// Profile is the locally built Cyrus interoperability configuration.
var Profile = definition.Profile{
	Name:          "cyrus",
	BuildContext:  "servers/cyrus",
	ContainerPort: 143,
	ExpectedCapabilities: []string{
		"IMAP4REV1", "IDLE", "NAMESPACE", "UIDPLUS", "LITERAL+", "ACL",
	},
	Tier: definition.TierNativeBuild,
}
