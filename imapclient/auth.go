package imapclient

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapsasl"
	"github.com/kiliant/go-imap/internal/imapwire"
	"github.com/kiliant/go-imap/internal/saslprep"
)

// SASLMechanism is a caller-supplied SASL exchange. Next receives each decoded
// server challenge and returns the decoded client response. A nil response to
// the initial nil challenge means this mechanism has no initial response; an
// empty, non-nil response is encoded as "=" when SASL-IR is used.
//
// Construct with keyed fields only; fields may be added in a future release.
type SASLMechanism struct {
	// Next advances the mechanism by one server challenge.
	Next func(challenge []byte) (response []byte, err error)
	_    struct{}
}

// AuthenticateOptions controls SASL authentication. The zero value selects the
// strongest supported built-in mechanism and uses password as its credential.
//
// Construct with keyed fields only; fields may be added in a future release.
type AuthenticateOptions struct {
	// Mechanism explicitly selects a server-advertised SASL mechanism. It is
	// case-insensitive. With an empty value Authenticate prefers, in order:
	// SCRAM-SHA-256-PLUS, SCRAM-SHA-256, SCRAM-SHA-1-PLUS, SCRAM-SHA-1,
	// OAUTHBEARER, XOAUTH2, CRAM-MD5, PLAIN, then LOGIN.
	Mechanism string

	// Token supplies the bearer token for XOAUTH2 or OAUTHBEARER. If it is
	// empty, password is used as the token for compatibility with simple
	// credential stores.
	Token string

	// OAuthHost and OAuthPort add the optional host and port fields to an
	// OAUTHBEARER initial response.
	OAuthHost string
	OAuthPort string

	// SASL supplies a custom mechanism for a server-advertised mechanism name
	// which the built-ins do not implement. It is called only after selection
	// and must return a non-nil mechanism with a non-nil Next function.
	SASL func(name string) (*SASLMechanism, error)

	// PrepareCredentials applies SASLprep (RFC 4013) to username and
	// password before the selected built-in mechanism sees them. It is
	// false by default.
	//
	// RFC 5802 requires SASLprep for SCRAM and RFC 4616 recommends it for
	// PLAIN, but many deployed servers store and compare the raw password
	// octets: enabling this against such a server breaks any password
	// containing a character that SASLprep changes. Enable it only when
	// the server is known to normalize credentials at enrollment.
	PrepareCredentials bool

	_ struct{}
}

// Login authenticates with the RFC 3501 LOGIN command. It refuses to send
// credentials when the server advertises LOGINDISABLED, or on a cleartext
// connection unless Options.AllowInsecureAuth was explicitly set.
//
// Login does not prepare (SASLprep, RFC 4013) username or password: the
// command has no options struct and adding a parameter would be a
// breaking change. Callers that need that transformation should use
// [Client.Authenticate] with [AuthenticateOptions.PrepareCredentials]
// instead.
func (c *Client) Login(ctx context.Context, username, password string) error {
	if err := c.prepareLogin(); err != nil {
		return err
	}
	cmd := c.beginAuthenticationCommand("LOGIN", func(enc *imapwire.Encoder) {
		enc.SP().String(username).SP().String(password)
	}, nil, false)
	if err := cmd.Wait(ctx); err != nil {
		return redactAuthenticationError(err, username, password)
	}
	c.authenticationSucceeded()
	return c.requestCapability(ctx)
}

// Authenticate authenticates using a SASL mechanism advertised by the server.
// password is used by password mechanisms; OAuth mechanisms use opts.Token
// when supplied. A nil opts selects the strongest advertised built-in
// mechanism with default settings. Authenticate refuses PLAIN and LOGIN over
// cleartext unless Options.AllowInsecureAuth was explicitly set.
func (c *Client) Authenticate(ctx context.Context, username, password string, opts *AuthenticateOptions) error {
	if opts == nil {
		opts = &AuthenticateOptions{}
	}
	name, mechanism, err := c.selectSASL(username, password, opts)
	if err != nil {
		return err
	}
	if (name == "PLAIN" || name == "LOGIN") && !c.tlsActive() && !c.opts.AllowInsecureAuth {
		return authProtocolError("refusing %s authentication over a cleartext connection", name)
	}

	var initial []byte
	useInitial := c.hasCapability("SASL-IR")
	if useInitial {
		initial, err = mechanism.Next(nil)
		if err != nil {
			return authMechanismError(err)
		}
		useInitial = initial != nil
	}

	// Serialise installation with the first command bytes so that no other
	// continuation consumer can replace this handler during the exchange.
	c.continuationOwnerMu.Lock()
	// A server that keeps sending continuation requests must not be able to
	// drive a mechanism forever. Every built-in mechanism refuses an
	// unexpected extra step, but a caller-supplied one need not.
	steps := 0
	clearHandler := c.setContinuation(func(text string) error {
		steps++
		if steps > maxSASLContinuations {
			return authProtocolError("server sent more than %d SASL continuation requests", maxSASLContinuations)
		}
		challenge, err := decodeSASL(text)
		if err != nil {
			return err
		}
		response, err := mechanism.Next(challenge)
		if err != nil {
			return authMechanismError(err)
		}
		return c.writeSASLResponse(response)
	})
	// Ownership of the continuation slot ends with the tagged completion, and
	// must end before the post-authentication CAPABILITY: that command claims
	// the slot for its own arguments like any other.
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			clearHandler()
			c.continuationOwnerMu.Unlock()
		})
	}
	defer release()
	cmd := c.beginAuthenticationCommand("AUTHENTICATE", func(enc *imapwire.Encoder) {
		enc.SP().Atom(name)
		if useInitial {
			enc.SP().Atom(encodeSASL(initial))
		}
	}, nil, true)
	if err := cmd.Wait(ctx); err != nil {
		release()
		return redactAuthenticationError(err, authenticationSecrets(name, username, password, opts)...)
	}
	release()
	c.authenticationSucceeded()
	return c.requestCapability(ctx)
}

func (c *Client) prepareLogin() error {
	if c.hasCapability("LOGINDISABLED") {
		return authProtocolError("server disables the LOGIN command")
	}
	if !c.tlsActive() && !c.opts.AllowInsecureAuth {
		return authProtocolError("refusing LOGIN authentication over a cleartext connection")
	}
	return nil
}

func (c *Client) selectSASL(username, password string, opts *AuthenticateOptions) (string, *SASLMechanism, error) {
	name := strings.ToUpper(opts.Mechanism)
	if name == "" {
		for _, candidate := range saslPreference {
			if c.hasCapability("AUTH=" + candidate) {
				name = candidate
				break
			}
		}
	}
	if name == "" {
		return "", nil, authProtocolError("server advertises no supported SASL mechanism")
	}
	if !c.hasCapability("AUTH=" + name) {
		return "", nil, authProtocolError("server does not advertise SASL mechanism %s", name)
	}

	if builtin, err := c.builtinSASL(name, username, password, opts); builtin != nil || err != nil {
		return name, builtin, err
	}
	if opts.SASL == nil {
		return "", nil, authProtocolError("unsupported SASL mechanism %s", name)
	}
	custom, err := opts.SASL(name)
	if err != nil {
		return "", nil, authMechanismError(err)
	}
	if custom == nil || custom.Next == nil {
		return "", nil, authProtocolError("custom SASL mechanism %s is invalid", name)
	}
	return name, custom, nil
}

var saslPreference = []string{
	"SCRAM-SHA-256-PLUS",
	"SCRAM-SHA-256",
	"SCRAM-SHA-1-PLUS",
	"SCRAM-SHA-1",
	"OAUTHBEARER",
	"XOAUTH2",
	"CRAM-MD5",
	"PLAIN",
	"LOGIN",
}

// passwordMechanisms lists the built-in mechanisms whose credentials
// AuthenticateOptions.PrepareCredentials applies to: every built-in
// mechanism that sends a username and/or password chosen by the caller,
// as opposed to an opaque bearer token.
//
// CRAM-MD5 (RFC 2195) predates stringprep entirely and RFC 2195 mandates no
// normalization of its own. It is included here anyway: PrepareCredentials
// is an explicit caller opt-in that means "prepare my credentials before
// any password mechanism sends them", and applying it uniformly across
// every password mechanism is more predictable than silently exempting
// one of them for a reason the caller has no way to discover. This is a
// judgement call, not something RFC 2195 requires or forbids.
var passwordMechanisms = map[string]bool{
	"PLAIN":              true,
	"LOGIN":              true,
	"CRAM-MD5":           true,
	"SCRAM-SHA-1":        true,
	"SCRAM-SHA-256":      true,
	"SCRAM-SHA-1-PLUS":   true,
	"SCRAM-SHA-256-PLUS": true,
}

// prepareCredentials applies SASLprep (RFC 4013) to username and password
// when opts.PrepareCredentials is set, using the query form ([Prepare],
// not [PrepareStored]): Authenticate is sending a credential, not
// enrolling one. With the option unset (the default) it returns username
// and password unchanged, byte for byte.
//
// A SASLprep failure (a prohibited code point or a bidi violation) is
// reported as a protocol error before any mechanism is constructed, so
// nothing reaches the wire. The underlying saslprep error names only the
// offending code point and never the credential itself; that property
// must survive here too, so do not add the username or password to this
// error.
func prepareCredentials(opts *AuthenticateOptions, username, password string) (string, string, error) {
	if !opts.PrepareCredentials {
		return username, password, nil
	}
	preparedUsername, err := saslprep.Prepare(username)
	if err != nil {
		return "", "", authProtocolError("SASLprep rejected the user name: %v", err)
	}
	preparedPassword, err := saslprep.Prepare(password)
	if err != nil {
		return "", "", authProtocolError("SASLprep rejected the password: %v", err)
	}
	return preparedUsername, preparedPassword, nil
}

// authenticationSecrets returns every credential form that may have reached
// the server, for redaction out of a failed command's text. With
// PrepareCredentials set, the octets on the wire are the prepared ones, so
// redacting only what the caller passed in would leave a server that echoes
// the authentication identity in its NO text unredacted.
func authenticationSecrets(name, username, password string, opts *AuthenticateOptions) []string {
	secrets := []string{username, password, opts.Token}
	if !opts.PrepareCredentials || !passwordMechanisms[name] {
		return secrets
	}
	// The same call already succeeded during mechanism construction, so an
	// error here means nothing was sent under the prepared form.
	preparedUsername, preparedPassword, err := prepareCredentials(opts, username, password)
	if err != nil {
		return secrets
	}
	return append(secrets, preparedUsername, preparedPassword)
}

func (c *Client) builtinSASL(name, username, password string, opts *AuthenticateOptions) (*SASLMechanism, error) {
	toClient := func(m *imapsasl.Mechanism) *SASLMechanism {
		if m == nil {
			return nil
		}
		return &SASLMechanism{Next: m.Next}
	}
	if passwordMechanisms[name] {
		var err error
		username, password, err = prepareCredentials(opts, username, password)
		if err != nil {
			return nil, err
		}
	}
	switch name {
	case "PLAIN":
		return toClient(imapsasl.Plain(username, password)), nil
	case "LOGIN":
		return toClient(imapsasl.Login(username, password)), nil
	case "CRAM-MD5":
		return toClient(imapsasl.CRAMMD5(username, password)), nil
	case "XOAUTH2":
		token := opts.Token
		if token == "" {
			token = password
		}
		return toClient(imapsasl.XOAUTH2(username, token)), nil
	case "OAUTHBEARER":
		token := opts.Token
		if token == "" {
			token = password
		}
		return toClient(imapsasl.OAUTHBEARER(username, token, opts.OAuthHost, opts.OAuthPort)), nil
	case "SCRAM-SHA-1":
		m, err := imapsasl.SCRAMSHA1(username, password)
		return toClient(m), err
	case "SCRAM-SHA-256":
		m, err := imapsasl.SCRAMSHA256(username, password)
		return toClient(m), err
	case "SCRAM-SHA-1-PLUS", "SCRAM-SHA-256-PLUS":
		binding, err := c.tlsChannelBinding()
		if err != nil {
			return nil, err
		}
		if name == "SCRAM-SHA-1-PLUS" {
			m, err := imapsasl.SCRAMSHA1Plus(username, password, binding)
			return toClient(m), err
		}
		m, err := imapsasl.SCRAMSHA256Plus(username, password, binding)
		return toClient(m), err
	default:
		return nil, nil
	}
}

func (c *Client) authenticationSucceeded() {
	c.mu.Lock()
	if !c.closed {
		c.state = StateAuthenticated
		c.caps = make(map[string]struct{})
		c.enabled = make(map[string]struct{})
		c.enc.SetUTF8Accept(false)
		c.dec.SetUTF8Accept(false)
	}
	c.mu.Unlock()
}

func (c *Client) tlsActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.conn.(*tls.Conn)
	return ok
}

func (c *Client) tlsChannelBinding() ([]byte, error) {
	c.mu.Lock()
	tlsConn, ok := c.conn.(*tls.Conn)
	c.mu.Unlock()
	if !ok {
		return nil, authProtocolError("SCRAM-PLUS requires a TLS connection")
	}
	state := tlsConn.ConnectionState()
	binding, err := state.ExportKeyingMaterial("EXPORTER-Channel-Binding", nil, 32)
	if err != nil {
		return nil, fmt.Errorf("imapclient: exporting TLS channel binding: %w", err)
	}
	return binding, nil
}

func decodeSASL(text string) ([]byte, error) {
	if text == "" || text == "=" {
		return []byte{}, nil
	}
	value, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, authProtocolError("invalid base64 SASL challenge")
	}
	return value, nil
}

func encodeSASL(value []byte) string {
	if len(value) == 0 {
		return "="
	}
	return base64.StdEncoding.EncodeToString(value)
}

func (c *Client) writeSASLResponse(response []byte) error {
	// writeMu, not mu: the reader goroutine runs this, and mu must stay
	// available to the rest of the session while it does.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		if err != nil {
			return err
		}
		return netClosedError{}
	}
	enc := c.enc
	c.mu.Unlock()
	enc.Atom(encodeSASL(response)).CRLF()
	if err := enc.Flush(); err != nil {
		return protocolError(err)
	}
	// AUTHENTICATE payloads are deliberately absent from traces.
	return nil
}

// maxSASLContinuations bounds one AUTHENTICATE exchange. No SASL mechanism in
// use over IMAP needs anything close to this many round trips.
const maxSASLContinuations = 16

type netClosedError struct{}

func (netClosedError) Error() string { return "network connection is closed" }

func authProtocolError(format string, args ...any) *imap.Error {
	return &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf(format, args...)}
}

func authMechanismError(err error) *imap.Error {
	var ierr *imap.Error
	if errors.As(err, &ierr) {
		return ierr
	}
	// Mechanisms must not put credentials into their errors. Keep the public
	// error deliberately generic so a custom mechanism cannot accidentally turn
	// a password or bearer token into application logs.
	return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "SASL mechanism failed"}
}

func redactAuthenticationError(err error, secrets ...string) error {
	var ierr *imap.Error
	if !errors.As(err, &ierr) {
		return err
	}
	copy := *ierr
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		copy.Text = strings.ReplaceAll(copy.Text, secret, "[redacted]")
		copy.CodeArgs = strings.ReplaceAll(copy.CodeArgs, secret, "[redacted]")
	}
	return &copy
}
