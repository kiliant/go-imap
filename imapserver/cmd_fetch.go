package imapserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type fetchArgs struct {
	set   string
	items []imap.FetchItem
	// changedSince is CONDSTORE's CHANGEDSINCE modifier, and vanished QRESYNC's
	// VANISHED modifier. See ext_b_condstore.go.
	changedSince uint64
	vanished     bool
	partial      *searchPartialRange
}

func parseFetch(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &fetchArgs{}
	if !expectMessageSet(decoder, &args.set) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	if decoder.PeekSpecial('(') {
		items, err := imapcodec.ReadFetchItems(decoder)
		if err != nil {
			return nil, 0, err
		}
		args.items = items
	} else if decoder.PeekAtomEqual("FAST") || decoder.PeekAtomEqual("ALL") || decoder.PeekAtomEqual("FULL") {
		var macro string
		if !decoder.ExpectAtom(&macro) {
			return nil, 0, decoder.Err()
		}
		switch strings.ToUpper(macro) {
		case "FAST":
			args.items = []imap.FetchItem{imap.FetchItemFlags, imap.FetchItemInternalDate, imap.FetchItemRFC822Size}
		case "ALL":
			args.items = []imap.FetchItem{imap.FetchItemFlags, imap.FetchItemInternalDate, imap.FetchItemRFC822Size, imap.FetchItemEnvelope}
		case "FULL":
			args.items = []imap.FetchItem{imap.FetchItemFlags, imap.FetchItemInternalDate, imap.FetchItemRFC822Size, imap.FetchItemEnvelope, &imap.FetchItemBodyStructure{}}
		default:
			return nil, 0, fmt.Errorf("unknown FETCH macro %q", macro)
		}
	} else {
		item, err := imapcodec.ReadFetchItem(decoder)
		if err != nil {
			return nil, 0, err
		}
		args.items = []imap.FetchItem{item}
	}
	if len(args.items) == 0 {
		if decoder.Err() != nil {
			return nil, 0, decoder.Err()
		}
		return nil, 0, fmt.Errorf("FETCH requires at least one item")
	}
	if err := parseFetchModifiers(decoder, args); err != nil {
		return nil, 0, err
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(args.set) + len(args.items)*24), nil
}

// fetchItemsSetSeen reports whether items include one that implicitly sets
// \Seen in a read-write selection: RFC822, RFC822.TEXT, and BODY[...] or
// BINARY[...] without .PEEK (RFC 3501 section 6.4.5, RFC 3516 section 4.1).
func fetchItemsSetSeen(items []imap.FetchItem) bool {
	for _, item := range items {
		switch item := item.(type) {
		case imap.FetchItemKeyword:
			if strings.EqualFold(string(item), string(imap.FetchItemRFC822)) || strings.EqualFold(string(item), string(imap.FetchItemRFC822Text)) {
				return true
			}
		case *imap.FetchItemBodySection:
			if item != nil && !item.Peek {
				return true
			}
		case *imap.FetchItemBinarySection:
			if item != nil && !item.Peek {
				return true
			}
		}
	}
	return false
}

func handleFetch(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*fetchArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid FETCH arguments")
	}
	if err := requireUIDCommand(c, command); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if args.vanished {
		if !commandUsesUIDs(command) {
			return c.writeBad(command.tag, "VANISHED requires UID FETCH")
		}
		if err := requireCapability(c, "QRESYNC"); err != nil {
			return c.writeBad(command.tag, err.Error())
		}
		if !c.state.enabledCapability("QRESYNC") {
			return c.writeBad(command.tag, "VANISHED requires ENABLE QRESYNC first")
		}
	}
	// The client's items, before the framework adds its own: withFetchUID and
	// applyCondStoreFetchItems below append UID and MODSEQ on the framework's
	// authority, and gating after them would refuse the framework's own work.
	if err := requireFetchItemCapabilities(c, args.items); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	uidMode := commandUsesUIDs(command)
	uids, _, err := resolveMessageSet(c.state.selected, args.set, uidMode)
	if err != nil {
		return c.writeBad(command.tag, "invalid FETCH message set")
	}
	if args.partial != nil {
		if !uidMode {
			return c.writeBad(command.tag, "PARTIAL requires UID FETCH")
		}
		if err := requireCapability(c, "PARTIAL"); err != nil {
			return c.writeBad(command.tag, err.Error())
		}
		// RFC 9394 section 3.4: window the UID set before CHANGEDSINCE.
		var numbers []uint32
		for _, uid := range c.state.selected.uids {
			if uids.Contains(uid) {
				numbers = append(numbers, uint32(uid))
			}
		}
		uids = nil
		for _, uid := range partialWindow(args.partial, numbers) {
			uids.AddNum(imap.UID(uid))
		}
	}
	usesModSeq := args.changedSince != 0
	for _, item := range args.items {
		if keyword, ok := item.(imap.FetchItemKeyword); ok && strings.EqualFold(string(keyword), string(imap.FetchItemModSeq)) {
			usesModSeq = true
		}
	}
	if err := validateSelectedCondStoreUse(ctx, c, usesModSeq, "FETCH MODSEQ/CHANGEDSINCE"); err != nil {
		return writeCondStoreUseError(c, command.tag, command.name, err)
	}
	if args.vanished {
		if err := writeFetchVanished(ctx, c, args); err != nil {
			return writeBackendError(c, command.tag, command.name, err)
		}
	}
	items, requestedUID := withFetchUID(args.items)
	items = applyCondStoreFetchItems(c, items)
	// RFC 7162 section 3.2.4 adds UID once CONDSTORE is enabled to responses
	// of a FETCH that asked for MODSEQ (CHANGEDSINCE implies it) or that
	// implicitly sets \Seen. Its exceptions keep a plain FETCH of other items
	// without the UID it did not ask for.
	includeUID := uidMode || requestedUID ||
		condStoreRequiresUID(c) && (usesModSeq || !c.state.selected.readOnly && fetchItemsSetSeen(args.items))
	var responseBytes int64
	writer := newFetchWriter(func(_ context.Context, data *imap.FetchMessageData) error {
		mapped, err := mapFetchResponse(c.state.selected, data, includeUID)
		if err != nil {
			return err
		}
		_, cleanup, err := prepareFetchResponseLiterals(mapped, maxCommandFetchBytes-responseBytes)
		if err != nil {
			return err
		}
		defer cleanup()
		wireBytes, err := fetchResponseWireSize(mapped)
		if err != nil {
			return err
		}
		if wireBytes > maxCommandFetchBytes-responseBytes {
			return commandLimitError("FETCH response byte limit exceeded")
		}
		responseBytes += wireBytes
		if err := writeFetchLikeResponse(c, mapped); err != nil {
			return err
		}
		return c.encoder.Flush()
	})
	origin := nextCommandOrigin()
	err = c.state.selected.mailbox.Fetch(ctx, writer, uids, &FetchOptions{
		MutationOptions: MutationOptions{Origin: origin},
		Items:           items,
		ChangedSince:    args.changedSince,
	})
	writer.core.close()
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if err := c.drainBeforeCompletion(command, updateAccounting{origin: origin}); err != nil {
		return err
	}
	if err := c.writeTagged(command.tag, "OK", command.name+" completed"); err != nil {
		return err
	}
	return c.drainUpdates(updateAccounting{origin: origin})
}
