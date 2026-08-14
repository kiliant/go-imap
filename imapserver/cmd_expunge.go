package imapserver

import (
	"context"
	"fmt"
	"slices"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func handleExpunge(ctx context.Context, c *conn, command *queuedCommand) error {
	_, err := expungeSelected(ctx, c, command, false, nil)
	return err
}

type uidExpungeArgs struct{ set string }

// parseUIDExpunge reads UID EXPUNGE's message set. RFC 4315 section 2.1 gives
// the command exactly one argument, and it is always a UID set — there is no
// sequence-numbered form, which is why this does not go through the usual
// commandUsesUIDs branch.
func parseUIDExpunge(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &uidExpungeArgs{}
	if !expectMessageSet(decoder, &args.set) || !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(args.set)), nil
}

// handleUIDExpunge implements UID EXPUNGE (RFC 4315 section 2.1): remove the
// messages that carry \Deleted *and* fall inside the given UID set, leaving
// every other deleted message in place.
//
// The point of the command is concurrency. A client that marks messages deleted
// and then sends a plain EXPUNGE also destroys anything another session flagged
// in the meantime; UID EXPUNGE removes exactly what this client asked for.
// Silently treating it as EXPUNGE would therefore lose other sessions' mail,
// which is why it is gated on the capability rather than approximated.
func handleUIDExpunge(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*uidExpungeArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid UID EXPUNGE arguments")
	}
	if err := requireCapability(c, "UIDPLUS"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	if c.state.selected == nil {
		return c.writeBad(command.tag, "no mailbox is selected")
	}
	// Always resolved as UIDs: the command has no sequence-numbered form.
	uids, _, err := resolveMessageSet(c.state.selected, args.set, true)
	if err != nil {
		return c.writeBad(command.tag, "invalid UID EXPUNGE message set")
	}
	_, err = expungeSelected(ctx, c, command, false, &uids)
	return err
}

func handleClose(ctx context.Context, c *conn, command *queuedCommand) error {
	if c.state.selected != nil && c.state.selected.readOnly {
		return closeSelected(ctx, c, command)
	}
	completed, err := expungeSelected(ctx, c, command, true, nil)
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

// expungeSelected runs EXPUNGE over the whole mailbox, or over uids when
// UID EXPUNGE restricted it. A nil uids means "every deleted message", which is
// the contract the backend interface already carried before UID EXPUNGE
// existed.
func expungeSelected(ctx context.Context, c *conn, command *queuedCommand, silent bool, uids *imap.UIDSet) (bool, error) {
	selected := c.state.selected
	if selected == nil {
		return false, c.writeBad(command.tag, "no mailbox is selected")
	}
	if selected.readOnly {
		return false, writeTaggedCondition(c, command.tag, "NO", imap.CodeReadOnly, "", "mailbox is read-only")
	}
	origin := nextCommandOrigin()
	shadow := slices.Clone(selected.uids)
	// Collected for the CONTEXT registrations, which the command's own
	// responses bypass. See ext_e_context.go.
	var removed []imap.UID
	writer := newExpungeWriter(func(_ context.Context, uid imap.UID) error {
		at, ok := slices.BinarySearch(shadow, uid)
		if !ok {
			return fmt.Errorf("imapserver: backend EXPUNGE returned unknown UID %d", uid)
		}
		if !silent {
			// QRESYNC mandates VANISHED and UIDONLY forbids the
			// sequence-numbered form. See ext_b_qresync.go.
			if removalsUseVanished(c) {
				c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("VANISHED").SP().
					RawValue([]byte(imap.UIDSetNum(uid).String())).CRLF()
			} else {
				c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Number(uint32(at + 1)).SP().Atom("EXPUNGE").CRLF()
			}
			if err := c.encoder.Flush(); err != nil {
				return err
			}
		}
		shadow = slices.Delete(shadow, at, at+1)
		removed = append(removed, uid)
		return nil
	})
	err := selected.mailbox.Expunge(ctx, writer, uids, &ExpungeOptions{MutationOptions: MutationOptions{Origin: origin}})
	writer.core.close()
	if err != nil {
		return false, writeBackendError(c, command.tag, command.name, err)
	}
	if err := c.drainUpdates(updateAccounting{origin: origin, effect: effectExpunge}); err != nil {
		return false, err
	}
	if err := notifySearchContexts(c, removed); err != nil {
		return false, err
	}
	if silent {
		return true, nil
	}
	return true, c.writeTagged(command.tag, "OK", "EXPUNGE completed")
}
