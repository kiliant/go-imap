package imapserver

import (
	"context"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// NAMESPACE (RFC 2342), UNAUTHENTICATE (RFC 8437) and the group D capabilities
// that are advertisement or response shape only.

// NamespaceSession is the optional NAMESPACE support of RFC 2342.
//
// It is a Session interface rather than server configuration because namespaces
// are usually a property of the authenticated user: which shared and other-user
// namespaces exist differs per account. A backend that does not implement it
// gets the framework's configured default, which is the single personal
// namespace almost every server presents.
type NamespaceSession interface {
	Namespace(ctx context.Context, options *NamespaceOptions) (*imap.NamespaceData, error)
}

// NamespaceOptions configures NAMESPACE. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type NamespaceOptions struct{ _ struct{} }

// UnauthenticateSession is the optional UNAUTHENTICATE support of RFC 8437:
// returning an authenticated connection to the not-authenticated state so it
// can be reused for another user, without the cost of a new TLS handshake.
type UnauthenticateSession interface {
	Unauthenticate(ctx context.Context, options *UnauthenticateOptions) error
}

// UnauthenticateOptions configures UNAUTHENTICATE. A nil pointer selects the
// defaults.
// Construct with keyed fields only; fields may be added in a future release.
type UnauthenticateOptions struct{ _ struct{} }

// defaultNamespace is what a backend that does not implement NamespaceSession
// presents: one personal namespace at the root, with the hierarchy delimiter
// this framework uses.
var defaultNamespace = imap.NamespaceData{
	Personal: []imap.NamespaceDescriptor{{Prefix: "", Delimiter: '/'}},
}

func init() {
	registerCapabilities(
		// NAMESPACE is always answerable: the framework has a default for a
		// backend that does not implement the interface, so unlike the other
		// group D capabilities this one needs no witness.
		capabilityDescriptor{Name: "NAMESPACE", States: stateMaskAuthenticated | stateMaskSelected},
		capabilityDescriptor{
			Name:            "UNAUTHENTICATE",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[UnauthenticateSession](),
		},
		// JMAPACCESS (RFC 9698) advertises that the account is also reachable
		// over JMAP. It is a pure advertisement plus a response code, so the
		// backend witnesses it by name.
		capabilityDescriptor{
			Name:            "JMAPACCESS",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("JMAPACCESS"),
		},
		// INPROGRESS (RFC 9585) lets a server send untagged OK progress
		// updates during a long command. Advertising it commits only to the
		// response shape being understood, which the framework owns.
		capabilityDescriptor{Name: "INPROGRESS", States: stateMaskAny},
	)
	registerCommand("NAMESPACE", stateMaskAuthenticated|stateMaskSelected, false, parseNoArgs, handleNamespace)
	registerCommand("UNAUTHENTICATE", stateMaskAuthenticated|stateMaskSelected, true, parseNoArgs, handleUnauthenticate)
}

func handleNamespace(ctx context.Context, c *conn, command *queuedCommand) error {
	data := &defaultNamespace
	if session, ok := c.state.session.(NamespaceSession); ok {
		reported, err := session.Namespace(ctx, nil)
		if err != nil {
			return writeBackendError(c, command.tag, command.name, err)
		}
		if reported != nil {
			data = reported
		}
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("NAMESPACE")
	for _, set := range [][]imap.NamespaceDescriptor{data.Personal, data.OtherUsers, data.Shared} {
		c.encoder.SP()
		// An absent namespace class is NIL, which is not the same as an empty
		// list: RFC 2342 section 5 uses NIL for "this server has no such
		// namespace" and "()" would claim one exists but is empty.
		if len(set) == 0 {
			c.encoder.NIL()
			continue
		}
		c.encoder.List(len(set), func(i int) {
			c.encoder.Special('(').String(set[i].Prefix).SP()
			if set[i].Delimiter == 0 {
				c.encoder.NIL()
			} else {
				c.encoder.String(string(set[i].Delimiter))
			}
			c.encoder.Special(')')
		})
	}
	c.encoder.CRLF()
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func handleUnauthenticate(ctx context.Context, c *conn, command *queuedCommand) error {
	if err := requireCapability(c, "UNAUTHENTICATE"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(UnauthenticateSession)
	if !ok {
		return c.writeBad(command.tag, "UNAUTHENTICATE is not available")
	}
	// Any selection is abandoned first. RFC 8437 section 3 returns the
	// connection to the not-authenticated state, in which a selected mailbox
	// cannot exist, and leaving the backend handle attached would leak it.
	if err := abandonCurrentSelection(ctx, c); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if err := session.Unauthenticate(ctx, nil); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if err := c.state.session.Close(ctx); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	c.state.unauthenticate()
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}
