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
	} else {
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
	}
	if len(args.items) == 0 || !decoder.ExpectCRLF() {
		if decoder.Err() != nil {
			return nil, 0, decoder.Err()
		}
		return nil, 0, fmt.Errorf("FETCH requires at least one item")
	}
	return args, int64(len(args.set) + len(args.items)*24), nil
}

func handleFetch(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*fetchArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid FETCH arguments")
	}
	uidMode := commandUsesUIDs(command)
	uids, _, err := resolveMessageSet(c.state.selected, args.set, uidMode)
	if err != nil {
		return c.writeBad(command.tag, "invalid FETCH message set")
	}
	items, requestedUID := withFetchUID(args.items)
	includeUID := uidMode || requestedUID
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
		if err := imapcodec.WriteFetchResponse(c.encoder, mapped, fetchLiteralSize); err != nil {
			return err
		}
		return c.encoder.Flush()
	})
	err = c.state.selected.mailbox.Fetch(ctx, writer, uids, &FetchOptions{Items: items})
	writer.core.close()
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if err := c.writeTagged(command.tag, "OK", command.name+" completed"); err != nil {
		return err
	}
	return c.drainUpdates(updateAccounting{})
}
