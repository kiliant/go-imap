package memory

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"hash"
	"strings"

	"github.com/kiliant/go-imap/imapserver"
)

// SCRAM credential storage for the reference backend.
//
// The derivations are computed lazily from the configured test password and
// cached, because that is the only way a backend configured with plaintext test
// passwords can serve SCRAM at all. A real backend stores the derivation and
// never has the password — which is the entire point of RFC 5802, and worth
// saying plainly here so nobody reads this as a model to copy.

const scramIterations = 4096

type scramDerivation struct {
	salt       []byte
	iterations int
	storedKey  []byte
	serverKey  []byte
}

// SCRAMCredentials implements [imapserver.SCRAMCredentials].
func (b *Backend) SCRAMCredentials(ctx context.Context, mechanism, username string, options *imapserver.SCRAMCredentialsOptions) (*imapserver.SCRAMStoredCredentials, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	newHash, ok := scramHash(mechanism)
	if !ok {
		return nil, authenticationError()
	}
	// This backend has no notion of one identity acting as another, so a
	// request to do so is refused rather than quietly ignored.
	if options != nil && options.AuthzID != "" && options.AuthzID != username {
		return nil, authenticationError()
	}
	b.mu.RLock()
	account := b.accounts[username]
	b.mu.RUnlock()
	if account == nil {
		return nil, authenticationError()
	}

	// The cache hangs off this Backend, not off the package. A package-level
	// cache keyed by username would let two backends configured with the same
	// username and different passwords share one derivation, so the second
	// would authenticate against the first one's password — and it would leak
	// state across the isolated instances backendtest's harness exists to
	// provide.
	key := mechanism + "\x00" + username
	b.scramMu.Lock()
	defer b.scramMu.Unlock()
	if b.scramCache == nil {
		b.scramCache = make(map[string]*scramDerivation)
	}
	derivation, ok := b.scramCache[key]
	if !ok {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		derivation = deriveSCRAM(newHash, account.password, salt, scramIterations)
		b.scramCache[key] = derivation
	}
	return &imapserver.SCRAMStoredCredentials{
		Salt:       append([]byte(nil), derivation.salt...),
		Iterations: derivation.iterations,
		StoredKey:  append([]byte(nil), derivation.storedKey...),
		ServerKey:  append([]byte(nil), derivation.serverKey...),
	}, nil
}

func scramHash(mechanism string) (func() hash.Hash, bool) {
	switch strings.ToUpper(mechanism) {
	case "SCRAM-SHA-1":
		return sha1.New, true
	case "SCRAM-SHA-256":
		return sha256.New, true
	default:
		return nil, false
	}
}

// deriveSCRAM computes the RFC 5802 section 3 derivation from a password.
func deriveSCRAM(newHash func() hash.Hash, password string, salt []byte, iterations int) *scramDerivation {
	salted := pbkdf2Sum(newHash, []byte(password), salt, iterations)
	clientKey := hmacOf(newHash, salted, []byte("Client Key"))
	digest := newHash()
	digest.Write(clientKey)
	return &scramDerivation{
		salt:       salt,
		iterations: iterations,
		storedKey:  digest.Sum(nil),
		serverKey:  hmacOf(newHash, salted, []byte("Server Key")),
	}
}

// pbkdf2Sum is PBKDF2-HMAC with one output block, which is all SCRAM needs: the
// derived key is exactly the hash length. Written here rather than pulled from
// a dependency because this module has none by policy.
func pbkdf2Sum(newHash func() hash.Hash, password, salt []byte, iterations int) []byte {
	previous := hmacOf(newHash, password, append(append([]byte(nil), salt...), 0, 0, 0, 1))
	result := append([]byte(nil), previous...)
	for i := 1; i < iterations; i++ {
		previous = hmacOf(newHash, password, previous)
		for j := range result {
			result[j] ^= previous[j]
		}
	}
	return result
}

func hmacOf(newHash func() hash.Hash, key, message []byte) []byte {
	mac := hmac.New(newHash, key)
	mac.Write(message)
	return mac.Sum(nil)
}

var _ imapserver.SCRAMCredentials = (*Backend)(nil)
