package imapserver

import (
	"context"
	"slices"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func handleMove(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*copyArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid MOVE arguments")
	}
	if c.state.selected.readOnly {
		return writeTaggedCondition(c, command.tag, "NO", imap.CodeReadOnly, "", "mailbox is read-only")
	}
	mover, ok := c.state.selected.mailbox.(MoveMailbox)
	if !ok || !supportsAtomicMove(&c.state, c.server.backend) {
		return writeTaggedCondition(c, command.tag, "NO", imap.CodeCannot, "", "atomic MOVE is unavailable")
	}
	uids, ordered, err := resolveMessageSet(c.state.selected, args.set, commandUsesUIDs(command))
	if err != nil {
		return c.writeBad(command.tag, "invalid MOVE message set")
	}
	origin := nextCommandOrigin()
	data, err := mover.Move(ctx, uids, args.destination, &MoveOptions{MutationOptions: MutationOptions{Origin: origin}})
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if codeArgs, ok := copyUIDArgs(data); ok {
		writeUntaggedOK(c, imap.CodeCopyUID, codeArgs, "messages moved")
	}
	removed := ordered
	if data != nil && data.HasUIDs && !data.SourceUIDs.IsEmpty() && !data.SourceUIDs.Dynamic() {
		removed = removed[:0]
		for _, uid := range ordered {
			if data.SourceUIDs.Contains(uid) {
				removed = append(removed, uid)
			}
		}
	}
	if err := writeCommandExpunges(c, removed); err != nil {
		return err
	}
	if err := c.drainUpdates(updateAccounting{origin: origin, effect: effectMoveOut}); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func writeCommandExpunges(c *conn, ordered []imap.UID) error {
	shadow := slices.Clone(c.state.selected.uids)
	for _, uid := range ordered {
		at, ok := slices.BinarySearch(shadow, uid)
		if !ok {
			continue
		}
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Number(uint32(at + 1)).SP().Atom("EXPUNGE").CRLF()
		shadow = slices.Delete(shadow, at, at+1)
	}
	return c.encoder.Flush()
}
