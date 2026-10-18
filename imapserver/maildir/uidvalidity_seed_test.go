package maildir

import (
	"testing"
	"time"
)

func TestInitialUIDValidityUsesTimeSeed(t *testing.T) {
	before := time.Now().Unix()
	b := openTest(t, t.TempDir())
	uidValidity := b.accounts["alice"].state.Mailboxes["INBOX"].UIDValidity
	if int64(uidValidity) < before || int64(uidValidity) > time.Now().Unix() {
		t.Errorf("new UIDVALIDITY %d does not use the current time", uidValidity)
	}
}
