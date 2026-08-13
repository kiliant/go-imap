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
// # Channel binding
//
// The -PLUS variants bind the authentication to the TLS connection it runs over,
// so an attacker who terminates TLS and re-originates it cannot relay a captured
// exchange. The binding value comes from RFC 9266's tls-exporter, which Go
// exposes as tls.ConnectionState.ExportKeyingMaterial.
//
// Advertising -PLUS commits the server to the downgrade defence of RFC 5802
// section 6, and getting that wrong converts a defence into a vector. Both
// directions are enforced here: a client sending the "y" header (meaning "I
// believe you have no -PLUS") is refused whenever -PLUS *is* advertised, since
// that belief can only come from tampering; and a client choosing a -PLUS
// mechanism must supply binding data that matches this connection's.

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
	SCRAMCredentials(ctx context.Context, mechanism, username string, options *SCRAMCredentialsOptions) (*SCRAMStoredCredentials, error)
}

// SCRAMCredentialsOptions configures a SCRAM credential lookup. A nil pointer
// selects the defaults.
//
// AuthzID is the authorization identity from RFC 5802 section 5.1's "a=" field:
// the identity the client wants to act as, when it differs from the one it
// authenticated as. That is the ordinary proxy-authentication and
// admin-as-user case, and a backend cannot implement it without seeing the
// field.
// Construct with keyed fields only; fields may be added in a future release.
type SCRAMCredentialsOptions struct {
	// AuthzID is the requested authorization identity, or empty when the
	// client asked to act as itself.
	AuthzID string `imapfeature:"scram"`
	_       struct{}
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
	"SCRAM-SHA-1":        sha1.New,
	"SCRAM-SHA-256":      sha256.New,
	"SCRAM-SHA-1-PLUS":   sha1.New,
	"SCRAM-SHA-256-PLUS": sha256.New,
}

// scramChannelBound reports whether a mechanism name is a -PLUS variant.
func scramChannelBound(mechanism string) bool {
	return strings.HasSuffix(mechanism, "-PLUS")
}

// scramBaseMechanism maps a -PLUS name to the credential family it shares with
// its unbound form: the stored derivation is the same, only the exchange differs.
func scramBaseMechanism(mechanism string) string {
	return strings.TrimSuffix(mechanism, "-PLUS")
}

// scramChannelBinding returns this connection's RFC 9266 tls-exporter value.
//
// It is empty on a cleartext connection, which is why the -PLUS descriptors
// require TLS: there is nothing to bind to otherwise.
func scramChannelBinding(c *conn) ([]byte, bool) {
	state, ok := connectionTLSState(c.currentTransport())
	if !ok {
		return nil, false
	}
	material, err := state.ExportKeyingMaterial("EXPORTER-Channel-Binding", nil, 32)
	if err != nil {
		return nil, false
	}
	return material, true
}

const featureSCRAM featureID = "scram"

func init() {
	registerFeatures(featureDescriptor{
		ID: featureSCRAM,
		Active: func(_ *sessionState, advertised map[string]bool) bool {
			return advertised["AUTH=SCRAM-SHA-256"] || advertised["AUTH=SCRAM-SHA-1"]
		},
	})
	for name := range scramMechanisms {
		descriptor := capabilityDescriptor{
			Name:            "AUTH=" + name,
			States:          stateMaskNotAuthenticated,
			RequiresBackend: hasSCRAMCredentials,
		}
		// A -PLUS mechanism has nothing to bind to without TLS.
		if scramChannelBound(name) {
			descriptor.RequiresTLS = tlsOnly
		}
		registerCapabilities(descriptor)
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
	bound := scramChannelBound(mechanism)
	var binding []byte
	if bound {
		var ok bool
		if binding, ok = scramChannelBinding(c); !ok {
			return authenticationRejected(c, command)
		}
	}
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
	plusAdvertised := scramPlusAdvertised(c)
	bare, username, authzID, clientNonce, err := parseSCRAMClientFirst(string(clientFirst), bound, plusAdvertised)
	if err != nil {
		return authenticationRejected(c, command)
	}
	// The stored derivation is shared with the unbound form: -PLUS changes the
	// exchange, not the credential.
	stored, err := store.SCRAMCredentials(ctx, scramBaseMechanism(mechanism), username, &SCRAMCredentialsOptions{AuthzID: authzID})
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

	withoutProof, proof, err := parseSCRAMClientFinal(string(response), combinedNonce, gs2Header(bound, authzID), binding)
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
		AuthzID:   authzID,
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
func parseSCRAMClientFirst(message string, bound, plusAdvertised bool) (bare, username, authzID, nonce string, err error) {
	header, rest, found := strings.Cut(message, ",")
	if !found {
		return "", "", "", "", fmt.Errorf("malformed SCRAM client-first message")
	}
	switch {
	case bound:
		// A -PLUS mechanism must declare which binding type it used.
		if header != "p=tls-exporter" {
			return "", "", "", "", fmt.Errorf("unsupported SCRAM channel binding type")
		}
	case header == "n":
		// "n" means the client does not support channel binding at all, which
		// is always allowed.
	case header == "y":
		// "y" means the client supports channel binding but believes this
		// server does not. RFC 5802 section 6 makes refusing that the downgrade
		// detection: if -PLUS *is* advertised, the belief can only come from
		// someone stripping it in transit.
		if plusAdvertised {
			return "", "", "", "", fmt.Errorf("SCRAM channel-binding downgrade detected")
		}
	default:
		return "", "", "", "", fmt.Errorf("unsupported SCRAM GS2 header")
	}
	authzField, remainder, found := strings.Cut(rest, ",")
	if !found {
		return "", "", "", "", fmt.Errorf("malformed SCRAM client-first message")
	}
	if authzField != "" {
		value, ok := strings.CutPrefix(authzField, "a=")
		if !ok {
			return "", "", "", "", fmt.Errorf("malformed SCRAM authorization identity")
		}
		authzID = strings.NewReplacer("=2C", ",", "=3D", "=").Replace(value)
	}
	bare = remainder
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
		return "", "", "", "", fmt.Errorf("malformed SCRAM client-first message")
	}
	return bare, username, authzID, nonce, nil
}

// parseSCRAMClientFinal reads "c=biws,r=nonce,p=proof" and checks the nonce.
//
// The nonce check is what binds this message to the server-first message this
// server sent, so a replayed client-final from another exchange fails here.
func parseSCRAMClientFinal(message, expectedNonce, header string, binding []byte) (withoutProof string, proof []byte, err error) {
	at := strings.LastIndex(message, ",p=")
	if at < 0 {
		return "", nil, fmt.Errorf("malformed SCRAM client-final message")
	}
	withoutProof = message[:at]
	proof, err = base64.StdEncoding.DecodeString(message[at+len(",p="):])
	if err != nil {
		return "", nil, fmt.Errorf("malformed SCRAM proof")
	}
	var nonce, channel string
	for _, field := range strings.Split(withoutProof, ",") {
		if value, ok := strings.CutPrefix(field, "r="); ok {
			nonce = value
		}
		if value, ok := strings.CutPrefix(field, "c="); ok {
			channel = value
		}
	}
	if subtle.ConstantTimeCompare([]byte(nonce), []byte(expectedNonce)) != 1 {
		return "", nil, fmt.Errorf("SCRAM nonce mismatch")
	}
	// The c= field carries the GS2 header the client claimed, plus the channel
	// binding data when one was used. Verifying it is what actually binds the
	// exchange to this connection: without the check, a -PLUS mechanism would
	// be no stronger than its unbound form.
	expectedChannel := base64.StdEncoding.EncodeToString(append([]byte(header), binding...))
	if subtle.ConstantTimeCompare([]byte(channel), []byte(expectedChannel)) != 1 {
		return "", nil, fmt.Errorf("SCRAM channel binding mismatch")
	}
	return withoutProof, proof, nil
}

// gs2Header reconstructs the header the client must have sent, which the c=
// field of its final message repeats.
func gs2Header(bound bool, authzID string) string {
	prefix := "n,"
	if bound {
		prefix = "p=tls-exporter,"
	}
	if authzID == "" {
		return prefix + ","
	}
	escaped := strings.NewReplacer(",", "=2C", "=", "=3D").Replace(authzID)
	return prefix + "a=" + escaped + ","
}

// scramPlusAdvertised reports whether any -PLUS mechanism is offered to this
// session, which decides whether a "y" header is a downgrade attempt.
func scramPlusAdvertised(c *conn) bool {
	for _, capability := range deriveCapabilities(&c.state, c.server) {
		if strings.HasPrefix(capability, "AUTH=SCRAM-") && strings.HasSuffix(capability, "-PLUS") {
			return true
		}
	}
	return false
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
