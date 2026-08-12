package imapserver

import (
	"context"
	"fmt"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func handleSelect(ctx context.Context, c *conn, command *queuedCommand) error {
	mailbox, ok := command.args.(string)
	if !ok || mailbox == "" {
		return c.writeBad(command.tag, "invalid SELECT arguments")
	}
	readOnly := command.name == "EXAMINE"
	if err := abandonCurrentSelection(ctx, c); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	selected, snapshot, err := selectAtomic(ctx, c, mailbox, &SelectOptions{ReadOnly: readOnly})
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if err := writeSelectSnapshot(c, snapshot); err != nil {
		return err
	}
	if err := c.drainUpdates(updateAccounting{}); err != nil {
		return err
	}
	status := "READ-WRITE"
	if readOnly || snapshot.ReadOnly || snapshot.Status.ReadOnly {
		status = "READ-ONLY"
	}
	if selected == nil {
		return fmt.Errorf("imapserver: SELECT installed no selected state")
	}
	return writeTaggedCondition(c, command.tag, "OK", imap.ResponseCode(status), "", command.name+" completed")
}

func abandonCurrentSelection(ctx context.Context, c *conn) error {
	selected := c.state.unselect()
	if selected == nil {
		return nil
	}
	selected.close()
	return selected.mailbox.Unselect(ctx)
}

func selectAtomic(ctx context.Context, c *conn, mailbox string, options *SelectOptions) (selected *selectedState, snapshot *SelectSnapshot, err error) {
	updater, queue := newSelectionUpdater(c.server.options.Limits, c.updateOverflow)
	succeeded := false
	defer func() {
		if !succeeded {
			updater.core.close()
			queue.close()
		}
		if recovered := recover(); recovered != nil {
			panic(recovered)
		}
	}()
	result, err := c.state.session.Select(ctx, mailbox, updater, options)
	if err != nil {
		return nil, nil, err
	}
	selected, err = attachSelectedState(result, updater, queue, c.server.options.Limits)
	if err != nil {
		if result != nil && result.Mailbox != nil {
			closeRejectedMailbox(result.Mailbox, c.server.options.Limits.CommandTimeout)
		}
		return nil, nil, err
	}
	if !c.state.selectMailbox(selected) {
		selected.close()
		closeRejectedMailbox(selected.mailbox, c.server.options.Limits.CommandTimeout)
		return nil, nil, fmt.Errorf("imapserver: selected mailbox does not satisfy the enabled protocol revision")
	}
	selected.readOnly = selected.readOnly || options != nil && options.ReadOnly
	snapshotCopy := result.Snapshot
	snapshotCopy.UIDs = append([]imap.UID(nil), result.Snapshot.UIDs...)
	snapshotCopy.Flags = append([]imap.Flag(nil), result.Snapshot.Flags...)
	snapshotCopy.PermanentFlags = append([]imap.Flag(nil), result.Snapshot.PermanentFlags...)
	succeeded = true
	return selected, &snapshotCopy, nil
}

func writeSelectSnapshot(c *conn, snapshot *SelectSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("imapserver: nil SELECT snapshot")
	}
	flags := snapshot.Flags
	if flags == nil {
		flags = snapshot.Status.Flags
	}
	permanentFlags := snapshot.PermanentFlags
	if permanentFlags == nil {
		permanentFlags = snapshot.Status.PermanentFlags
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("FLAGS").SP().List(len(flags), func(i int) {
		c.encoder.Flag(string(flags[i]))
	}).CRLF()
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Number(snapshot.Status.NumMessages).SP().Atom("EXISTS").CRLF()
	if c.state.revision == revisionIMAP4rev1 {
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Number(snapshot.NumRecent).SP().Atom("RECENT").CRLF()
	}
	encodedFlags, err := encodeFlagList(permanentFlags)
	if err != nil {
		return err
	}
	writeUntaggedOK(c, imap.CodePermanentFlags, encodedFlags, "permanent flags")
	writeUntaggedOK(c, imap.CodeUIDValidity, fmt.Sprint(snapshot.Status.UIDValidity), "UID validity")
	writeUntaggedOK(c, imap.CodeUIDNext, fmt.Sprint(snapshot.Status.UIDNext), "predicted next UID")
	if snapshot.Status.Unseen != 0 && c.state.revision == revisionIMAP4rev1 {
		writeUntaggedOK(c, imap.CodeUnseen, fmt.Sprint(snapshot.Status.Unseen), "first unseen message")
	}
	if snapshot.NoModSeq || snapshot.Status.NoModSeq {
		writeUntaggedOK(c, imap.CodeNoModSeq, "", "mod-sequences unavailable")
	} else if snapshot.HighestModSeq != 0 || snapshot.Status.HighestModSeq != 0 {
		modSeq := snapshot.HighestModSeq
		if modSeq == 0 {
			modSeq = snapshot.Status.HighestModSeq
		}
		writeUntaggedOK(c, imap.CodeHighestModSeq, fmt.Sprint(modSeq), "highest modification sequence")
	}
	return c.encoder.Flush()
}

func writeUntaggedOK(c *conn, code imap.ResponseCode, args, textValue string) {
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").RespCond(imapwire.RespCond{
		Status: "OK",
		Text:   imapwire.RespText{Code: string(code), Args: args, Text: textValue},
	}).CRLF()
}
