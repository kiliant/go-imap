package imapclient

import (
	"strings"

	"github.com/kiliant/go-imap"
)

// UIDOnlyEnabled reports whether ENABLE UIDONLY succeeded in this session.
// UIDONLY, RFC 9586.
//
// After UIDONLY is enabled the server suppresses sequence numbers: EXPUNGE
// becomes VANISHED, and sequence-number forms of FETCH/STORE/SEARCH/COPY/MOVE
// are answered with [imap.CodeUIDRequired]. Callers must use the UID command
// variants and [UnilateralDataHandler.Vanished].
//
// Core FETCH responses still carry a SeqNum field populated from the wire
// when a number is present. Under UIDONLY that number is absent; do not
// assume SeqNum is meaningful. Redesigning seqnum-free FETCH data is a core
// (api-guardian) change outside this extension's ownership.
func (c *Client) UIDOnlyEnabled() bool {
	return c.hasEnabled("UIDONLY")
}

// ErrUIDRequired is returned locally when a sequence-number command is
// refused because UIDONLY is enabled. It wraps into [imap.Error] with
// [imap.CodeUIDRequired] so callers can match either form.
func uidRequiredError(operation string) *imap.Error {
	return &imap.Error{
		Type: imap.ErrorTypeProtocol,
		Code: imap.CodeUIDRequired,
		Text: operation + " is not available after ENABLE UIDONLY; use the UID form",
	}
}

// requireUIDCommands reports whether sequence-number message commands must be
// refused locally. Used by extension helpers that share this package; core
// FETCH/STORE are not rewritten here.
func (c *Client) requireUIDCommands() bool {
	return c.UIDOnlyEnabled()
}

// parseUIDRequiredArgs is a no-op placeholder for response-code symmetry;
// UIDREQUIRED takes no arguments (RFC 9586).
func parseUIDRequiredArgs(args string) error {
	if strings.TrimSpace(args) != "" {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "UIDREQUIRED response code must not carry arguments"}
	}
	return nil
}
