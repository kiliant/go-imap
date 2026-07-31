package stalwart

import "github.com/kiliant/go-imap/interop/definition"

// Profile is the pinned Stalwart interoperability configuration.
var Profile = definition.Profile{
	Name:            "stalwart",
	BuildContext:    "servers/stalwart",
	ContainerPort:   143,
	AdditionalPorts: []int{8080},
	ExpectedCapabilities: []string{
		"IMAP4REV1", "IMAP4REV2", "ENABLE", "IDLE", "UIDPLUS", "MOVE",
		"LIST-EXTENDED", "SPECIAL-USE", "NAMESPACE", "LITERAL+", "SASL-IR",
	},
	Tier: definition.TierNativeImage,
}
