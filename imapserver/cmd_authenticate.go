package imapserver

import (
	"context"
	"crypto/tls"
	"slices"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type loginArgs struct {
	username string
	password string
}

type authenticateArgs struct {
	mechanism  string
	initial    string
	hasInitial bool
}

func parseLogin(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	var args loginArgs
	if !decoder.ExpectAstring(&args.username) || !decoder.ExpectSP() || !decoder.ExpectAstring(&args.password) || !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return &args, int64(len(args.username) + len(args.password)), nil
}

func parseAuthenticate(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	var args authenticateArgs
	if !decoder.ExpectAtom(&args.mechanism) {
		return nil, 0, decoder.Err()
	}
	args.mechanism = strings.ToUpper(args.mechanism)
	if decoder.SP() {
		if !decoder.ExpectAtom(&args.initial) {
			return nil, 0, decoder.Err()
		}
		args.hasInitial = true
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return &args, int64(len(args.mechanism) + len(args.initial)), nil
}

func handleLogin(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*loginArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid LOGIN arguments")
	}
	if !authenticationTransportAllowed(c, "LOGIN") {
		return writeTaggedCondition(c, command.tag, "NO", imap.CodePrivacyRequired, "", "LOGIN requires transport security")
	}
	credentials := &Credentials{Mechanism: "LOGIN", Username: args.username, Password: args.password}
	return authenticateBackend(ctx, c, command.tag, credentials)
}

func handleAuthenticate(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*authenticateArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid AUTHENTICATE arguments")
	}
	// SCRAM verifies the client's proof in the framework against material only
	// the backend holds, so it does not fit the extract-credentials-and-ask
	// shape the other mechanisms share. See ext_d_scram.go.
	if isSCRAMMechanism(args.mechanism) {
		if c.server == nil || c.server.backend == nil {
			return writeTaggedCondition(c, command.tag, "NO", imap.CodeUnavailable, "", "authentication is unavailable")
		}
		if !authenticationTransportAllowed(c, args.mechanism) {
			return writeTaggedCondition(c, command.tag, "NO", imap.CodePrivacyRequired, "", "authentication mechanism requires transport security")
		}
		if !slices.Contains(deriveCapabilities(&c.state, c.server), "AUTH="+strings.ToUpper(args.mechanism)) {
			return writeTaggedCondition(c, command.tag, "NO", imap.CodeCannot, "", "authentication mechanism is not available")
		}
		return handleSCRAMAuthenticate(ctx, c, command, args)
	}
	server, err := newSASLServer(args.mechanism)
	if err != nil {
		return writeTaggedCondition(c, command.tag, "NO", imap.CodeCannot, "", "authentication mechanism is not supported")
	}
	if c.server == nil || c.server.backend == nil {
		return writeTaggedCondition(c, command.tag, "NO", imap.CodeUnavailable, "", "authentication is unavailable")
	}
	if !authenticationTransportAllowed(c, args.mechanism) {
		return writeTaggedCondition(c, command.tag, "NO", imap.CodePrivacyRequired, "", "authentication mechanism requires transport security")
	}
	capability := "AUTH=" + args.mechanism
	if !slices.Contains(deriveCapabilities(&c.state, c.server), capability) {
		return writeTaggedCondition(c, command.tag, "NO", imap.CodeCannot, "", "authentication mechanism is not available")
	}

	var response []byte
	if args.hasInitial {
		var aborted bool
		response, aborted, err = decodeSASL(args.initial)
		if err != nil {
			return c.writeBad(command.tag, "invalid SASL initial response")
		}
		if aborted {
			return c.writeBad(command.tag, "AUTHENTICATE cancelled")
		}
	} else {
		var aborted bool
		response, aborted, err = c.continueSASL(ctx, server.initialChallenge())
		if err != nil {
			return err
		}
		if aborted {
			return c.writeBad(command.tag, "AUTHENTICATE cancelled")
		}
	}

	for step := 0; step < maxSASLSteps; step++ {
		credentials, challenge, err := server.next(response)
		if err != nil {
			return c.writeBad(command.tag, "invalid SASL response")
		}
		if credentials != nil {
			return authenticateBackend(ctx, c, command.tag, credentials)
		}
		var aborted bool
		response, aborted, err = c.continueSASL(ctx, challenge)
		if err != nil {
			return err
		}
		if aborted {
			return c.writeBad(command.tag, "AUTHENTICATE cancelled")
		}
	}
	return c.writeBad(command.tag, "too many SASL continuation steps")
}

func authenticationTransportAllowed(c *conn, mechanism string) bool {
	if c == nil || c.server == nil || c.server.backend == nil {
		return false
	}
	mechanism = strings.ToUpper(mechanism)
	if mechanism == "XOAUTH2" || mechanism == "OAUTHBEARER" {
		return c.state.tls
	}
	// SCRAM is allowed on cleartext without the AllowInsecureAuth opt-in that
	// PLAIN and LOGIN need, because it does not put the password on the wire:
	// the client sends a proof derived from it and the server never learns it.
	// A passive observer of a cleartext SCRAM exchange gains nothing usable.
	//
	// That is not the same as SCRAM being safe against an *active* attacker,
	// who can strip the mechanism list down to PLAIN — the defence against
	// that is channel binding, and this server does not advertise the -PLUS
	// variants. See ext_d_scram.go for why.
	if isSCRAMMechanism(mechanism) {
		return true
	}
	if mechanism != "PLAIN" && mechanism != "LOGIN" {
		return false
	}
	return c.state.tls || !c.server.options.RequireTLS && c.server.options.AllowInsecureAuth
}

func authenticateBackend(ctx context.Context, c *conn, tag string, credentials *Credentials) error {
	if c.server.backend == nil {
		return writeTaggedCondition(c, tag, "NO", imap.CodeUnavailable, "", "authentication is unavailable")
	}
	session, err := c.server.backend.Authenticate(ctx, connectionInfo(c), credentials, nil)
	if err != nil {
		return writeBackendError(c, tag, "authentication", err)
	}
	if session == nil {
		return writeTaggedCondition(c, tag, "NO", imap.CodeServerBug, "", "authentication returned no session")
	}
	if !c.state.authenticate(session) {
		_ = session.Close(ctx)
		return writeTaggedCondition(c, tag, "NO", imap.CodeServerBug, "", "authentication state transition failed")
	}
	return c.writeTagged(tag, "OK", "authentication completed")
}

func connectionInfo(c *conn) *ConnInfo {
	info := &ConnInfo{}
	if c == nil || c.raw == nil {
		return info
	}
	info.LocalAddr = c.raw.LocalAddr()
	info.RemoteAddr = c.raw.RemoteAddr()
	if state, ok := connectionTLSState(c.currentTransport()); ok {
		info.TLS = &state
	}
	return info
}

func connectionTLSState(transport any) (tls.ConnectionState, bool) {
	switch transport := transport.(type) {
	case *tls.Conn:
		state := transport.ConnectionState()
		return state, true
	case *compressedConn:
		return connectionTLSState(transport.Conn)
	default:
		return tls.ConnectionState{}, false
	}
}
