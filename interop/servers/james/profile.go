package james

import "github.com/kiliant/go-imap/interop/definition"

// Profile is the opt-in, amd64-emulated Apache James configuration.
//
// James's demo image refuses LOGIN on a cleartext connection ("Plain login
// / authentication are disabled"), so TLSPort points the harness at its
// implicit-TLS IMAPS listener (993) for every LOGIN, including the harness's
// own post-provisioning smoke check.
var Profile = definition.Profile{
	Name:            "james",
	Image:           "docker.io/apache/james:demo-3.8.2",
	ContainerPort:   143,
	AdditionalPorts: []int{993},
	TLSPort:         993,
	ProvisionCommands: [][]string{
		{"james-cli", "AddDomain", "example.test"},
		{"james-cli", "AddUser", "interop@example.test", "interop-pw"},
	},
	ExpectedCapabilities: []string{
		"IMAP4REV1", "IDLE", "NAMESPACE",
	},
	Tier: definition.TierEmulated,
}
