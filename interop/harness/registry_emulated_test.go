//go:build interop_emulated

package harness

import "testing"

func TestEmulatedProfileIncluded(t *testing.T) {
	for _, profile := range Profiles() {
		if profile.Name == "james" {
			return
		}
	}
	t.Fatal("interop_emulated did not enable James")
}
