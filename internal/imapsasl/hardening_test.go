package imapsasl

import (
	"encoding/base64"
	"strings"
	"testing"
)

// A credential carrying a field delimiter would otherwise let a caller who
// forwards untrusted input forge the remaining fields of the response.
func TestMechanismsRejectControlCharactersInCredentials(t *testing.T) {
	scram := func(username, password string) *Mechanism {
		m, err := SCRAMSHA256(username, password)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, tc := range []struct {
		name      string
		mechanism *Mechanism
	}{
		{"PLAIN username", Plain("us\x00er", "pw")},
		{"PLAIN password", Plain("user", "p\x00w")},
		{"LOGIN username", Login("us\ner", "pw")},
		{"CRAM-MD5 username with a space", CRAMMD5("us er", "pw")},
		{"CRAM-MD5 username with a NUL", CRAMMD5("us\x00er", "pw")},
		{"XOAUTH2 username", XOAUTH2("us\x01er", "token")},
		{"XOAUTH2 token", XOAUTH2("user", "tok\x01en")},
		{"OAUTHBEARER token", OAUTHBEARER("user", "tok\x01en", "", "")},
		{"OAUTHBEARER host", OAUTHBEARER("user", "token", "ho\x01st", "")},
		{"SCRAM username", scram("us\x00er", "pw")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			challenge := []byte(nil)
			if tc.mechanism.Name == "CRAM-MD5" {
				challenge = []byte("<1@example>")
			}
			var err error
			for range 3 {
				if _, err = tc.mechanism.Next(challenge); err != nil {
					break
				}
			}
			if err == nil {
				t.Fatal("Next() = nil, want a rejection")
			}
		})
	}
}

// RFC 7628 section 3.1 puts the authzid in a GS2 header, where an unescaped
// comma or equals sign would end the field early.
func TestOAUTHBEAREREscapesAuthzid(t *testing.T) {
	response, err := OAUTHBEARER("a,b=c@example.com", "token", "", "").Next(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(response)
	if !strings.HasPrefix(got, "n,a=a=2Cb=3Dc@example.com,\x01") {
		t.Fatalf("initial response = %q, want the authzid escaped", got)
	}
}

// The SCRAM iteration count drives a PBKDF2 loop the server controls, so an
// unbounded one is a one-line denial of service against the client.
func TestSCRAMRejectsExcessiveIterationCount(t *testing.T) {
	m, err := SCRAMSHA256("user", "pw")
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Next(nil)
	if err != nil {
		t.Fatal(err)
	}
	nonce := scramAttrValue(strings.TrimPrefix(string(first), "n,,"), "r")
	salt := base64.StdEncoding.EncodeToString([]byte("salt"))
	serverFirst := "r=" + nonce + "server,s=" + salt + ",i=100000000"
	if _, err := m.Next([]byte(serverFirst)); err == nil || !strings.Contains(err.Error(), "exceeds the limit") {
		t.Fatalf("Next() = %v, want the iteration count rejected", err)
	}
}

// Every mechanism here is a fixed-length exchange. A server that keeps sending
// challenges must be refused rather than driven forever.
func TestMechanismsRefuseExtraChallenges(t *testing.T) {
	for _, m := range []*Mechanism{
		XOAUTH2("user", "token"),
		OAUTHBEARER("user", "token", "", ""),
	} {
		t.Run(m.Name, func(t *testing.T) {
			if _, err := m.Next(nil); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Next([]byte("{\"status\":\"401\"}")); err != nil {
				t.Fatalf("error challenge = %v, want the empty response", err)
			}
			if _, err := m.Next([]byte("{\"status\":\"401\"}")); err == nil {
				t.Fatal("third challenge = nil, want a rejection")
			}
		})
	}
}
