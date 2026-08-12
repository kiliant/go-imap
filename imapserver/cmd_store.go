package imapserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type storeArgs struct {
	set    string
	op     StoreFlagsOp
	silent bool
	flags  []imap.Flag
}

func parseStore(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &storeArgs{}
	var operation string
	if !decoder.ExpectSequenceSet(&args.set) || !decoder.ExpectSP() || !decoder.ExpectAtom(&operation) || !decoder.ExpectSP() {
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
	var rawFlags []string
	if err := decoder.ExpectFlagList(&rawFlags); err != nil {
		return nil, 0, err
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
	uidMode := commandUsesUIDs(command)
	uids, _, err := resolveMessageSet(c.state.selected, args.set, uidMode)
	if err != nil {
		return c.writeBad(command.tag, "invalid STORE message set")
	}
	origin := nextCommandOrigin()
	var responseBytes int64
	writer := newFetchWriter(func(_ context.Context, data *imap.FetchMessageData) error {
		if args.silent {
			return nil
		}
		mapped, err := mapFetchResponse(c.state.selected, data, uidMode)
		if err != nil {
			return err
		}
		_, cleanup, err := prepareFetchResponseLiterals(mapped, maxCommandFetchBytes)
		if err != nil {
			return err
		}
		defer cleanup()
		wireBytes, err := fetchResponseWireSize(mapped)
		if err != nil {
			return err
		}
		if wireBytes > maxCommandFetchBytes-responseBytes {
			return commandLimitError("STORE response byte limit exceeded")
		}
		responseBytes += wireBytes
		if err := imapcodec.WriteFetchResponse(c.encoder, mapped, fetchLiteralSize); err != nil {
			return err
		}
		return c.encoder.Flush()
	})
	err = c.state.selected.mailbox.Store(ctx, writer, uids, &StoreFlags{Op: args.op, Flags: args.flags}, &StoreOptions{
		MutationOptions: MutationOptions{Origin: origin},
		Silent:          args.silent,
	})
	writer.core.close()
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if err := c.writeTagged(command.tag, "OK", command.name+" completed"); err != nil {
		return err
	}
	return c.drainUpdates(updateAccounting{origin: origin, effect: effectStore})
}
