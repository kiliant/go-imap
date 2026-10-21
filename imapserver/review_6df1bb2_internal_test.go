package imapserver

import (
	"testing"

	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestReviewHSearchSavedUIDCapabilityGate(t *testing.T) {
	// SEARCHRES is framework-owned and cannot be withdrawn by a backend
	// filter. Its explicit gate must nevertheless apply to the UID $ alias.
	for _, wire := range []string{"$", "UID $"} {
		criteria, err := imapcodec.ReadSearchCriteria(imapwire.NewDecoderString(wire+"\r\n", nil))
		if err != nil {
			t.Fatal(err)
		}
		gate, ok := criterionCapability(criteria)
		if !ok || gate(nil, nil) || !gate(nil, map[string]bool{"SEARCHRES": true}) {
			t.Fatalf("%s lost its SEARCHRES gate", wire)
		}
	}
}
