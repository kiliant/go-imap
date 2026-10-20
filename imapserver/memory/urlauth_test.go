package memory

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func urlAuthSessions(t *testing.T) (*session, *session) {
	t.Helper()
	backend := New(&Options{Users: map[string]string{"alice": "secret", "bob": "secret", "plus+user": "secret"}})
	login := func(name string) *session {
		s, err := backend.Authenticate(t.Context(), nil, &imapserver.Credentials{Mechanism: "LOGIN", Username: name, Password: "secret"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close(context.Background(), nil) })
		return s.(*session)
	}
	alice, bob := login("alice"), login("bob")
	for _, s := range []*session{alice, bob} {
		raw := "Subject: " + s.username + "\r\n\r\n" + s.username + " body"
		if _, err := s.Append(t.Context(), "INBOX", strings.NewReader(raw), nil); err != nil {
			t.Fatal(err)
		}
	}
	return alice, bob
}

func readURLAuth(t *testing.T, s *session, url string) (string, bool) {
	t.Helper()
	result, err := s.FetchURLAuth(t.Context(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		return "", false
	}
	if result.Body == nil {
		t.Fatal("resolved URL has nil body")
	}
	defer result.Body.Close()
	raw, err := io.ReadAll(result.Body)
	if err != nil || !result.HasSize || result.Size != int64(len(raw)) {
		t.Fatalf("URL body size = %d/%t, read %d bytes: %v", result.Size, result.HasSize, len(raw), err)
	}
	return string(raw), true
}

func TestURLAuthRejectsInvalidGeneration(t *testing.T) {
	alice, _ := urlAuthSessions(t)
	base := "imap://alice@example.com/INBOX/;UID=1"
	for _, raw := range []string{
		base,
		base + ";URLAUTH=",
		base + ";URLAUTH=unknown",
		base + ";URLAUTH=user+",
		base + ";URLAUTH=user+nobody",
		base + ";URLAUTH=submit+",
		base + ";URLAUTH=anonymous:internal:" + strings.Repeat("a", 64),
		base + ";URLAUTH=anonymous;EXPIRE=2999-01-01T00:00:00Z",
		base + ";EXPIRE=bad;URLAUTH=anonymous",
		base + ";EXPIRE=" + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + ";URLAUTH=anonymous",
		"imap://bob@example.com/INBOX/;UID=1;URLAUTH=anonymous",
		"imap://example.com/INBOX/;UID=1;URLAUTH=anonymous",
		"imap://alice:secret@example.com/INBOX/;UID=1;URLAUTH=anonymous",
		"imap://alice;AUTH=%00@example.com/INBOX/;UID=1;URLAUTH=anonymous",
		"https://alice@example.com/INBOX/;UID=1;URLAUTH=anonymous",
		"imap://alice@/INBOX/;UID=1;URLAUTH=anonymous",
		"imap://alice@example.com/NoSuchMailbox/;UID=1;URLAUTH=anonymous",
		"imap://alice@example.com/INBOX;UIDVALIDITY=2/;UID=1;URLAUTH=anonymous",
		"imap://alice@example.com/INBOX/;URLAUTH=anonymous",
		"imap://alice@example.com/INBOX/;UID=0;URLAUTH=anonymous",
		"imap://alice@example.com/INBOX/;UID=01;URLAUTH=anonymous",
		"imap://alice@example.com/INBOX/;UID=1junk;URLAUTH=anonymous",
		"imap://alice@example.com/INBOX/;UID=4294967296;URLAUTH=anonymous",
		base + "/;PARTIAL=0.0;URLAUTH=anonymous",
		base + "/;SECTION=1%0D%0A;URLAUTH=anonymous",
		base + ";URLAUTH=anonymous?search=ALL",
		base + ";URLAUTH=anonymous#fragment",
		base + ";URLAUTH=anonymous#",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := alice.GenerateURLAuth(t.Context(), raw, "INTERNAL", nil)
			var failure *imap.Error
			if !errors.As(err, &failure) || failure.Type != imap.ErrorTypeBad {
				t.Fatalf("invalid generation = %v", err)
			}
		})
	}
	alice.account.mu.Lock()
	defer alice.account.mu.Unlock()
	if len(alice.account.urlAuthKeys) != 0 {
		t.Fatal("invalid generation created mailbox access keys")
	}
}

func TestURLAuthAccessAndOwnerIdentity(t *testing.T) {
	alice, bob := urlAuthSessions(t)
	for _, tc := range []struct {
		access     string
		alice, bob bool
	}{
		{"anonymous", true, true}, {"authuser", true, true},
		{"user+alice", true, false}, {"user+bob", false, true},
		{"user+plus+user", false, false}, {"user+plus%2Buser", false, false},
		{"submit+alice", false, false},
	} {
		t.Run(tc.access, func(t *testing.T) {
			raw := "imap://alice@example.com/INBOX/;UID=1;URLAUTH=" + tc.access
			authorized, err := alice.GenerateURLAuth(t.Context(), raw, "internal", nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, caller := range []struct {
				s       *session
				allowed bool
			}{{alice, tc.alice}, {bob, tc.bob}} {
				body, ok := readURLAuth(t, caller.s, authorized)
				if ok != caller.allowed || ok && body != "Subject: alice\r\n\r\nalice body" {
					t.Fatalf("%s resolving %s: %q, %t", caller.s.username, tc.access, body, ok)
				}
			}
		})
	}
}

func TestURLAuthPreservesSignedBytesAndSections(t *testing.T) {
	alice, bob := urlAuthSessions(t)
	for _, tc := range []struct{ url, want string }{
		{"IMAP://ali%63e@EXAMPLE.com/inbox/;uId=1;URLAUTH=user+%62ob", "Subject: alice\r\n\r\nalice body"},
		{"imap://alice;AUTH=*@example.com/INBOX;UIDVALIDITY=1/;UID=1/;SECTION=TEXT/;PARTIAL=2.4;URLAUTH=anonymous", "ice "},
		{"imap://alice@example.com/INBOX/;UID=1/;SECTION=HEADER.FIELDS%20(SUBJECT);URLAUTH=anonymous", "Subject: alice\r\n\r\n"},
		{"imap://alice@example.com/INBOX/;UID=1/;PARTIAL=18;URLAUTH=anonymous", "alice body"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			authorized, err := alice.GenerateURLAuth(t.Context(), tc.url, "INTERNAL", nil)
			if err != nil || !strings.HasPrefix(authorized, tc.url+":internal:") {
				t.Fatalf("generation changed URL: %q, %v", authorized, err)
			}
			if got, ok := readURLAuth(t, bob, authorized); !ok || got != tc.want {
				t.Fatalf("URL body = %q/%t, want %q", got, ok, tc.want)
			}
			// Hexadecimal tokens are case insensitive, but rump bytes are not.
			rump, token, _ := strings.Cut(authorized, ":internal:")
			if _, ok := readURLAuth(t, bob, rump+":INTERNAL:"+strings.ToUpper(token)); !ok {
				t.Fatal("uppercase hexadecimal token refused")
			}
			mutated := strings.Replace(authorized, "example.com", "EXAMPLE.COM", 1)
			if mutated == authorized {
				mutated = strings.Replace(authorized, "%63", "c", 1)
			}
			if mutated != authorized {
				if _, ok := readURLAuth(t, bob, mutated); ok {
					t.Fatal("normalizing signed URL bytes preserved authorization")
				}
			}
		})
	}
}

func TestURLAuthExpiryAndMailboxIdentity(t *testing.T) {
	alice, bob := urlAuthSessions(t)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rump := "imap://alice@example.com/INBOX/;UID=1;EXPIRE=" + future + ";URLAUTH=anonymous"
	authorized, err := alice.GenerateURLAuth(t.Context(), rump, "INTERNAL", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := readURLAuth(t, bob, authorized); !ok {
		t.Fatal("unexpired URL refused")
	}
	expired := strings.Replace(rump, future, "2000-01-01T00:00:00Z", 1)
	alice.account.mu.Lock()
	key := alice.account.urlAuthKeys["INBOX"]
	alice.account.mu.Unlock()
	if _, ok := readURLAuth(t, bob, expired+":internal:"+urlAuthToken(key.secret, expired)); ok {
		t.Fatal("expired URL with a valid token was honored")
	}
	alice.account.mu.Lock()
	alice.account.mailboxes["INBOX"].uidValidity++
	alice.account.mu.Unlock()
	if _, ok := readURLAuth(t, bob, authorized); ok {
		t.Fatal("UIDVALIDITY change revived an old token")
	}
	newURL, err := alice.GenerateURLAuth(t.Context(), rump, "INTERNAL", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := readURLAuth(t, bob, newURL); !ok {
		t.Fatal("new mailbox key failed after UIDVALIDITY change")
	}
	if _, ok := readURLAuth(t, bob, authorized); ok {
		t.Fatal("rotating a mailbox key revived the previous token")
	}
}

func TestURLAuthDeleteRecreateDoesNotReuseAuthorization(t *testing.T) {
	alice, bob := urlAuthSessions(t)
	if err := alice.Create(t.Context(), "Archive", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.Append(t.Context(), "Archive", strings.NewReader("old body"), nil); err != nil {
		t.Fatal(err)
	}
	rump := "imap://alice@example.com/Archive/;UID=1;URLAUTH=anonymous"
	authorized, err := alice.GenerateURLAuth(t.Context(), rump, "INTERNAL", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Delete(t.Context(), "Archive", nil); err != nil {
		t.Fatal(err)
	}
	if err := alice.Create(t.Context(), "Archive", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.Append(t.Context(), "Archive", strings.NewReader("private replacement"), nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := readURLAuth(t, bob, authorized); ok {
		t.Fatal("old token exposed a recreated mailbox")
	}
	if _, err := alice.GenerateURLAuth(t.Context(), rump, "INTERNAL", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := readURLAuth(t, bob, authorized); ok {
		t.Fatal("newly generated mailbox key revived an old URL")
	}
}

func TestURLAuthMalformedResolutionAndCancellation(t *testing.T) {
	alice, bob := urlAuthSessions(t)
	rump := "imap://alice@example.com/INBOX/;UID=1;URLAUTH=anonymous"
	authorized, err := alice.GenerateURLAuth(t.Context(), rump, "INTERNAL", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, malformed := range []string{rump, authorized + "x", authorized + "/", authorized + ";URLAUTH=anonymous", strings.Replace(authorized, ":internal:", ":unknown:", 1), authorized[:len(authorized)-1]} {
		if _, ok := readURLAuth(t, bob, malformed); ok {
			t.Fatalf("malformed URL resolved: %q", malformed)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := alice.GenerateURLAuth(ctx, rump, "INTERNAL", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled generation = %v", err)
	}
	if _, err := bob.FetchURLAuth(ctx, authorized, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled resolution = %v", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	result, err := bob.FetchURLAuth(ctx, authorized, nil)
	if err != nil || result == nil {
		t.Fatalf("resolution = %#v, %v", result, err)
	}
	cancel()
	defer result.Body.Close()
	if _, err := result.Body.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolved reader ignored cancellation: %v", err)
	}
}

func TestURLAuthRevocationUsesOwnerKeyTable(t *testing.T) {
	alice, bob := urlAuthSessions(t)
	rump := "imap://alice@example.com/INBOX/;UID=1;URLAUTH=user+bob"
	authorized, err := alice.GenerateURLAuth(t.Context(), rump, "INTERNAL", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bob.ResetURLAuthKey(t.Context(), "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := readURLAuth(t, bob, authorized); !ok {
		t.Fatal("recipient reset invalidated the owner's mailbox key")
	}
	if err := alice.ResetURLAuthKey(t.Context(), "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := readURLAuth(t, bob, authorized); ok {
		t.Fatal("owner reset failed to revoke a delegated URL")
	}
}
