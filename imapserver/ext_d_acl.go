package imapserver

import (
	"context"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ACL, RIGHTS= and LIST-MYRIGHTS (RFC 4314, RFC 8440).
//
// Rights are carried as [imap.ACLRights] strings in both directions. RFC 4314
// defines the base letters and the RIGHTS= capability advertises further sets,
// so a letter this library does not know passes through verbatim rather than
// being dropped by a closed type.

// ACLSession is the optional ACL support of RFC 4314. A Session implements it
// when the authenticated user can see or change access control lists.
//
// MyRights is separated from GetACL because RFC 4314 section 4 lets a user ask
// what they themselves may do without holding the "a" right needed to read the
// whole list.
type ACLSession interface {
	GetACL(ctx context.Context, mailbox string, options *ACLOptions) (*imap.ACLData, error)
	MyRights(ctx context.Context, mailbox string, options *ACLOptions) (imap.ACLRights, error)
	ListRights(ctx context.Context, mailbox, identifier string, options *ACLOptions) (*imap.ListRightsData, error)
}

// ACLSetSession is the optional right-changing half of RFC 4314, kept separate
// so a backend can expose access control lists read-only.
//
// SetACL applies a rights modification; an empty rights string with the default
// operation replaces the entry with nothing. DeleteACL removes an identifier's
// entry entirely, which is not the same as granting it no rights.
type ACLSetSession interface {
	SetACL(ctx context.Context, mailbox, identifier string, rights imap.ACLRights, options *ACLSetOptions) error
	DeleteACL(ctx context.Context, mailbox, identifier string, options *ACLOptions) error
}

// ACLOptions configures an ACL query. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type ACLOptions struct{ _ struct{} }

// ACLRightsOp selects how SETACL changes an identifier's rights.
type ACLRightsOp string

const (
	// ACLRightsSet replaces the identifier's rights.
	ACLRightsSet ACLRightsOp = ""
	// ACLRightsAdd adds the listed rights.
	ACLRightsAdd ACLRightsOp = "+"
	// ACLRightsRemove removes the listed rights.
	ACLRightsRemove ACLRightsOp = "-"
)

// ACLSetOptions configures SETACL. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type ACLSetOptions struct {
	// Op selects replace, add or remove semantics, from the leading "+" or "-"
	// of RFC 4314 section 3.1.
	Op ACLRightsOp `imapfeature:"acl"`
	_  struct{}
}

const featureACL featureID = "acl"

func init() {
	registerFeatures(featureDescriptor{
		ID:     featureACL,
		Active: func(_ *sessionState, advertised map[string]bool) bool { return advertised["ACL"] },
	})
	registerCapabilities(
		capabilityDescriptor{
			Name:            "ACL",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[ACLSession](),
		},
		// RFC 8440 adds MYRIGHTS as a LIST return option. The LIST return-option
		// plumbing is group A's; this advertises the capability and the
		// standalone MYRIGHTS command, which is the part that works today.
		capabilityDescriptor{
			Name:            "LIST-MYRIGHTS",
			States:          stateMaskAuthenticated | stateMaskSelected,
			Depends:         []string{"ACL"},
			RequiresBackend: sessionImplements[ACLSession](),
		},
	)
	registerCommand("GETACL", stateMaskAuthenticated|stateMaskSelected, false, parseSingleAstring, handleGetACL)
	registerCommand("MYRIGHTS", stateMaskAuthenticated|stateMaskSelected, false, parseSingleAstring, handleMyRights)
	registerCommand("LISTRIGHTS", stateMaskAuthenticated|stateMaskSelected, false, parseTwoAstrings, handleListRights)
	registerCommand("DELETEACL", stateMaskAuthenticated|stateMaskSelected, false, parseTwoAstrings, handleDeleteACL)
	registerCommand("SETACL", stateMaskAuthenticated|stateMaskSelected, false, parseSetACL, handleSetACL)
}

type twoAstringArgs struct{ first, second string }

func parseTwoAstrings(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &twoAstringArgs{}
	if !decoder.ExpectAstring(&args.first) || !decoder.ExpectSP() ||
		!decoder.ExpectAstring(&args.second) || !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(args.first) + len(args.second)), nil
}

type setACLArgs struct {
	mailbox    string
	identifier string
	op         ACLRightsOp
	rights     imap.ACLRights
}

func parseSetACL(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &setACLArgs{}
	var rights string
	if !decoder.ExpectAstring(&args.mailbox) || !decoder.ExpectSP() ||
		!decoder.ExpectAstring(&args.identifier) || !decoder.ExpectSP() ||
		!decoder.ExpectAstring(&rights) || !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	// RFC 4314 section 3.1: a leading "+" or "-" makes the modification
	// additive or subtractive rather than a replacement.
	switch {
	case len(rights) > 0 && rights[0] == '+':
		args.op, rights = ACLRightsAdd, rights[1:]
	case len(rights) > 0 && rights[0] == '-':
		args.op, rights = ACLRightsRemove, rights[1:]
	}
	args.rights = imap.ACLRights(rights)
	return args, int64(len(args.mailbox) + len(args.identifier) + len(rights)), nil
}

func handleGetACL(ctx context.Context, c *conn, command *queuedCommand) error {
	mailbox, _ := command.args.(string)
	if err := requireCapability(c, "ACL"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(ACLSession)
	if !ok {
		return c.writeBad(command.tag, "ACL is not available")
	}
	data, err := session.GetACL(ctx, mailbox, nil)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if data != nil {
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("ACL").SP().Mailbox(data.Mailbox)
		for _, entry := range data.Entries {
			c.encoder.SP().Astring(entry.Identifier).SP().Astring(string(entry.Rights))
		}
		c.encoder.CRLF()
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func handleMyRights(ctx context.Context, c *conn, command *queuedCommand) error {
	mailbox, _ := command.args.(string)
	if err := requireCapability(c, "ACL"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(ACLSession)
	if !ok {
		return c.writeBad(command.tag, "ACL is not available")
	}
	rights, err := session.MyRights(ctx, mailbox, nil)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("MYRIGHTS").SP().
		Mailbox(mailbox).SP().Astring(string(rights)).CRLF()
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func handleListRights(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*twoAstringArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid LISTRIGHTS arguments")
	}
	if err := requireCapability(c, "ACL"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(ACLSession)
	if !ok {
		return c.writeBad(command.tag, "ACL is not available")
	}
	data, err := session.ListRights(ctx, args.first, args.second, nil)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if data != nil {
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("LISTRIGHTS").SP().
			Mailbox(args.first).SP().Astring(args.second).SP().Astring(string(data.Required))
		// Each optional group is a separate token: RFC 4314 section 3.7 uses
		// that to say which rights may be granted individually.
		for _, optional := range data.Optional {
			c.encoder.SP().Astring(string(optional))
		}
		c.encoder.CRLF()
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func handleDeleteACL(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*twoAstringArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid DELETEACL arguments")
	}
	if err := requireCapability(c, "ACL"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(ACLSetSession)
	if !ok {
		return c.writeBad(command.tag, "ACL modification is not available")
	}
	if err := session.DeleteACL(ctx, args.first, args.second, nil); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func handleSetACL(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*setACLArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid SETACL arguments")
	}
	if err := requireCapability(c, "ACL"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(ACLSetSession)
	if !ok {
		return c.writeBad(command.tag, "ACL modification is not available")
	}
	if err := session.SetACL(ctx, args.mailbox, args.identifier, args.rights, &ACLSetOptions{Op: args.op}); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}
