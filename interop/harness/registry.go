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

// validateNativeProfile rejects a native profile carrying container settings.
//
// Silently ignoring them would be the worse failure: a profile that sets both
// Native and Image looks configured for a container it will never start, and
// the resulting matrix row would claim to have tested something it did not.
func validateNativeProfile(profile definition.Profile) error {
	var conflicts []string
	if profile.Image != "" {
		conflicts = append(conflicts, "image")
	}
	if profile.BuildContext != "" {
		conflicts = append(conflicts, "build context")
	}
	if profile.ContainerPort != 0 {
		conflicts = append(conflicts, "container port")
	}
	if len(profile.AdditionalPorts) != 0 {
		conflicts = append(conflicts, "additional ports")
	}
	if len(profile.Environment) != 0 {
		conflicts = append(conflicts, "environment")
	}
	if len(profile.Arguments) != 0 {
		conflicts = append(conflicts, "arguments")
	}
	if len(profile.ProvisionCommands) != 0 {
		conflicts = append(conflicts, "provision commands")
	}
	if profile.TLSPort != 0 {
		conflicts = append(conflicts, "TLS port")
	}
	if len(conflicts) != 0 {
		return fmt.Errorf("interop: native profile %s must not set %s", profile.Name, strings.Join(conflicts, ", "))
	}
	if profile.Tier != definition.TierInProcess {
		return fmt.Errorf("interop: native profile %s must use TierInProcess, has tier %d", profile.Name, profile.Tier)
	}
	return nil
}

func validateProfile(profile definition.Profile) error {
	if profile.Name == "" {
		return fmt.Errorf("interop: profile has no name")
	}
	if strings.ContainsAny(profile.Name, " /\\\t\r\n") {
		return fmt.Errorf("interop: invalid profile name %q", profile.Name)
	}
	if profile.Native != nil {
		return validateNativeProfile(profile)
	}
	if (profile.Image == "") == (profile.BuildContext == "") {
		return fmt.Errorf("interop: profile %s must set exactly one of image, build context and native", profile.Name)
	}
	if profile.Image != "" && (strings.HasSuffix(profile.Image, ":latest") || (!strings.Contains(profile.Image, ":") && !strings.Contains(profile.Image, "@sha256:"))) {
		return fmt.Errorf("interop: profile %s image is not pinned: %q", profile.Name, profile.Image)
	}
	if profile.ContainerPort < 1 || profile.ContainerPort > 65535 {
		return fmt.Errorf("interop: profile %s has invalid container port %d", profile.Name, profile.ContainerPort)
	}
	ports := map[int]bool{profile.ContainerPort: true}
	for _, port := range profile.AdditionalPorts {
		if port < 1 || port > 65535 {
			return fmt.Errorf("interop: profile %s has invalid additional port %d", profile.Name, port)
		}
		if ports[port] {
			return fmt.Errorf("interop: profile %s repeats published port %d", profile.Name, port)
		}
		ports[port] = true
	}
	if profile.Tier < definition.TierNativeImage || profile.Tier > definition.TierEmulated {
		return fmt.Errorf("interop: profile %s has invalid tier %d", profile.Name, profile.Tier)
	}
	return nil
}
