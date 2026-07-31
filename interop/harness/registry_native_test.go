//go:build !interop_emulated

package harness

import "testing"

func TestEmulatedProfileExcluded(t *testing.T) {
	for _, profile := range Profiles() {
		if profile.Name == "james" {
			t.Fatal("emulated James profile enabled without interop_emulated")
		}
	}
}
