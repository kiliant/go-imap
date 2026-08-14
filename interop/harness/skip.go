package harness

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/kiliant/go-imap/interop/definition"
)

// MissingExpectedCapabilities returns profile promises absent from the live
// server. Callers must fail, not skip, when this list is non-empty.
func MissingExpectedCapabilities(profile definition.Profile, advertised map[string]bool) []string {
	var missing []string
	for _, capability := range profile.ExpectedCapabilities {
		if !advertised[strings.ToUpper(capability)] {
			missing = append(missing, strings.ToUpper(capability))
		}
	}
	sort.Strings(missing)
	return missing
}

// AssertProfile fails when a live server does not meet its own profile.
func AssertProfile(t testing.TB, profile definition.Profile, advertised map[string]bool) {
	t.Helper()
	if missing := MissingExpectedCapabilities(profile, advertised); len(missing) != 0 {
		t.Fatalf("%s capability profile mismatch: missing %s (advertised: %s)",
			attribution(profile), strings.Join(missing, ", "), FormatCapabilities(advertised))
	}
}

// attribution names the profile and, for our own server, says whose bug a
// failure is.
//
// The distinction is the whole reason the first-party entry is worth having.
// Dovecot failing its profile nearly always means the image moved; go-imap
// failing its profile means go-imap is wrong, and a matrix that renders the two
// identically trains everyone reading it to assume the first.
func attribution(profile definition.Profile) string {
	if profile.FirstParty {
		return profile.Name + " (first-party: this is our bug, not a container problem)"
	}
	return profile.Name
}

// provisionFailure formats a startup or seeding failure with the same
// attribution AssertProfile applies to capability mismatches.
func provisionFailure(profile definition.Profile, err error) string {
	return fmt.Sprintf("interop: provision %s: %v", attribution(profile), err)
}

// RequireCapabilities skips a feature test when the live server does not
// advertise one of the capabilities needed by that test.
func RequireCapabilities(t testing.TB, advertised map[string]bool, required ...string) {
	t.Helper()
	var missing []string
	for _, capability := range required {
		capability = strings.ToUpper(capability)
		if !advertised[capability] {
			missing = append(missing, capability)
		}
	}
	if len(missing) != 0 {
		t.Skipf("server does not advertise required capability: %s", strings.Join(missing, ", "))
	}
}

// FormatCapabilities formats a stable capability table cell.
func FormatCapabilities(capabilities map[string]bool) string {
	items := make([]string, 0, len(capabilities))
	for capability, enabled := range capabilities {
		if enabled {
			items = append(items, strings.ToUpper(capability))
		}
	}
	sort.Strings(items)
	return strings.Join(items, " ")
}

// CapabilityTable formats the per-server matrix requested by the task.
func CapabilityTable(rows map[string]map[string]bool) string {
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%-12s %s\n", name, FormatCapabilities(rows[name]))
	}
	return b.String()
}
