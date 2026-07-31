package imapclient

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapsasl"
	"github.com/kiliant/go-imap/internal/imapwire"
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

	_ struct{}
}

// Login authenticates with the RFC 3501 LOGIN command. It refuses to send
// credentials when the server advertises LOGINDISABLED, or on a cleartext
// connection unless Options.AllowInsecureAuth was explicitly set.
func (c *Client) Login(ctx context.Context, username, password string) error {
	if err := c.prepareLogin(); err != nil {
		return err
	}
	cmd := c.beginAuthenticationCommand("LOGIN", func(enc *imapwire.Encoder) {
		enc.SP().String(username).SP().String(password)
	}, nil)
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

	// Serialise installation with the first command bytes. APPEND uses the
	// same interlock for synchronising literals, so no other continuation
	// consumer can replace this handler during the exchange.
	c.literalMu.Lock()
	defer c.literalMu.Unlock()
	clear := c.setContinuation(func(text string) error {
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
	cmd := c.beginAuthenticationCommand("AUTHENTICATE", func(enc *imapwire.Encoder) {
		enc.SP().Atom(name)
		if useInitial {
			enc.SP().Atom(encodeSASL(initial))
		}
	}, nil)
	if err := cmd.Wait(ctx); err != nil {
		clear()
		return redactAuthenticationError(err, username, password, opts.Token)
	}
	clear()
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

func (c *Client) builtinSASL(name, username, password string, opts *AuthenticateOptions) (*SASLMechanism, error) {
	toClient := func(m *imapsasl.Mechanism) *SASLMechanism {
		if m == nil {
			return nil
		}
		return &SASLMechanism{Next: m.Next}
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		if c.closeErr != nil {
			return c.closeErr
		}
		return netClosedError{}
	}
	c.enc.Atom(encodeSASL(response)).CRLF()
	if err := c.enc.Flush(); err != nil {
		return protocolError(err)
	}
	// AUTHENTICATE payloads are deliberately absent from traces.
	return nil
}

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
