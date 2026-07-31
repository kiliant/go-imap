package harness

import (
	"io"
	"strings"
	"testing"

	"github.com/kiliant/go-imap/interop/definition"
)

func TestProfilesAreValidAndPinned(t *testing.T) {
	profiles := Profiles()
	if len(profiles) < 5 {
		t.Fatalf("got %d native profiles, want at least 5", len(profiles))
	}
	seen := make(map[string]bool)
	for _, profile := range profiles {
		if err := validateProfile(profile); err != nil {
			t.Errorf("%s: %v", profile.Name, err)
		}
		if seen[profile.Name] {
			t.Errorf("duplicate profile %q", profile.Name)
		}
		seen[profile.Name] = true
	}
}

func TestValidateProfileRejectsLatest(t *testing.T) {
	profile := definition.Profile{Name: "moving", Image: "example.test/imap:latest", ContainerPort: 143, Tier: 1}
	if err := validateProfile(profile); err == nil {
		t.Fatal(":latest profile accepted")
	}
}

func TestValidateProfileAdditionalPorts(t *testing.T) {
	profile := definition.Profile{
		Name:            "multiple-listeners",
		Image:           "example.test/imap:v1",
		ContainerPort:   143,
		AdditionalPorts: []int{8080},
		Tier:            definition.TierNativeImage,
	}
	if err := validateProfile(profile); err != nil {
		t.Fatalf("valid additional port rejected: %v", err)
	}
	profile.AdditionalPorts = []int{143}
	if err := validateProfile(profile); err == nil {
		t.Fatal("primary port accepted as an additional port")
	}
	profile.AdditionalPorts = []int{8080, 8080}
	if err := validateProfile(profile); err == nil {
		t.Fatal("duplicate additional port accepted")
	}
	profile.AdditionalPorts = []int{70000}
	if err := validateProfile(profile); err == nil {
		t.Fatal("out-of-range additional port accepted")
	}
}

func TestServerAddressForPort(t *testing.T) {
	server := &Server{
		Profile: definition.Profile{ContainerPort: 143},
		Address: "127.0.0.1:1143",
		additionalAddresses: map[int]string{
			8080: "127.0.0.1:18080",
		},
	}
	if got, ok := server.AddressForPort(143); !ok || got != "127.0.0.1:1143" {
		t.Fatalf("primary address = %q, %t", got, ok)
	}
	if got, ok := server.AddressForPort(8080); !ok || got != "127.0.0.1:18080" {
		t.Fatalf("additional address = %q, %t", got, ok)
	}
	if got, ok := server.AddressForPort(993); ok || got != "" {
		t.Fatalf("unpublished address = %q, %t", got, ok)
	}
}

func TestFixturesAreRepeatableAndComplete(t *testing.T) {
	fixtures := Fixtures()
	if len(fixtures) != 10 {
		t.Fatalf("got %d messages, want 10", len(fixtures))
	}
	var seen, flagged, large int
	for _, fixture := range fixtures {
		for _, flag := range fixture.Flags {
			switch flag {
			case "\\Seen":
				seen++
			case "\\Flagged":
				flagged++
			}
		}
		if fixture.Size >= 5<<20 {
			large++
		}
		for attempt := 0; attempt < 2; attempt++ {
			n, err := io.Copy(io.Discard, fixture.Open())
			if err != nil {
				t.Fatalf("%s: %v", fixture.Name, err)
			}
			if n != fixture.Size {
				t.Fatalf("%s: reader produced %d bytes, declared %d", fixture.Name, n, fixture.Size)
			}
		}
	}
	if seen != 1 || flagged != 1 || large != 1 {
		t.Fatalf("fixture roles: seen=%d flagged=%d large=%d", seen, flagged, large)
	}
}

func TestProfileMismatchIsDistinctFromFeatureGate(t *testing.T) {
	profile := definition.Profile{ExpectedCapabilities: []string{"IMAP4REV1", "CONDSTORE"}}
	advertised := map[string]bool{"IMAP4REV1": true}
	missing := MissingExpectedCapabilities(profile, advertised)
	if got := strings.Join(missing, ","); got != "CONDSTORE" {
		t.Fatalf("missing = %q", got)
	}
	// The live set remains authoritative for feature gates: no capability is
	// synthesized from the profile.
	if advertised["CONDSTORE"] {
		t.Fatal("profile expectation mutated the live capability set")
	}
}

func TestUniqueMailboxSanitizesAndSeparates(t *testing.T) {
	one := UniqueMailbox("Test / one")
	two := UniqueMailbox("Test / one")
	if one == two {
		t.Fatal("mailbox namespaces collided")
	}
	if strings.ContainsAny(one, " /\\") {
		t.Fatalf("unsafe mailbox name %q", one)
	}
}

func TestSelectProfiles(t *testing.T) {
	profiles := Profiles()
	selected, err := selectProfiles(profiles, "greenmail,dovecot")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || selected[0].Name != "dovecot" || selected[1].Name != "greenmail" {
		t.Fatalf("unexpected selection: %v", selected)
	}
	if _, err := selectProfiles(profiles, "missing"); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestExtractContainerIDIgnoresPullProgress(t *testing.T) {
	const id = "2807f71c9e33d6911de16973e1a2be16ae9ce3aa92c7fe594a61abd5f6080f15"
	got, err := extractContainerID("Trying to pull image...\nCopying blob sha256:abc\n" + id + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != id {
		t.Fatalf("ID = %q, want %q", got, id)
	}
	if _, err := extractContainerID("pull failed"); err == nil {
		t.Fatal("missing ID accepted")
	}
}
