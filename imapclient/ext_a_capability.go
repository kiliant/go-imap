package imapclient

import (
	"errors"
	"fmt"

	"github.com/kiliant/go-imap"
)

// ErrCapabilityNotAdvertised reports that a command or command argument was
// not sent because the server did not advertise the capability it requires and
// no faithful fallback exists.
//
// It is a sentinel, not an error type: it is always delivered wrapped in the
// single [imap.Error] type, as the Err field, so the one-error-type rule holds.
// Match it with [errors.Is]:
//
//	if errors.Is(err, imapclient.ErrCapabilityNotAdvertised) {
//		// the server cannot do this; choose another strategy
//	}
//
// Nothing is written to the connection when this error is returned. Sending a
// command a server never advertised is not merely rude: some servers close the
// connection on an unknown command instead of replying BAD, which would take
// the whole session down.
var ErrCapabilityNotAdvertised = errors.New("imapclient: server does not advertise the capability this operation requires")

// capabilityError builds the [imap.Error] wrapper around
// [ErrCapabilityNotAdvertised].
func capabilityError(operation, capability string) *imap.Error {
	return &imap.Error{
		Type: imap.ErrorTypeProtocol,
		Text: fmt.Sprintf("%s requires the %s capability, which the server does not advertise", operation, capability),
		Err:  ErrCapabilityNotAdvertised,
	}
}

// unsupportedCommand returns an already-failed command handle for a command
// that was never written because its capability is absent.
func unsupportedCommand(operation, capability string) *Command {
	return failedCommand(operation, capabilityError(operation, capability))
}

// supportsAny reports whether the server advertises at least one of the named
// capabilities. Several group A features are reachable either through their own
// capability or through IMAP4rev2 having been enabled.
func (c *Client) supportsAny(names ...string) bool {
	for _, name := range names {
		if c.hasCapability(name) {
			return true
		}
	}
	return false
}
