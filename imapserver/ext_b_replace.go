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
		name: "REPLACE", states: stateMaskSelected, barrier: true, parse: parseReplace, handle: handleReplace,
	}
}

type replaceArgs struct {
	set     string
	mailbox string
	message appendMessage
}

func parseReplace(decoder *imapwire.Decoder) (any, int64, error) {
	args := &replaceArgs{}
	if !decoder.ExpectSP() || !expectMessageSet(decoder, &args.set) || !decoder.ExpectSP() ||
		!decoder.ExpectMailbox(&args.mailbox) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	if err := parseAppendMessagePrefix(decoder, &args.message); err != nil {
		return nil, 0, err
	}
	if err := parseAppendPayloadStart(decoder, &args.message); err != nil {
		return nil, 0, err
	}
	return args, int64(len(args.set) + len(args.mailbox) + len(args.message.flags)*16), nil
}

func handleReplace(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*replaceArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid REPLACE arguments")
	}
	failure := requireUIDCommand(c, command)
	if failure == nil {
		if err := requireCapability(c, "REPLACE"); err != nil {
			failure = &imap.Error{Type: imap.ErrorTypeBad, Text: err.Error()}
		}
	}
	if failure == nil && c.state.selected.readOnly {
		failure = &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeReadOnly, Text: "mailbox is read-only"}
	}
	_, ordered, err := resolveMessageSet(c.state.selected, args.set, commandUsesUIDs(command))
	if failure == nil && (err != nil || len(ordered) != 1) {
		failure = &imap.Error{Type: imap.ErrorTypeBad, Text: "REPLACE requires exactly one message"}
	}
	mailbox, ok := c.state.selected.mailbox.(ReplaceMailbox)
	if failure == nil && !ok {
		failure = fmt.Errorf("imapserver: backend advertises REPLACE but the selected mailbox does not implement ReplaceMailbox")
	}
	staged, err := c.stageAppend(ctx, args.message, true, failure)
	defer staged.close()
	if err != nil {
		return writeAppendDecodeError(c, command.tag, err, "invalid REPLACE payload")
	}
	if staged.failure != nil {
		return writeBackendError(c, command.tag, command.name, staged.failure)
	}
	if err := ctx.Err(); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	message := staged.messages[0]
	origin := nextCommandOrigin()
	data, backendErr := mailbox.Replace(ctx, ordered[0], args.mailbox, message.Literal, &ReplaceOptions{
		MutationOptions: MutationOptions{Origin: origin},
		Flags:           message.Flags,
		InternalDate:    message.InternalDate,
	})
	if backendErr != nil {
		return writeBackendError(c, command.tag, command.name, backendErr)
	}
	if err := c.drainUpdatesThrough(updateAccounting{origin: origin}); err != nil {
		return err
	}
	if data != nil && data.HasUID && data.UIDValidity != 0 && data.UID != 0 {
		return writeTaggedCondition(c, command.tag, "OK", imap.CodeAppendUID,
			fmt.Sprintf("%d %d", data.UIDValidity, data.UID), command.name+" completed")
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}
