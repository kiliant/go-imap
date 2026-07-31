// Package imapsasl implements the SASL mechanisms used by imapclient.
//
// The package deliberately depends only on the Go standard library. Its
// Mechanism type is a small state machine: Next is called with each decoded
// server challenge and returns the decoded response to send to the server.
package imapsasl

import (
	"crypto/hmac"
	"crypto/md5"  // #nosec G501 -- CRAM-MD5 is required for legacy servers.
	"crypto/sha1" // #nosec G505 -- SCRAM-SHA-1 is required for legacy servers.
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"strings"
)

// Mechanism is one SASL exchange. Next receives the decoded server challenge
// (nil for an initial response) and returns the decoded client response.
//
// It is deliberately a struct of a function field rather than an interface:
// users of imapclient can provide custom mechanisms without this package
// committing an exported interface to a fixed method set.
type Mechanism struct {
	Name string
	Next func(challenge []byte) ([]byte, error)
}

// checkField rejects the octets that would let a credential escape the field it
// belongs to. Every mechanism here builds its response by concatenation, so an
// unchecked NUL or SOH in a username lets a caller who forwards untrusted input
// forge the remaining fields — an "auth=Bearer" of their choosing, for
// instance. CR and LF are refused for the same reason one layer up.
func checkField(mechanism, field, value string) error {
	if i := strings.IndexFunc(value, func(r rune) bool {
		return r == 0 || r == '\x01' || r == '\r' || r == '\n'
	}); i >= 0 {
		return fmt.Errorf("imapsasl: %s %s contains a control character at offset %d", mechanism, field, i)
	}
	return nil
}

// Plain returns the RFC 4616 PLAIN mechanism.
func Plain(username, password string) *Mechanism {
	called := false
	return &Mechanism{Name: "PLAIN", Next: func([]byte) ([]byte, error) {
		if called {
			return nil, errors.New("imapsasl: unexpected PLAIN challenge")
		}
		called = true
		if err := checkField("PLAIN", "username", username); err != nil {
			return nil, err
		}
		if err := checkField("PLAIN", "password", password); err != nil {
			return nil, err
		}
		return []byte("\x00" + username + "\x00" + password), nil
	}}
}

// Login returns the legacy SASL LOGIN mechanism.
func Login(username, password string) *Mechanism {
	step := 0
	return &Mechanism{Name: "LOGIN", Next: func([]byte) ([]byte, error) {
		step++
		switch step {
		case 1:
			if err := checkField("LOGIN", "username", username); err != nil {
				return nil, err
			}
			return []byte(username), nil
		case 2:
			if err := checkField("LOGIN", "password", password); err != nil {
				return nil, err
			}
			return []byte(password), nil
		default:
			return nil, errors.New("imapsasl: unexpected LOGIN challenge")
		}
	}}
}

// CRAMMD5 returns the legacy RFC 2195 CRAM-MD5 mechanism.
func CRAMMD5(username, password string) *Mechanism {
	called := false
	return &Mechanism{Name: "CRAM-MD5", Next: func(challenge []byte) ([]byte, error) {
		// CRAM-MD5 has no initial response. A nil input is the auth driver's
		// probe for SASL-IR support; a server challenge is never nil.
		if challenge == nil && !called {
			return nil, nil
		}
		if called || len(challenge) == 0 {
			return nil, errors.New("imapsasl: invalid CRAM-MD5 challenge")
		}
		called = true
		// The response is "username SP digest"; a space in the username would
		// make the server read a different name than the caller supplied.
		if err := checkField("CRAM-MD5", "username", username); err != nil {
			return nil, err
		}
		if strings.ContainsAny(username, " ") {
			return nil, errors.New("imapsasl: CRAM-MD5 username contains a space")
		}
		mac := hmac.New(md5.New, []byte(password)) // #nosec G401 -- RFC 2195.
		_, _ = mac.Write(challenge)
		return []byte(username + " " + hex.EncodeToString(mac.Sum(nil))), nil
	}}
}

// XOAUTH2 returns the widely deployed XOAUTH2 mechanism.
func XOAUTH2(username, token string) *Mechanism {
	step := 0
	return &Mechanism{Name: "XOAUTH2", Next: func([]byte) ([]byte, error) {
		step++
		switch step {
		case 1:
			if err := checkField("XOAUTH2", "username", username); err != nil {
				return nil, err
			}
			if err := checkField("XOAUTH2", "token", token); err != nil {
				return nil, err
			}
			return []byte("user=" + username + "\x01auth=Bearer " + token + "\x01\x01"), nil
		case 2:
			// Google sends a base64 JSON error challenge before its final NO,
			// and expects an empty response before it will send that NO.
			return []byte{}, nil
		default:
			return nil, errors.New("imapsasl: unexpected XOAUTH2 challenge")
		}
	}}
}

// OAUTHBEARER returns the RFC 7628 OAUTHBEARER mechanism. host and port are
// optional; include them when the token issuer uses them to bind the token to a
// target service.
func OAUTHBEARER(username, token, host, port string) *Mechanism {
	step := 0
	return &Mechanism{Name: "OAUTHBEARER", Next: func([]byte) ([]byte, error) {
		step++
		switch step {
		case 1:
			for field, value := range map[string]string{"username": username, "token": token, "host": host, "port": port} {
				if err := checkField("OAUTHBEARER", field, value); err != nil {
					return nil, err
				}
			}
			var b strings.Builder
			b.WriteString("n,a=")
			// RFC 7628 section 3.1: the authzid is a GS2 header field, so it
			// takes the RFC 5801 escaping that keeps a comma or equals sign
			// from ending it early.
			b.WriteString(scramEscape(username))
			b.WriteString(",\x01")
			if host != "" {
				b.WriteString("host=")
				b.WriteString(host)
				b.WriteByte('\x01')
			}
			if port != "" {
				b.WriteString("port=")
				b.WriteString(port)
				b.WriteByte('\x01')
			}
			b.WriteString("auth=Bearer ")
			b.WriteString(token)
			b.WriteString("\x01\x01")
			return []byte(b.String()), nil
		case 2:
			// RFC 7628 section 3.2.3: the client answers an error challenge
			// with a single "%x01" before the server sends its tagged failure.
			return []byte{}, nil
		default:
			return nil, errors.New("imapsasl: unexpected OAUTHBEARER challenge")
		}
	}}
}

// SCRAMSHA1 returns RFC 5802 SCRAM-SHA-1.
func SCRAMSHA1(username, password string) (*Mechanism, error) {
	return newSCRAM("SCRAM-SHA-1", sha1.New, 20, username, password, nil)
}

// SCRAMSHA256 returns RFC 7677 SCRAM-SHA-256.
func SCRAMSHA256(username, password string) (*Mechanism, error) {
	return newSCRAM("SCRAM-SHA-256", sha256.New, 32, username, password, nil)
}

// SCRAMSHA1Plus returns RFC 5802 SCRAM-SHA-1-PLUS. channelBinding must be the
// bytes exported from the TLS connection with the "EXPORTER-Channel-Binding"
// label.
func SCRAMSHA1Plus(username, password string, channelBinding []byte) (*Mechanism, error) {
	return newSCRAM("SCRAM-SHA-1-PLUS", sha1.New, 20, username, password, channelBinding)
}

// SCRAMSHA256Plus returns RFC 7677 SCRAM-SHA-256-PLUS. channelBinding must be
// the bytes exported from the TLS connection with the
// "EXPORTER-Channel-Binding" label.
func SCRAMSHA256Plus(username, password string, channelBinding []byte) (*Mechanism, error) {
	return newSCRAM("SCRAM-SHA-256-PLUS", sha256.New, 32, username, password, channelBinding)
}

// maxSCRAMIterations bounds the PBKDF2 work a server can ask the client to do.
const maxSCRAMIterations = 1 << 20

type scram struct {
	hash           func() hash.Hash
	hashSize       int
	username       string
	password       string
	gs2Header      string
	channelBinding []byte
	clientFirst    string
	serverFirst    string
	serverSig      []byte
	step           int
}

func newSCRAM(name string, hashFunc func() hash.Hash, hashSize int, username, password string, channelBinding []byte) (*Mechanism, error) {
	s := &scram{hash: hashFunc, hashSize: hashSize, username: username, password: password, channelBinding: channelBinding}
	if strings.HasSuffix(name, "-PLUS") {
		if len(channelBinding) == 0 {
			return nil, errors.New("imapsasl: SCRAM-PLUS requires channel binding")
		}
		s.gs2Header = "p=tls-exporter,,"
	} else {
		s.gs2Header = "n,,"
	}
	return &Mechanism{Name: name, Next: s.next}, nil
}

func (s *scram) next(challenge []byte) ([]byte, error) {
	s.step++
	switch s.step {
	case 1:
		if err := checkField("SCRAM", "username", s.username); err != nil {
			return nil, err
		}
		nonce, err := scramNonce()
		if err != nil {
			return nil, err
		}
		s.clientFirst = "n=" + scramEscape(s.username) + ",r=" + nonce
		return []byte(s.gs2Header + s.clientFirst), nil
	case 2:
		return s.clientFinal(string(challenge))
	case 3:
		if err := s.verifyServerFinal(string(challenge)); err != nil {
			return nil, err
		}
		return nil, nil
	default:
		return nil, errors.New("imapsasl: unexpected SCRAM challenge")
	}
}

func (s *scram) clientFinal(serverFirst string) ([]byte, error) {
	attrs, err := scramAttrs(serverFirst)
	if err != nil {
		return nil, err
	}
	nonce, ok := attrs["r"]
	if !ok || !strings.HasPrefix(nonce, scramAttrValue(s.clientFirst, "r")) || len(nonce) <= len(scramAttrValue(s.clientFirst, "r")) {
		return nil, errors.New("imapsasl: invalid SCRAM server nonce")
	}
	salt64, ok := attrs["s"]
	if !ok {
		return nil, errors.New("imapsasl: SCRAM server omitted salt")
	}
	salt, err := base64.StdEncoding.DecodeString(salt64)
	if err != nil || len(salt) == 0 {
		return nil, errors.New("imapsasl: invalid SCRAM salt")
	}
	iterations, ok := attrs["i"]
	if !ok {
		return nil, errors.New("imapsasl: SCRAM server omitted iteration count")
	}
	iter, err := strconv.Atoi(iterations)
	if err != nil || iter <= 0 || strconv.Itoa(iter) != iterations {
		return nil, errors.New("imapsasl: invalid SCRAM iteration count")
	}
	if iter > maxSCRAMIterations {
		// The count is server-controlled and drives a PBKDF2 loop, so an
		// unbounded one is a one-line denial of service against the client.
		// RFC 7677 recommends 4096; deployed servers use up to a few hundred
		// thousand.
		return nil, fmt.Errorf("imapsasl: SCRAM iteration count %d exceeds the limit of %d", iter, maxSCRAMIterations)
	}

	cb := append([]byte(s.gs2Header), s.channelBinding...)
	withoutProof := "c=" + base64.StdEncoding.EncodeToString(cb) + ",r=" + nonce
	authMessage := s.clientFirst + "," + serverFirst + "," + withoutProof
	saltedPassword := pbkdf2(s.hash, []byte(s.password), salt, iter, s.hashSize)
	clientKey := hmacSum(s.hash, saltedPassword, []byte("Client Key"))
	storedKey := hashSum(s.hash, clientKey)
	clientSignature := hmacSum(s.hash, storedKey, []byte(authMessage))
	proof := xor(clientKey, clientSignature)
	serverKey := hmacSum(s.hash, saltedPassword, []byte("Server Key"))
	s.serverSig = hmacSum(s.hash, serverKey, []byte(authMessage))
	s.serverFirst = serverFirst
	return []byte(withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof)), nil
}

func (s *scram) verifyServerFinal(serverFinal string) error {
	attrs, err := scramAttrs(serverFinal)
	if err != nil {
		return err
	}
	if attrs["e"] != "" {
		return errors.New("imapsasl: SCRAM server rejected authentication")
	}
	signature, ok := attrs["v"]
	if !ok {
		return errors.New("imapsasl: SCRAM server omitted signature")
	}
	got, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || subtle.ConstantTimeCompare(got, s.serverSig) != 1 {
		return errors.New("imapsasl: SCRAM server signature mismatch")
	}
	return nil
}

func scramNonce() (string, error) {
	// A cryptographically random nonce prevents server replay and nonce-prefix
	// attacks. Raw base64 contains no commas, which SCRAM reserves as a field
	// delimiter.
	b := make([]byte, 18)
	if _, err := randRead(b); err != nil {
		return "", errors.New("imapsasl: unable to generate SCRAM nonce")
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}

func scramEscape(s string) string {
	s = strings.ReplaceAll(s, "=", "=3D")
	return strings.ReplaceAll(s, ",", "=2C")
}

func scramAttrs(s string) (map[string]string, error) {
	attrs := make(map[string]string)
	for _, part := range strings.Split(s, ",") {
		if len(part) < 3 || part[1] != '=' || part[0] < 'a' || part[0] > 'z' {
			return nil, errors.New("imapsasl: malformed SCRAM attribute")
		}
		key := part[:1]
		if _, duplicate := attrs[key]; duplicate {
			return nil, errors.New("imapsasl: duplicate SCRAM attribute")
		}
		attrs[key] = part[2:]
	}
	return attrs, nil
}

func scramAttrValue(s, key string) string {
	for _, part := range strings.Split(s, ",") {
		if strings.HasPrefix(part, key+"=") {
			return strings.TrimPrefix(part, key+"=")
		}
	}
	return ""
}

func pbkdf2(newHash func() hash.Hash, password, salt []byte, iterations, size int) []byte {
	mac := hmac.New(newHash, password)
	_, _ = mac.Write(salt)
	_, _ = mac.Write([]byte{0, 0, 0, 1})
	u := mac.Sum(nil)
	out := append([]byte(nil), u...)
	for n := 1; n < iterations; n++ {
		mac = hmac.New(newHash, password)
		_, _ = mac.Write(u)
		u = mac.Sum(nil)
		for i := range out {
			out[i] ^= u[i]
		}
	}
	return out[:size]
}

func hmacSum(newHash func() hash.Hash, key, value []byte) []byte {
	h := hmac.New(newHash, key)
	_, _ = h.Write(value)
	return h.Sum(nil)
}

func hashSum(newHash func() hash.Hash, value []byte) []byte {
	h := newHash()
	_, _ = h.Write(value)
	return h.Sum(nil)
}

func xor(left, right []byte) []byte {
	result := make([]byte, len(left))
	for i := range left {
		result[i] = left[i] ^ right[i]
	}
	return result
}
