package dovecot

import "github.com/kiliant/go-imap/interop/definition"

// Profile is the pinned Dovecot interoperability configuration.
var Profile = definition.Profile{
	Name:          "dovecot",
	BuildContext:  "servers/dovecot",
	ContainerPort: 31143,
	Environment: map[string]string{
		"USER_PASSWORD": "{PLAIN}interop-pw",
	},
	ExpectedCapabilities: []string{
		"IMAP4REV1", "SASL-IR", "IDLE", "ENABLE", "ID", "LITERAL+",
	},
	Tier: definition.TierNativeBuild,
}
