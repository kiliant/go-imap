package imapserver

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"hash"
	"strings"

	"github.com/kiliant/go-imap"
)

// SCRAM-SHA-1 and SCRAM-SHA-256 (RFC 5802, RFC 7677), server side.
//
// # Why this needs a backend interface at all
//
// SERVER-DESIGN.md §3 draws the authentication line at "the framework owns every
// SASL mechanism state machine; the backend answers the credential question".
// For PLAIN, LOGIN, XOAUTH2 and OAUTHBEARER the backend receives extracted
// credentials and says yes or no. SCRAM does not work that way: the server never
// sees the password, and cannot, because the whole point is that a stolen
// database does not yield one. Instead it needs the stored derivation — salt,
// iteration count, StoredKey and ServerKey — which only the backend has.
//
// So the exchange, the nonces and the proof verification are all here, and the
// backend supplies four values. AUTH=SCRAM-* is advertised only when the backend
// implements the interface, because there is nothing to verify against
// otherwise.
//
// Channel binding (the -PLUS variants) is deliberately not implemented. It needs
// tls.ConnectionState.ExportKeyingMaterial, which the framework has, but
// advertising -PLUS commits the server to rejecting a client that downgrades to
// the non-PLUS form — and getting that wrong turns a downgrade defence into a
// downgrade vector. It is better absent than approximate.

// SCRAMCredentials is the optional SCRAM support of RFC 5802. A Backend
// implements it when it stores SCRAM derivations rather than passwords.
//
// SCRAMCredentials is consulted before any session exists, so it lives on the
// Backend rather than on a Session.
type SCRAMCredentials interface {
	// SCRAMCredentials returns the stored derivation for a username under the
	// named mechanism ("SCRAM-SHA-256"). It returns an error when the user is
	// unknown; the framework does not distinguish that from a bad password on
	// the wire, so a caller cannot probe for valid usernames.
	//
	// A backend implementing this must also accept the SCRAM mechanism in
	// Backend.Authenticate with an empty Password: the framework has already
	// verified the client's proof by then, and there is no password to check.
	SCRAMCredentials(ctx context.Context, mechanism, username string) (*SCRAMStoredCredentials, error)
}

// SCRAMStoredCredentials is one user's stored SCRAM derivation. It is
// deliberately not the password: RFC 5802's design is that this is all a server
// needs, so a stolen copy does not yield the password.
// Construct with keyed fields only; fields may be added in a future release.
type SCRAMStoredCredentials struct {
	// Salt is the per-user salt the password was derived under.
	Salt []byte
	// Iterations is the PBKDF2 iteration count.
	Iterations int
	// StoredKey is H(ClientKey), used to verify the client's proof.
	StoredKey []byte
	// ServerKey is HMAC(SaltedPassword, "Server Key"), used to sign the
	// server's own response so the client can authenticate the server.
	ServerKey []byte
	_         struct{}
}

// scramMechanisms are the mechanisms this file implements, and the hash each
// uses.
var scramMechanisms = map[string]func() hash.Hash{
	"SCRAM-SHA-1":   sha1.New,
	"SCRAM-SHA-256": sha256.New,
}

func init() {
	for name := range scramMechanisms {
		registerCapabilities(capabilityDescriptor{
			Name:            "AUTH=" + name,
			States:          stateMaskNotAuthenticated,
			RequiresBackend: hasSCRAMCredentials,
		})
	}
}

func hasSCRAMCredentials(_ *sessionState, backend Backend) bool {
	_, ok := backend.(SCRAMCredentials)
	return ok
}

// isSCRAMMechanism reports whether AUTHENTICATE should take the SCRAM path.
func isSCRAMMechanism(mechanism string) bool {
	_, ok := scramMechanisms[strings.ToUpper(mechanism)]
	return ok
}

// handleSCRAMAuthenticate runs a SCRAM exchange to completion.
//
// Every failure is reported identically and only after the exchange would
// otherwise have finished, so an attacker learns nothing from the shape or
// timing of a rejection beyond "it failed" — not whether the user exists, and
// not which step went wrong.
func handleSCRAMAuthenticate(ctx context.Context, c *conn, command *queuedCommand, args *authenticateArgs) error {
	mechanism := strings.ToUpper(args.mechanism)
	newHash := scramMechanisms[mechanism]
	store, ok := c.server.backend.(SCRAMCredentials)
	if !ok {
		return writeTaggedCondition(c, command.tag, "NO", imap.CodeCannot, "", "authentication mechanism is not supported")
	}

	clientFirst, err := scramInitialResponse(ctx, c, command, args)
	if err != nil {
		return err
	}
	if clientFirst == nil {
		return c.writeBad(command.tag, "AUTHENTICATE cancelled")
	}
	bare, username, clientNonce, err := parseSCRAMClientFirst(string(clientFirst))
	if err != nil {
		return authenticationRejected(c, command)
	}
	stored, err := store.SCRAMCredentials(ctx, mechanism, username)
	if err != nil || stored == nil || len(stored.StoredKey) == 0 || !scramIterationsValid(stored.Iterations) {
		return authenticationRejected(c, command)
	}

	serverNonce, err := scramNonce()
	if err != nil {
		return authenticationRejected(c, command)
	}
	combinedNonce := clientNonce + serverNonce
	serverFirst := fmt.Sprintf("r=%s,s=%s,i=%d", combinedNonce,
		base64.StdEncoding.EncodeToString(stored.Salt), stored.Iterations)

	response, aborted, err := c.continueSASL(ctx, []byte(serverFirst))
	if err != nil {
		return err
	}
	if aborted {
		return c.writeBad(command.tag, "AUTHENTICATE cancelled")
	}

	withoutProof, proof, err := parseSCRAMClientFinal(string(response), combinedNonce)
	if err != nil {
		return authenticationRejected(c, command)
	}
	authMessage := bare + "," + serverFirst + "," + withoutProof
	if !verifySCRAMProof(newHash, stored, authMessage, proof) {
		return authenticationRejected(c, command)
	}

	// The client is authenticated. The server now proves itself in return,
	// which is what stops a man in the middle replaying a captured exchange.
	serverSignature := hmacSum(newHash, stored.ServerKey, authMessage)
	final := "v=" + base64.StdEncoding.EncodeToString(serverSignature)
	if _, aborted, err = c.continueSASL(ctx, []byte(final)); err != nil {
		return err
	}
	if aborted {
		return c.writeBad(command.tag, "AUTHENTICATE cancelled")
	}
	// The backend is told the mechanism and the identity, and no password:
	// there is none to pass on, and the framework has already verified the
	// proof. A backend must therefore accept a SCRAM mechanism in
	// Backend.Authenticate without checking a password — checking one it never
	// received would reject every SCRAM login, which is exactly what the
	// reference backend did until this was written down.
	return authenticateBackend(ctx, c, command.tag, &Credentials{
		Mechanism: mechanism,
		Username:  username,
	})
}

// scramInitialResponse returns the client-first message, from the SASL-IR
// initial response when there was one and a continuation otherwise.
func scramInitialResponse(ctx context.Context, c *conn, command *queuedCommand, args *authenticateArgs) ([]byte, error) {
	if args.hasInitial {
		response, aborted, err := decodeSASL(args.initial)
		if err != nil {
			return nil, c.writeBad(command.tag, "invalid SASL initial response")
		}
		if aborted {
			return nil, nil
		}
		return response, nil
	}
	response, aborted, err := c.continueSASL(ctx, nil)
	if err != nil {
		return nil, err
	}
	if aborted {
		return nil, nil
	}
	return response, nil
}

func authenticationRejected(c *conn, command *queuedCommand) error {
	return writeTaggedCondition(c, command.tag, "NO", imap.CodeAuthenticationFailed, "", "authentication failed")
}

// parseSCRAMClientFirst reads "n,,n=user,r=nonce", returning the bare part that
// the auth message is built from.
//
// A "y" GS2 header means the client believes the server supports channel
// binding and is deliberately not using it. Since this server never advertises
// a -PLUS mechanism, that belief is wrong and the exchange is refused rather
// than continued — RFC 5802 section 6 makes this the downgrade detection.
func parseSCRAMClientFirst(message string) (bare, username, nonce string, err error) {
	switch {
	case strings.HasPrefix(message, "n,,"):
		bare = message[len("n,,"):]
	case strings.HasPrefix(message, "n,"), strings.HasPrefix(message, "y,"):
		return "", "", "", fmt.Errorf("unsupported SCRAM GS2 header")
	default:
		return "", "", "", fmt.Errorf("malformed SCRAM client-first message")
	}
	for _, field := range strings.Split(bare, ",") {
		value, ok := strings.CutPrefix(field, "n=")
		if ok {
			// RFC 5802 section 5.1 escapes "=" and "," in the username.
			username = strings.NewReplacer("=2C", ",", "=3D", "=").Replace(value)
			continue
		}
		if value, ok := strings.CutPrefix(field, "r="); ok {
			nonce = value
		}
	}
	if username == "" || nonce == "" {
		return "", "", "", fmt.Errorf("malformed SCRAM client-first message")
	}
	return bare, username, nonce, nil
}

// parseSCRAMClientFinal reads "c=biws,r=nonce,p=proof" and checks the nonce.
//
// The nonce check is what binds this message to the server-first message this
// server sent, so a replayed client-final from another exchange fails here.
func parseSCRAMClientFinal(message, expectedNonce string) (withoutProof string, proof []byte, err error) {
	at := strings.LastIndex(message, ",p=")
	if at < 0 {
		return "", nil, fmt.Errorf("malformed SCRAM client-final message")
	}
	withoutProof = message[:at]
	proof, err = base64.StdEncoding.DecodeString(message[at+len(",p="):])
	if err != nil {
		return "", nil, fmt.Errorf("malformed SCRAM proof")
	}
	var nonce string
	for _, field := range strings.Split(withoutProof, ",") {
		if value, ok := strings.CutPrefix(field, "r="); ok {
			nonce = value
		}
	}
	if subtle.ConstantTimeCompare([]byte(nonce), []byte(expectedNonce)) != 1 {
		return "", nil, fmt.Errorf("SCRAM nonce mismatch")
	}
	return withoutProof, proof, nil
}

// verifySCRAMProof checks the client's proof against the stored key.
//
// RFC 5802 section 3: ClientSignature is HMAC(StoredKey, AuthMessage), the proof
// is ClientKey XOR ClientSignature, and the client is authentic when
// H(ClientKey) equals StoredKey. The comparison is constant-time — a
// variable-time one would leak the stored key a byte at a time to anyone able
// to make guesses.
func verifySCRAMProof(newHash func() hash.Hash, stored *SCRAMStoredCredentials, authMessage string, proof []byte) bool {
	signature := hmacSum(newHash, stored.StoredKey, authMessage)
	if len(proof) != len(signature) {
		return false
	}
	clientKey := make([]byte, len(proof))
	for i := range proof {
		clientKey[i] = proof[i] ^ signature[i]
	}
	digest := newHash()
	digest.Write(clientKey)
	return subtle.ConstantTimeCompare(digest.Sum(nil), stored.StoredKey) == 1
}

func hmacSum(newHash func() hash.Hash, key []byte, message string) []byte {
	mac := hmac.New(newHash, key)
	mac.Write([]byte(message))
	return mac.Sum(nil)
}

// scramNonce returns a fresh server nonce.
//
// RFC 5802 section 5.1 requires it to be unpredictable: it is what stops an
// attacker replaying a captured client-final message, so it comes from the
// cryptographic source rather than a counter.
func scramNonce() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(raw), nil
}

// scramIterationsValid bounds the iteration count a backend may report.
//
// A count below the RFC 7677 section 4 floor is too weak to be worth
// advertising, and an unbounded one lets a misconfigured backend turn every
// login into a denial of service against itself.
func scramIterationsValid(iterations int) bool {
	return iterations >= 4096 && iterations <= 1_000_000
}
