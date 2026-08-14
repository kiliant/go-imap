//go:build interop

package interop

// mbsync (isync) driven against imapserver+memory. See client_test.go for why
// third-party clients are a separate shape from the interop matrix.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const mbsyncImage = "go-imap-mbsync:local"

// mbsyncConfig is a channel between our server and a local Maildir. Far/Near is
// the isync 1.4 spelling; bookworm ships 1.4.4.
//
// There is deliberately no TLSType line. The profile's server is the same
// cleartext listener every other entry in the matrix is measured on — it sets
// no TLSConfig — and Debian's isync is built without TLS support, so the
// keyword is not merely unnecessary but unrecognised. Transport is not what
// this test is about; synchronisation behaviour is.
func mbsyncConfig(port string) string {
	return fmt.Sprintf(`IMAPAccount goimap
Host %s
Port %s
User %s
Pass %s
AuthMechs LOGIN

IMAPStore goimap-remote
Account goimap

MaildirStore goimap-local
Path /maildir/
Inbox /maildir/INBOX
SubFolders Verbatim

Channel goimap
Far :goimap-remote:
Near :goimap-local:
Patterns *
Create Both
Expunge Both
SyncState *
`, containerHost, port, interopUser, interopPassword)
}

// TestMbsyncFullSyncAndResync is T24's "at least one real third-party client
// completes a full sync/resync cycle" acceptance criterion.
//
// The resync is the half that matters. A first pass only proves the server can
// be read; the second pass proves the server reported UIDVALIDITY, UIDNEXT and
// per-message UIDs consistently enough that a synchroniser recognises its own
// prior state instead of re-downloading, which is the bug class that makes a
// server unusable with real clients while every unit test still passes.
func TestMbsyncFullSyncAndResync(t *testing.T) {
	buildImage(t, mbsyncImage, "testdata/mbsync")
	port := serverForClients(t)
	const seeded = 12
	seed(t, port, "INBOX", seeded)

	// mbsync talks to the server through a recorder, so its sessions can seed
	// the fuzz corpus with traffic no one on this project wrote. See
	// capture_test.go; without GOIMAP_CAPTURE_CORPUS this only forwards.
	recorded := newRecorder(t, port)
	port = recorded.port()
	defer recorded.writeCorpus(t, "mbsync")

	// Both passes run in one container so the second sees the first's Maildir
	// and sync state — that state is the whole point of the resync.
	script := fmt.Sprintf(`set -e
mkdir -p /maildir/INBOX
cat > /tmp/mbsyncrc <<'MBSYNCEOF'
%s
MBSYNCEOF
echo "=== pass 1: full sync ==="
mbsync -c /tmp/mbsyncrc -V -a
echo "=== after pass 1 ==="
ls -1 /maildir/INBOX/cur /maildir/INBOX/new 2>/dev/null | grep -c . || true
echo "COUNT1=$(find /maildir/INBOX/cur /maildir/INBOX/new -type f 2>/dev/null | wc -l | tr -d ' ')"
echo "=== pass 2: resync ==="
mbsync -c /tmp/mbsyncrc -V -a
echo "COUNT2=$(find /maildir/INBOX/cur /maildir/INBOX/new -type f 2>/dev/null | wc -l | tr -d ' ')"
echo "=== done ==="
`, mbsyncConfig(port))

	out, err := runInImage(t, mbsyncImage, script, 5*time.Minute)
	t.Logf("mbsync output:\n%s", out)
	if err != nil {
		t.Fatalf("mbsync did not complete a sync/resync cycle against our server: %v", err)
	}

	// Both passes must see every seeded message. The counts being equal is what
	// rules out the resync having re-downloaded into duplicates, and equal to
	// the seed count is what rules out both passes having quietly fetched
	// nothing at all.
	first := extractCount(t, out, "COUNT1")
	second := extractCount(t, out, "COUNT2")
	if first != seeded {
		t.Errorf("mbsync fetched %d of %d seeded messages on the first pass", first, seeded)
	}
	if second != first {
		t.Errorf("resync changed the mailbox: %d messages after pass 1, %d after pass 2", first, second)
	}
	// mbsync reports recoverable protocol trouble on stderr while still
	// exiting zero, so a clean exit code is not on its own a clean run.
	for _, line := range strings.Split(out, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "malformed") ||
			strings.Contains(lower, "unexpected") {
			t.Errorf("mbsync reported a protocol problem: %q", strings.TrimSpace(line))
		}
	}
}

func extractCount(t *testing.T, out, key string) int {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, key+"="); ok {
			var value int
			if _, err := fmt.Sscanf(after, "%d", &value); err != nil {
				t.Fatalf("parsing %s from %q: %v", key, line, err)
			}
			return value
		}
	}
	t.Fatalf("%s not reported by the container script", key)
	return 0
}
