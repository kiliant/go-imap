package imapserver

import (
	"context"
	"fmt"

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

// NAMESPACE has no framework default, deliberately.
//
// An earlier version answered a backend that does not implement
// NamespaceSession with one personal namespace at prefix "" and a hardcoded "/"
// delimiter. That is wrong for any backend using another separator — Courier and
// Dovecot's Maildir++ layout both use "." — and it is wrong in the worst way: the
// delimiter in every LIST response comes from the backend through
// imap.ListData.Delimiter, so the invented NAMESPACE contradicted the server's
// own LIST output. A client that believed it would build mailbox paths that do
// not exist.
//
// The framework cannot guess the delimiter, so it does not: NAMESPACE is
// advertised only when the backend implements NamespaceSession and can answer
// for itself.

func init() {
	registerCapabilities(
		capabilityDescriptor{
			Name:            "NAMESPACE",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[NamespaceSession](),
		},
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
	if err := requireCapability(c, "NAMESPACE"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(NamespaceSession)
	if !ok {
		return c.writeBad(command.tag, "NAMESPACE is not available")
	}
	data, err := session.Namespace(ctx, nil)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if data == nil {
		return writeBackendError(c, command.tag, command.name,
			fmt.Errorf("imapserver: backend implements NamespaceSession but reported no namespaces"))
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
	// The session is discarded whether or not Close succeeded, and the state
	// transition happens before the error is reported. An error from Close does
	// not mean the session is still usable, and leaving it attached means the
	// connection's teardown closes it a second time — which a backend that
	// releases a pooled handle or decrements a refcount in Close experiences as
	// a double release, on an error path, where it is hardest to find.
	closeErr := c.state.session.Close(ctx)
	c.state.unauthenticate()
	if closeErr != nil {
		return writeBackendError(c, command.tag, command.name, closeErr)
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}
