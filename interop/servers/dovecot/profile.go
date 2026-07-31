package dovecot

import "github.com/kiliant/go-imap/interop/definition"

// Profile is the pinned Dovecot interoperability configuration.
var Profile = definition.Profile{
	Name:          "dovecot",
	Image:         "docker.io/dovecot/dovecot:2.4.3",
	ContainerPort: 31143,
	Environment: map[string]string{
		"USER_PASSWORD": "{PLAIN}interop-pw",
	},
	ExpectedCapabilities: []string{
		"IMAP4REV1", "SASL-IR", "IDLE", "ENABLE", "ID", "LITERAL+",
	},
	Tier: definition.TierNativeImage,
}
