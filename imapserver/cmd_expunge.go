package imapserver

import (
	"context"
	"fmt"
	"slices"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func handleExpunge(ctx context.Context, c *conn, command *queuedCommand) error {
	_, err := expungeSelected(ctx, c, command, false)
	return err
}

func handleClose(ctx context.Context, c *conn, command *queuedCommand) error {
	if c.state.selected != nil && c.state.selected.readOnly {
		return closeSelected(ctx, c, command)
	}
	completed, err := expungeSelected(ctx, c, command, true)
	if err != nil || !completed {
		return err
	}
	return closeSelected(ctx, c, command)
}

func closeSelected(ctx context.Context, c *conn, command *queuedCommand) error {
	selected := c.state.unselect()
	if selected == nil {
		return c.writeBad(command.tag, "no mailbox is selected")
	}
	selected.close()
	if err := selected.mailbox.Unselect(ctx); err != nil {
		return writeBackendError(c, command.tag, "CLOSE", err)
	}
	return c.writeTagged(command.tag, "OK", "CLOSE completed")
}

func expungeSelected(ctx context.Context, c *conn, command *queuedCommand, silent bool) (bool, error) {
	selected := c.state.selected
	if selected == nil {
		return false, c.writeBad(command.tag, "no mailbox is selected")
	}
	if selected.readOnly {
		return false, writeTaggedCondition(c, command.tag, "NO", imap.CodeReadOnly, "", "mailbox is read-only")
	}
	origin := nextCommandOrigin()
	shadow := slices.Clone(selected.uids)
	writer := newExpungeWriter(func(_ context.Context, uid imap.UID) error {
		at, ok := slices.BinarySearch(shadow, uid)
		if !ok {
			return fmt.Errorf("imapserver: backend EXPUNGE returned unknown UID %d", uid)
		}
		if !silent {
			c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Number(uint32(at + 1)).SP().Atom("EXPUNGE").CRLF()
			if err := c.encoder.Flush(); err != nil {
				return err
			}
		}
		shadow = slices.Delete(shadow, at, at+1)
		return nil
	})
	err := selected.mailbox.Expunge(ctx, writer, nil, &ExpungeOptions{MutationOptions: MutationOptions{Origin: origin}})
	writer.core.close()
	if err != nil {
		return false, writeBackendError(c, command.tag, command.name, err)
	}
	if err := c.drainUpdates(updateAccounting{origin: origin, effect: effectExpunge}); err != nil {
		return false, err
	}
	if silent {
		return true, nil
	}
	return true, c.writeTagged(command.tag, "OK", "EXPUNGE completed")
}
