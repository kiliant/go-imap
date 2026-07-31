package imapsasl

import (
	"crypto/sha1" // #nosec G505 -- RFC 5802 test vector.
	"encoding/base64"
	"strings"
	"testing"
)

func TestSimpleMechanisms(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		m := Plain("alice", "secret")
		got, err := m.Next(nil)
		if err != nil || string(got) != "\x00alice\x00secret" {
			t.Fatalf("Next() = %q, %v", got, err)
		}
	})
	t.Run("login", func(t *testing.T) {
		m := Login("alice", "secret")
		got, err := m.Next(nil)
		if err != nil || string(got) != "alice" {
			t.Fatalf("first Next() = %q, %v", got, err)
		}
		got, err = m.Next([]byte("Password:"))
		if err != nil || string(got) != "secret" {
			t.Fatalf("second Next() = %q, %v", got, err)
		}
	})
	t.Run("cram-md5", func(t *testing.T) {
		m := CRAMMD5("tim", "tanstaaftanstaaf")
		got, err := m.Next([]byte("<1896.697170952@postoffice.reston.mci.net>"))
		if err != nil || string(got) != "tim b913a602c7eda7a495b4e6e7334d3890" {
			t.Fatalf("Next() = %q, %v", got, err)
		}
	})
	t.Run("oauth", func(t *testing.T) {
		m := OAUTHBEARER("alice", "token", "mail.example.test", "993")
		got, err := m.Next(nil)
		want := "n,a=alice,\x01host=mail.example.test\x01port=993\x01auth=Bearer token\x01\x01"
		if err != nil || string(got) != want {
			t.Fatalf("Next() = %q, %v", got, err)
		}
		got, err = m.Next([]byte(`{"status":"401"}`))
		if err != nil || len(got) != 0 {
			t.Fatalf("error reply = %q, %v", got, err)
		}
	})
}

func TestSCRAMSHA1ServerSignature(t *testing.T) {
	oldRandRead := randRead
	randRead = func(b []byte) (int, error) {
		copy(b, []byte("fixed-client-nonce!"))
		return len(b), nil
	}
	t.Cleanup(func() { randRead = oldRandRead })

	m, err := SCRAMSHA1("user", "pencil")
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Next(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(first), "n,,n=user,r=") {
		t.Fatalf("client first = %q", first)
	}
	nonce := strings.TrimPrefix(string(first), "n,,n=user,r=")
	serverFirst := "r=" + nonce + "server,s=c2FsdA==,i=4096"
	final, err := m.Next([]byte(serverFirst))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(final), ",p=") {
		t.Fatalf("client final lacks proof: %q", final)
	}

	withoutProof := strings.Split(string(final), ",p=")[0]
	authMessage := "n=user,r=" + nonce + "," + serverFirst + "," + withoutProof
	salted := pbkdf2(sha1.New, []byte("pencil"), []byte("salt"), 4096, sha1.Size)
	serverKey := hmacSum(sha1.New, salted, []byte("Server Key"))
	serverSig := hmacSum(sha1.New, serverKey, []byte(authMessage))
	if _, err := m.Next([]byte("v=" + base64.StdEncoding.EncodeToString(serverSig))); err != nil {
		t.Fatalf("valid server signature: %v", err)
	}
}

func TestSCRAMRejectsInvalidServerSignature(t *testing.T) {
	m, err := SCRAMSHA256("user", "password")
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Next(nil)
	if err != nil {
		t.Fatal(err)
	}
	nonce := scramAttrValue(strings.TrimPrefix(string(first), "n,,"), "r")
	if _, err := m.Next([]byte("r=" + nonce + "server,s=c2FsdA==,i=1")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Next([]byte("v=YmFk")); err == nil {
		t.Fatal("accepted an invalid SCRAM server signature")
	}
}

func TestSCRAMPlusChannelBinding(t *testing.T) {
	m, err := SCRAMSHA256Plus("alice", "secret", []byte("exported binding"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Next(nil)
	if err != nil || !strings.HasPrefix(string(first), "p=tls-exporter,,") {
		t.Fatalf("client first = %q, %v", first, err)
	}
	if _, err := SCRAMSHA256Plus("alice", "secret", nil); err == nil {
		t.Fatal("accepted missing channel binding")
	}
}
