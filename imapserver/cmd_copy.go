package imapserver

import (
	"context"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type copyArgs struct {
	set         string
	destination string
}

func parseCopy(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &copyArgs{}
	if !decoder.ExpectSequenceSet(&args.set) || !decoder.ExpectSP() || !decoder.ExpectMailbox(&args.destination) || !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(args.set) + len(args.destination)), nil
}

func handleCopy(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*copyArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid COPY arguments")
	}
	uids, _, err := resolveMessageSet(c.state.selected, args.set, commandUsesUIDs(command))
	if err != nil {
		return c.writeBad(command.tag, "invalid COPY message set")
	}
	origin := nextCommandOrigin()
	data, err := c.state.selected.mailbox.Copy(ctx, uids, args.destination, &CopyOptions{MutationOptions: MutationOptions{Origin: origin}})
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if err := c.drainUpdates(updateAccounting{origin: origin}); err != nil {
		return err
	}
	if codeArgs, ok := copyUIDArgs(data); ok {
		return writeTaggedCondition(c, command.tag, "OK", imap.CodeCopyUID, codeArgs, command.name+" completed")
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}
