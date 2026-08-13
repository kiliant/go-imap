package imapserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// METADATA and METADATA-SERVER (RFC 5464).
//
// Entry names are an open tree, carried as [imap.MetadataEntryName]. A nil
// value means NIL — the entry is unset, and in SETMETADATA means "remove it".
// An empty string is a present, empty value. The two must not be conflated:
// RFC 5464 section 4.3 makes removal and blanking different operations.

// MetadataSession is the optional METADATA support of RFC 5464.
//
// An empty mailbox name addresses the server annotation space, which is what
// METADATA-SERVER advertises. Both scopes go through the same methods because
// RFC 5464 gives them the same syntax and the same entry tree; a backend that
// serves only one returns nothing for the other.
type MetadataSession interface {
	GetMetadata(ctx context.Context, mailbox string, entries []imap.MetadataEntryName, options *MetadataOptions) (*imap.MailboxMetadata, error)
	SetMetadata(ctx context.Context, mailbox string, entries []imap.MetadataEntry, options *MetadataOptions) error
}

// MetadataOptions configures a METADATA operation. A nil pointer selects the
// defaults.
// Construct with keyed fields only; fields may be added in a future release.
type MetadataOptions struct {
	// Depth limits how far below each requested entry the server descends:
	// "0", "1" or "infinity" per RFC 5464 section 4.2.2. Empty means "0".
	Depth string `imapfeature:"metadata"`
	// MaxSize suppresses values larger than this many octets.
	// RFC 5464 section 4.2.2.
	//
	// HasMaxSize carries presence separately: MAXSIZE 0 is a real request —
	// it suppresses every value and asks for them all to be named in
	// METADATA LONGENTRIES — and is not the same as no MAXSIZE at all.
	MaxSize uint32 `imapfeature:"metadata"`
	// HasMaxSize reports whether the client supplied MAXSIZE.
	HasMaxSize bool `imapfeature:"metadata"`
	_          struct{}
}

const featureMetadata featureID = "metadata"

func init() {
	registerFeatures(featureDescriptor{
		ID: featureMetadata,
		Active: func(_ *sessionState, advertised map[string]bool) bool {
			return advertised["METADATA"] || advertised["METADATA-SERVER"]
		},
	})
	registerCapabilities(
		capabilityDescriptor{
			Name:            "METADATA",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[MetadataSession](),
		},
		// METADATA-SERVER is the weaker claim: server-scope annotations only.
		// A backend implementing the interface can serve both, so METADATA
		// implies it rather than the two being alternatives.
		// RFC 9590's LIST return option. See ext_d_listret.go.
		capabilityDescriptor{
			Name:            "LIST-METADATA",
			States:          stateMaskAuthenticated | stateMaskSelected,
			Depends:         []string{"METADATA"},
			RequiresBackend: sessionImplements[MetadataSession](),
		},
		capabilityDescriptor{
			Name:            "METADATA-SERVER",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[MetadataSession](),
		},
	)
	registerCommand("GETMETADATA", stateMaskAuthenticated|stateMaskSelected, false, parseGetMetadata, handleGetMetadata)
	registerCommand("SETMETADATA", stateMaskAuthenticated|stateMaskSelected, false, parseSetMetadata, handleSetMetadata)
}

type getMetadataArgs struct {
	mailbox string
	entries []imap.MetadataEntryName
	options MetadataOptions
}

func parseGetMetadata(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &getMetadataArgs{}
	// The option list, when present, precedes the mailbox name.
	if decoder.PeekSpecial('(') {
		if err := decoder.ExpectList(func() error {
			var name string
			if !decoder.ExpectAtom(&name) || !decoder.ExpectSP() {
				return decoder.Err()
			}
			switch strings.ToUpper(name) {
			case "DEPTH":
				var depth string
				if !decoder.ExpectAtom(&depth) {
					return decoder.Err()
				}
				switch strings.ToLower(depth) {
				case "0", "1", "infinity":
					args.options.Depth = strings.ToLower(depth)
				default:
					return fmt.Errorf("invalid GETMETADATA DEPTH %q", depth)
				}
			case "MAXSIZE":
				if !decoder.ExpectNumber(&args.options.MaxSize) {
					return decoder.Err()
				}
				args.options.HasMaxSize = true
			default:
				return fmt.Errorf("unsupported GETMETADATA option %q", name)
			}
			return nil
		}); err != nil {
			return nil, 0, err
		}
		if !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
	}
	if !decoder.ExpectMailbox(&args.mailbox) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	readEntry := func() error {
		var entry string
		if !decoder.ExpectAstring(&entry) {
			return decoder.Err()
		}
		args.entries = append(args.entries, imap.MetadataEntryName(entry))
		return nil
	}
	if decoder.PeekSpecial('(') {
		if err := decoder.ExpectList(readEntry); err != nil {
			return nil, 0, err
		}
	} else if err := readEntry(); err != nil {
		return nil, 0, err
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(args.mailbox) + len(args.entries)*32), nil
}

type setMetadataArgs struct {
	mailbox string
	entries []imap.MetadataEntry
}

func parseSetMetadata(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &setMetadataArgs{}
	if !decoder.ExpectMailbox(&args.mailbox) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	size := int64(len(args.mailbox))
	if err := decoder.ExpectList(func() error {
		var name, value string
		var isNil bool
		if !decoder.ExpectAstring(&name) || !decoder.ExpectSP() {
			return decoder.Err()
		}
		if !decoder.ExpectNString(&value, &isNil) {
			return decoder.Err()
		}
		entry := imap.MetadataEntry{Name: imap.MetadataEntryName(name)}
		// NIL removes the entry; an empty string sets it to empty. Keeping the
		// pointer nil for NIL is what preserves that distinction.
		if !isNil {
			stored := value
			entry.Value = &stored
		}
		args.entries = append(args.entries, entry)
		size += int64(len(name) + len(value))
		return nil
	}); err != nil {
		return nil, 0, err
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, size, nil
}

// requireMetadataCapability accepts either METADATA token, since a backend may
// advertise only the server-scope form.
func requireMetadataCapability(c *conn) error {
	advertised := advertisedCapabilities(c)
	if advertised["METADATA"] || advertised["METADATA-SERVER"] {
		return nil
	}
	return fmt.Errorf("METADATA is not available")
}

func handleGetMetadata(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*getMetadataArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid GETMETADATA arguments")
	}
	if err := requireMetadataCapability(c); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(MetadataSession)
	if !ok {
		return c.writeBad(command.tag, "METADATA is not available")
	}
	options := args.options
	data, err := session.GetMetadata(ctx, args.mailbox, args.entries, &options)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if data != nil && len(data.Entries) != 0 {
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("METADATA").SP().Mailbox(args.mailbox).SP().
			List(len(data.Entries), func(i int) {
				entry := data.Entries[i]
				c.encoder.Astring(string(entry.Name)).SP()
				if entry.Value == nil {
					c.encoder.NIL()
				} else {
					c.encoder.String(*entry.Value)
				}
			}).CRLF()
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func handleSetMetadata(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*setMetadataArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid SETMETADATA arguments")
	}
	if err := requireMetadataCapability(c); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(MetadataSession)
	if !ok {
		return c.writeBad(command.tag, "METADATA is not available")
	}
	if err := session.SetMetadata(ctx, args.mailbox, args.entries, nil); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}
