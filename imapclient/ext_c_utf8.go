package imapclient

import (
	"strings"
)

// UTF8 capabilities. UTF8=ACCEPT is RFC 9755 (which obsoletes RFC 6855);
// UTF8=ALL / APPEND / USER originate in RFC 5738 and survive as capability
// names servers still advertise. Do not cite RFC 6855 for new work.
const (
	CapabilityUTF8Accept = "UTF8=ACCEPT"
	CapabilityUTF8All    = "UTF8=ALL"
	CapabilityUTF8Append = "UTF8=APPEND"
	CapabilityUTF8Only   = "UTF8=ONLY"
	CapabilityUTF8User   = "UTF8=USER"
)

// EnableUTF8Accept issues ENABLE UTF8=ACCEPT. RFC 9755.
//
// On a successful ENABLE the wire codec switches mailbox names and other
// astrings from modified UTF-7 to raw UTF-8. That switch is performed by the
// T07 ENABLE completion hook; this method is the Group C entry point that
// documents the capability and the RFC number correctly.
//
// UTF8=ONLY (RFC 9755) means the server speaks only UTF-8: there is nothing to
// enable, and [Client.UTF8AcceptEnabled] is not set by the mere advertisement.
// Callers that see UTF8=ONLY should still send ENABLE UTF8=ACCEPT when the
// server also advertises ENABLE, because that is how the client and server
// agree the session is in UTF-8 mode.
func (c *Client) EnableUTF8Accept() *EnableCommand {
	return c.Enable(CapabilityUTF8Accept)
}

// UTF8AcceptEnabled reports whether ENABLE UTF8=ACCEPT has succeeded on this
// connection, so mailbox names and SEARCH strings may carry raw UTF-8.
func (c *Client) UTF8AcceptEnabled() bool {
	return c.utf8AcceptEnabled()
}

// UTF8AppendAllowed reports whether the server accepts UTF-8 message literals
// in APPEND — either because UTF8=ACCEPT / UTF8=ONLY is in play, or because
// the narrower UTF8=APPEND capability is advertised (RFC 5738).
//
// When this is true, callers that APPEND a message containing NUL or 8-bit
// data should set [AppendMessage.Binary] so the payload goes out as literal8.
func (c *Client) UTF8AppendAllowed() bool {
	if c.UTF8AcceptEnabled() || c.Supports(CapabilityUTF8Only) {
		return true
	}
	// UTF8=APPEND alone authorises UTF-8 APPEND literals without a prior
	// ENABLE. Merely advertising UTF8=ACCEPT does not: RFC 9755 makes the
	// ENABLE the session switch.
	return c.Supports(CapabilityUTF8Append)
}

// UTF8Only reports whether the server advertises UTF8=ONLY: it does not speak
// modified UTF-7 at all. RFC 9755.
func (c *Client) UTF8Only() bool {
	return c.Supports(CapabilityUTF8Only)
}

// utf8CapabilityKnown reports whether name is one of the UTF8=* capabilities
// this package documents. Used by tests.
func utf8CapabilityKnown(name string) bool {
	switch strings.ToUpper(name) {
	case CapabilityUTF8Accept, CapabilityUTF8All, CapabilityUTF8Append,
		CapabilityUTF8Only, CapabilityUTF8User:
		return true
	default:
		return false
	}
}
