package imapserver

import (
	"errors"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestSelectedMessageLimitSurvivesLaterUpdates(t *testing.T) {
	selected := &selectedState{
		uids:     []imap.UID{1},
		revision: "r1",
		maxUIDs:  1,
	}
	_, err := selected.applyBatch(&UpdateBatch{
		Before:  "r1",
		After:   "r2",
		Changes: []Update{&UpdateAdd{UIDs: []imap.UID{2}}},
	}, updateAccounting{})
	if !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("selected message overflow = %v", err)
	}
	if len(selected.uids) != 1 || selected.uids[0] != 1 {
		t.Fatalf("overflow changed selected UID map: %v", selected.uids)
	}
}
