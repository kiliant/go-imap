package imapserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func handleSelect(ctx context.Context, c *conn, command *queuedCommand) error {
	args, ok := command.args.(*selectArgs)
	if !ok || args == nil || args.mailbox == "" {
		return c.writeBad(command.tag, "invalid SELECT arguments")
	}
	if uidOnlyEnabled(c) && args.qresync != nil && !args.qresync.SeqMatchSeqNums.IsEmpty() {
		return writeBackendError(c, command.tag, command.name, uidRequiredError("QRESYNC sequence match data is not allowed once UIDONLY is enabled"))
	}
	mailbox := args.mailbox
	readOnly := command.name == "EXAMINE"
	// SELECT parameters from CONDSTORE and QRESYNC. See ext_b_condstore.go.
	options := &SelectOptions{ReadOnly: readOnly}
	if err := applySelectParams(c, args, options); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	hadSelection := c.state.selected != nil
	abandonCurrentSelection(ctx, c)
	if hadSelection {
		writeUntaggedOK(c, imap.CodeClosed, "", "previous mailbox closed")
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	selected, snapshot, err := selectAtomic(ctx, c, mailbox, options)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	// The selection is installed from here on. Every failure below fails the
	// command, and RFC 3501 section 6.3.1 and RFC 9051 section 6.3.2 leave no
	// mailbox selected after a failed SELECT, so each one goes through
	// failInstalledSelection. Answering NO while the selection stays installed
	// would accept selected-state commands the client believes are invalid.
	if advertisedCapabilities(c)["OBJECTID"] {
		id, err := mailboxIDResponse(ctx, c, mailbox)
		if err != nil {
			return failInstalledSelection(ctx, c, command, selected, err)
		}
		writeUntaggedOK(c, imap.CodeMailboxID, id, "mailbox identity")
	}
	if err := writeSelectSnapshot(c, snapshot); err != nil {
		return err
	}
	if err := writeSelectedMailboxList(ctx, c, mailbox); err != nil {
		return failInstalledSelection(ctx, c, command, selected, err)
	}
	// The QRESYNC resynchronisation report follows the ordinary selection
	// responses, so the client has UIDVALIDITY and HIGHESTMODSEQ before it is
	// told what vanished. See ext_b_qresync.go.
	if err := writeQResyncSelection(ctx, c, options.QResync, snapshot); err != nil {
		return failInstalledSelection(ctx, c, command, selected, err)
	}
	if err := c.drainUpdates(updateAccounting{}); err != nil {
		return err
	}
	status := "READ-WRITE"
	if readOnly || snapshot.Status.ReadOnly {
		status = "READ-ONLY"
	}
	if selected == nil {
		return fmt.Errorf("imapserver: SELECT installed no selected state")
	}
	return writeTaggedCondition(c, command.tag, "OK", imap.ResponseCode(status), "", command.name+" completed")
}

// writeSelectedMailboxList emits the untagged LIST response that RFC 9051
// section 6.3.2 adds to SELECT and EXAMINE. It is new in rev2 — RFC 3501 has no
// such response — so a rev1 session gets nothing, and the check is on the
// enabled revision rather than on a capability token, because rev2 incorporates
// the behaviour without one.
//
// The data comes from the backend's own List rather than from a new field on
// SelectSnapshot. That keeps the backend contract unchanged, and it guarantees
// the attributes and delimiter agree with what a LIST command would report for
// the same mailbox, which is the entire point of the response: the client is
// meant to be able to skip that round trip.
//
// The selected name is passed as the pattern, so a name containing a wildcard
// can match siblings; results are filtered back down to the one mailbox. If the
// backend lists nothing under its own name, no response is written rather than
// a fabricated one.
func writeSelectedMailboxList(ctx context.Context, c *conn, mailbox string) error {
	if c.state.revision != revisionIMAP4rev2 {
		return nil
	}
	options := &ListOptions{}
	args := &listArgs{}
	written := false
	writer := newListWriter(func(_ context.Context, data *imap.ListData) error {
		if written || data == nil || !mailboxNamesEqual(data.Mailbox, mailbox) {
			return nil
		}
		written = true
		return writeListData(c, "LIST", listResultAttrs(args, options, data.Attrs), data)
	})
	err := c.state.session.List(ctx, writer, "", []string{mailbox}, options)
	writer.core.close()
	return err
}

// mailboxNamesEqual compares two mailbox names as the protocol does: exactly,
// except that INBOX is case-insensitive per RFC 3501 section 5.1 and RFC 9051
// section 5.1. It exists so a SELECT of "inbox" still recognises the backend's
// canonical "INBOX" in its own listing.
func mailboxNamesEqual(a, b string) bool {
	if a == b {
		return true
	}
	return strings.EqualFold(a, "INBOX") && strings.EqualFold(b, "INBOX")
}

// failInstalledSelection answers a SELECT or EXAMINE that failed after its
// selection was installed. The selection is released before the tagged
// response, so the client and the server agree that nothing is selected.
//
// The tagged response maps the error that failed the command, exactly as it
// would before installation: a release failure must not replace a protocol
// refusal, or expose private text through it. The release runs on a fresh
// bounded context because the command's own context is often the reason the
// command failed — a cancelled Resync must still release its selection, and
// it must do so before the next command can run, not at connection teardown.
// A failed Unselect is reported to Options.OnError instead.
func failInstalledSelection(ctx context.Context, c *conn, command *queuedCommand, selected *selectedState, err error) error {
	releaseInstalledSelection(ctx, c, selected)
	return writeBackendError(c, command.tag, command.name, err)
}

// releaseInstalledSelection ends selected exactly once: the state transition
// is what makes a second call a no-op, so the updater, the queue and the
// backend handle are each released by whichever call wins it.
func releaseInstalledSelection(ctx context.Context, c *conn, selected *selectedState) {
	if selected == nil || c.state.selected != selected || c.state.unselect() != selected {
		return
	}
	selected.close()
	releaseCtx := context.WithoutCancel(ctx)
	if timeout := c.server.options.Limits.CommandTimeout; timeout > 0 {
		var cancel context.CancelFunc
		releaseCtx, cancel = context.WithTimeout(releaseCtx, timeout)
		defer cancel()
	}
	if err := selected.mailbox.Unselect(releaseCtx, nil); err != nil {
		c.reportCleanupError("UNSELECT", err)
	}
}

func abandonCurrentSelection(ctx context.Context, c *conn) {
	releaseInstalledSelection(ctx, c, c.state.selected)
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
	// MailboxStatus.Mailbox is descriptive, not a required lookup key.
	selected.name = mailbox
	if !c.state.selectMailbox(selected) {
		selected.close()
		closeRejectedMailbox(selected.mailbox, c.server.options.Limits.CommandTimeout)
		return nil, nil, fmt.Errorf("imapserver: selected mailbox does not satisfy the enabled protocol revision")
	}
	selected.readOnly = selected.readOnly || options != nil && options.ReadOnly
	snapshotCopy := result.Snapshot
	snapshotCopy.UIDs = slices.Clone(result.Snapshot.UIDs)
	snapshotCopy.Status.Flags = slices.Clone(result.Snapshot.Status.Flags)
	snapshotCopy.Status.PermanentFlags = slices.Clone(result.Snapshot.Status.PermanentFlags)
	succeeded = true
	return selected, &snapshotCopy, nil
}

func writeSelectSnapshot(c *conn, snapshot *SelectSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("imapserver: nil SELECT snapshot")
	}
	flags := wireFlags(c, snapshot.Status.Flags)
	permanentFlags := snapshot.Status.PermanentFlags
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("FLAGS").SP().List(len(flags), func(i int) {
		c.encoder.Flag(string(flags[i]))
	}).CRLF()
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Number(snapshot.Status.NumMessages).SP().Atom("EXISTS").CRLF()
	if c.state.revision == revisionIMAP4rev1 {
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Number(snapshot.Status.NumRecent).SP().Atom("RECENT").CRLF()
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
	if snapshot.Status.NoModSeq {
		writeUntaggedOK(c, imap.CodeNoModSeq, "", "mod-sequences unavailable")
	} else if snapshot.Status.HighestModSeq != 0 {
		writeUntaggedOK(c, imap.CodeHighestModSeq, fmt.Sprint(snapshot.Status.HighestModSeq), "highest modification sequence")
	}
	return c.encoder.Flush()
}

func writeUntaggedOK(c *conn, code imap.ResponseCode, args, textValue string) {
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").RespCond(imapwire.RespCond{
		Status: "OK",
		Text:   imapwire.RespText{Code: string(code), Args: args, Text: textValue},
	}).CRLF()
}
