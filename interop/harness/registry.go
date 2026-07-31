package harness

import (
	"fmt"
	"strings"

	"github.com/kiliant/go-imap/interop/definition"
	"github.com/kiliant/go-imap/interop/servers/courier"
	"github.com/kiliant/go-imap/interop/servers/cyrus"
	"github.com/kiliant/go-imap/interop/servers/dovecot"
	"github.com/kiliant/go-imap/interop/servers/greenmail"
	"github.com/kiliant/go-imap/interop/servers/stalwart"
)

// Profiles returns a fresh copy of the profiles enabled by the current build
// tags. Tier 3 is included only with interop_emulated.
func Profiles() []definition.Profile {
	profiles := []definition.Profile{
		dovecot.Profile,
		stalwart.Profile,
		greenmail.Profile,
		cyrus.Profile,
		courier.Profile,
	}
	profiles = append(profiles, emulatedProfiles...)
	return profiles
}

func validateProfile(profile definition.Profile) error {
	if profile.Name == "" {
		return fmt.Errorf("interop: profile has no name")
	}
	if strings.ContainsAny(profile.Name, " /\\\t\r\n") {
		return fmt.Errorf("interop: invalid profile name %q", profile.Name)
	}
	if (profile.Image == "") == (profile.BuildContext == "") {
		return fmt.Errorf("interop: profile %s must set exactly one of image and build context", profile.Name)
	}
	if profile.Image != "" && (strings.HasSuffix(profile.Image, ":latest") || (!strings.Contains(profile.Image, ":") && !strings.Contains(profile.Image, "@sha256:"))) {
		return fmt.Errorf("interop: profile %s image is not pinned: %q", profile.Name, profile.Image)
	}
	if profile.ContainerPort < 1 || profile.ContainerPort > 65535 {
		return fmt.Errorf("interop: profile %s has invalid container port %d", profile.Name, profile.ContainerPort)
	}
	if profile.Tier < definition.TierNativeImage || profile.Tier > definition.TierEmulated {
		return fmt.Errorf("interop: profile %s has invalid tier %d", profile.Name, profile.Tier)
	}
	return nil
}
