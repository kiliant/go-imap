package imapserver

import (
	"context"

	"github.com/kiliant/go-imap"
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
	uids, _, err := resolveMessageSet(c.state.selected, args.set, commandUsesUIDs(command))
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
	// The backend's source-removal batch is the canonical ordering. Writing the
	// returned UIDs first can race an older queued removal and map them through a
	// stale sequence view, just like EXPUNGE.
	if err := c.drainUpdatesThrough(updateAccounting{origin: origin}); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}
