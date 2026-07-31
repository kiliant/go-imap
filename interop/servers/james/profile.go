package james

import "github.com/kiliant/go-imap/interop/definition"

// Profile is the opt-in, amd64-emulated Apache James configuration.
var Profile = definition.Profile{
	Name:          "james",
	Image:         "docker.io/apache/james:demo-3.8.2",
	ContainerPort: 143,
	ProvisionCommands: [][]string{
		{"james-cli", "AddDomain", "example.test"},
		{"james-cli", "AddUser", "interop@example.test", "interop-pw"},
	},
	ExpectedCapabilities: []string{
		"IMAP4REV1", "IDLE", "NAMESPACE",
	},
	Tier: definition.TierEmulated,
}
