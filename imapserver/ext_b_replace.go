package imapserver

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// REPLACE (RFC 8508).
//
// REPLACE is APPEND and EXPUNGE as one operation. The framework does not
// synthesise it from those two, for the same reason it never synthesises MOVE
// from COPY, STORE and EXPUNGE: a client using REPLACE is asking for atomicity,
// and a synthesised version delivers the opposite — a window in which the
// message exists twice, or not at all.

// ReplaceOptions configures REPLACE. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type ReplaceOptions struct {
	MutationOptions
	// Flags is the flag set for the replacement message.
	Flags []imap.Flag `imapfeature:"replace"`
	// InternalDate is the replacement's internal date. The zero value asks the
	// backend to use its current time.
	InternalDate time.Time `imapfeature:"replace"`
	_            struct{}
}

// ReplaceMailbox is the optional atomic REPLACE operation of RFC 8508.
//
// Replace removes the message identified by uid from the selected mailbox and
// stores literal into mailbox, which may be a different mailbox, as one atomic
// operation. Neither the removal nor the store may be observable without the
// other.
type ReplaceMailbox interface {
	Replace(ctx context.Context, uid imap.UID, mailbox string, literal io.Reader, options *ReplaceOptions) (*imap.AppendData, error)
}

const featureReplace featureID = "replace"

func init() {
	registerFeatures(featureDescriptor{
		ID: featureReplace,
		Active: func(_ *sessionState, advertised map[string]bool) bool {
			return advertised["REPLACE"]
		},
	})
	registerCapabilities(
		capabilityDescriptor{
			Name:            "REPLACE",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("REPLACE"),
		},
	)
	registerCommand("REPLACE", stateMaskSelected, true, parseReplace, handleReplace)
	uidCommandDescriptors["REPLACE"] = &commandDescriptor{
		name: "REPLACE", states: stateMaskSelected, parse: parseReplace, handle: handleReplace,
	}
}

type replaceArgs struct {
	set          string
	mailbox      string
	flags        []imap.Flag
	internalDate time.Time
	literal      *imapwire.LiteralReader
}

// parseReplace reads REPLACE's message identifier followed by APPEND's
// argument shape. RFC 8508 section 4.
func parseReplace(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &replaceArgs{}
	if !expectMessageSet(decoder, &args.set) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	if !decoder.ExpectMailbox(&args.mailbox) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	if decoder.PeekSpecial('(') {
		var rawFlags []string
		if err := decoder.ExpectFlagList(&rawFlags); err != nil {
			return nil, 0, err
		}
		for _, flag := range rawFlags {
			args.flags = append(args.flags, imap.Flag(flag))
		}
		if !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
	}
	if decoder.PeekSpecial('"') {
		if !decoder.ExpectDateTime(&args.internalDate) || !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
	}
	literal, ok := decoder.Literal()
	if !ok {
		return nil, 0, decoder.Err()
	}
	if literal.Binary() {
		if err := literal.Discard(); err != nil {
			return nil, 0, err
		}
		return nil, 0, fmt.Errorf("literal8 REPLACE requires BINARY")
	}
	args.literal = literal
	return args, int64(len(args.set) + len(args.mailbox) + len(args.flags)*16), nil
}

func handleReplace(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*replaceArgs)
	if args == nil || args.literal == nil {
		return c.writeBad(command.tag, "invalid REPLACE arguments")
	}
	literal := &appendLiteral{reader: args.literal, remaining: args.literal.Size()}
	// The literal is on the wire whatever happens next, so every failure path
	// below must still drain it or the connection desynchronises.
	drain := func() error {
		if literal.remaining == 0 {
			return nil
		}
		literal.remaining = 0
		return args.literal.Discard()
	}
	if c.state.selected.readOnly {
		if err := drain(); err != nil {
			return err
		}
		return writeTaggedCondition(c, command.tag, "NO", imap.CodeReadOnly, "", "mailbox is read-only")
	}
	_, ordered, err := resolveMessageSet(c.state.selected, args.set, commandUsesUIDs(command))
	if err != nil {
		if drainErr := drain(); drainErr != nil {
			return drainErr
		}
		return c.writeBad(command.tag, "invalid REPLACE message set")
	}
	// RFC 8508 section 4: REPLACE names exactly one message.
	if len(ordered) != 1 {
		if drainErr := drain(); drainErr != nil {
			return drainErr
		}
		return c.writeBad(command.tag, "REPLACE requires exactly one message")
	}
	mailbox, ok := c.state.selected.mailbox.(ReplaceMailbox)
	if !ok {
		if drainErr := drain(); drainErr != nil {
			return drainErr
		}
		return writeBackendError(c, command.tag, command.name,
			fmt.Errorf("imapserver: backend advertises REPLACE but the selected mailbox does not implement ReplaceMailbox"))
	}
	origin := nextCommandOrigin()
	data, backendErr := mailbox.Replace(ctx, ordered[0], args.mailbox, literal, &ReplaceOptions{
		MutationOptions: MutationOptions{Origin: origin},
		Flags:           args.flags,
		InternalDate:    args.internalDate,
	})
	if err := drain(); err != nil {
		return err
	}
	if backendErr != nil {
		return writeBackendError(c, command.tag, command.name, backendErr)
	}
	if err := c.drainUpdates(updateAccounting{origin: origin, effect: effectExpunge}); err != nil {
		return err
	}
	if data != nil && data.HasUID && data.UIDValidity != 0 && data.UID != 0 {
		return writeTaggedCondition(c, command.tag, "OK", imap.CodeAppendUID,
			fmt.Sprintf("%d %d", data.UIDValidity, data.UID), command.name+" completed")
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}
