package courier

import "github.com/kiliant/go-imap/interop/definition"

// Profile is the locally built Courier interoperability configuration.
var Profile = definition.Profile{
	Name:          "courier",
	BuildContext:  "servers/courier",
	ContainerPort: 143,
	ExpectedCapabilities: []string{
		"IMAP4REV1", "IDLE", "NAMESPACE", "UIDPLUS",
	},
	MailboxPrefix: "INBOX.",
	Tier:          definition.TierNativeBuild,
}
