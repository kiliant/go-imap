package imapserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type storeArgs struct {
	set    string
	op     StoreFlagsOp
	silent bool
	flags  []imap.Flag
	// unchangedSince is CONDSTORE's UNCHANGEDSINCE modifier, and
	// hasUnchangedSince its presence — zero is a legal value.
	// See ext_b_condstore.go.
	unchangedSince    uint64
	hasUnchangedSince bool
}

func parseStore(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &storeArgs{}
	var operation string
	if !expectMessageSet(decoder, &args.set) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	if err := parseStoreModifiers(decoder, args); err != nil {
		return nil, 0, err
	}
	if !decoder.ExpectAtom(&operation) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	operation = strings.ToUpper(operation)
	if strings.HasSuffix(operation, ".SILENT") {
		args.silent = true
		operation = strings.TrimSuffix(operation, ".SILENT")
	}
	args.op = StoreFlagsOp(operation)
	if args.op != StoreFlagsSet && args.op != StoreFlagsAdd && args.op != StoreFlagsRemove {
		return nil, 0, fmt.Errorf("invalid STORE operation %q", operation)
	}
	// RFC 3501 section 9 and RFC 9051 section 9 both define
	//
	//	store-att-flags = (["+" / "-"] "FLAGS" [".SILENT"]) SP
	//	                  (flag-list / (flag *(SP flag)))
	//
	// so the parentheses are optional: "STORE 1 +FLAGS \Deleted" is as valid as
	// "STORE 1 +FLAGS (\Deleted)". Only the parenthesised form was accepted
	// until T24, which made every client using the bare form — Dovecot's
	// imaptest among them, which is how this was found — unable to set a flag
	// at all. Nothing in this repository generated the bare form, so no test
	// here had ever sent one.
	var rawFlags []string
	if decoder.PeekSpecial('(') {
		if err := decoder.ExpectFlagList(&rawFlags); err != nil {
			return nil, 0, err
		}
	} else {
		for {
			var flag string
			if !decoder.ExpectFlag(&flag) {
				return nil, 0, decoder.Err()
			}
			rawFlags = append(rawFlags, flag)
			if !decoder.SP() {
				break
			}
		}
	}
	for _, flag := range rawFlags {
		args.flags = append(args.flags, imap.Flag(flag))
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(args.set) + len(operation) + len(args.flags)*16), nil
}

func handleStore(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*storeArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid STORE arguments")
	}
	if c.state.selected.readOnly {
		return writeTaggedCondition(c, command.tag, "NO", imap.CodeReadOnly, "", "mailbox is read-only")
	}
	if err := requireUIDCommand(c, command); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	uidMode := commandUsesUIDs(command)
	uids, _, err := resolveMessageSet(c.state.selected, args.set, uidMode)
	if err != nil {
		return c.writeBad(command.tag, "invalid STORE message set")
	}
	if err := validateCondStoreUse(c, args.hasUnchangedSince, "STORE UNCHANGEDSINCE"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	origin := nextCommandOrigin()
	var responseBytes int64
	type pendingStoreResponse struct {
		data    *imap.FetchMessageData
		cleanup func()
	}
	var responses []pendingStoreResponse
	defer func() {
		for _, response := range responses {
			response.cleanup()
		}
	}()
	writer := newFetchWriter(func(_ context.Context, data *imap.FetchMessageData) error {
		if args.silent {
			return nil
		}
		mapped, err := mapFetchResponse(c.state.selected, data, uidMode)
		if err != nil {
			return err
		}
		mapped = stripModSeqUnlessEnabled(c, mapped)
		_, cleanup, err := prepareFetchResponseLiterals(mapped, maxCommandFetchBytes)
		if err != nil {
			return err
		}
		wireBytes, err := fetchResponseWireSize(mapped)
		if err != nil {
			cleanup()
			return err
		}
		if wireBytes > maxCommandFetchBytes-responseBytes {
			cleanup()
			return commandLimitError("STORE response byte limit exceeded")
		}
		responseBytes += wireBytes
		responses = append(responses, pendingStoreResponse{data: mapped, cleanup: cleanup})
		return nil
	})
	options := &StoreOptions{
		MutationOptions:   MutationOptions{Origin: origin},
		Silent:            args.silent,
		UnchangedSince:    args.unchangedSince,
		HasUnchangedSince: args.hasUnchangedSince,
	}
	flags := &StoreFlags{Op: args.op, Flags: args.flags}
	// A conditional store takes the CONDSTORE path so the backend can report
	// which messages it refused; an unconditional one stays on the base method.
	var condStore *CondStoreResult
	if args.hasUnchangedSince {
		condStore, err = storeCondStore(ctx, c, writer, uids, flags, options)
	} else {
		err = c.state.selected.mailbox.Store(ctx, writer, uids, flags, options)
	}
	writer.core.close()
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	// A keyword must appear in the mailbox's untagged FLAGS response before it
	// first appears in a FETCH FLAGS response (RFC 3501 section 7.2.6). The
	// backend publishes the complete value in the command's update batch; peek
	// at that value here, while leaving the ordered batch queued for normal
	// revision accounting and delivery to every other selected connection.
	if mailboxFlags, ok := c.state.selected.queue.mailboxFlagsForOrigin(origin); ok &&
		!slices.Equal(c.state.selected.flags, mailboxFlags) {
		c.state.selected.flags = slices.Clone(mailboxFlags)
		if err := c.writeUpdate(deliveredUpdate{kind: updateMailboxFlags, flags: mailboxFlags}); err != nil {
			return err
		}
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	for _, response := range responses {
		if err := writeFetchLikeResponse(c, response.data); err != nil {
			return err
		}
	}
	if len(responses) != 0 {
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	// RFC 7162 section 3.1.3: rejected messages are reported on a successful
	// tagged OK through MODIFIED, not as a command failure.
	if modified, ok := condStoreModifiedArgs(condStore); ok {
		if err := writeTaggedCondition(c, command.tag, "OK", imap.CodeModified, modified, command.name+" completed"); err != nil {
			return err
		}
	} else if err := c.writeTagged(command.tag, "OK", command.name+" completed"); err != nil {
		return err
	}
	return c.drainUpdates(updateAccounting{origin: origin, effect: effectStore})
}
