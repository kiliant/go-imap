package greenmail

import "github.com/kiliant/go-imap/interop/definition"

// Profile is the pinned GreenMail interoperability configuration.
var Profile = definition.Profile{
	Name:          "greenmail",
	Image:         "docker.io/greenmail/standalone:2.1.9",
	ContainerPort: 3143,
	Environment: map[string]string{
		"GREENMAIL_OPTS": "-Dgreenmail.setup.test.imap -Dgreenmail.hostname=0.0.0.0 -Dgreenmail.users=interop@example.test:interop-pw",
	},
	ExpectedCapabilities: []string{"IMAP4REV1"},
	Tier:                 definition.TierNativeImage,
}
