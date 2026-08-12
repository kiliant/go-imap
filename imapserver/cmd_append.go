package imapserver

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type appendArgs struct {
	mailbox      string
	flags        []imap.Flag
	internalDate time.Time
	literal      *imapwire.LiteralReader
}

func parseAppend(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &appendArgs{}
	if !decoder.ExpectMailbox(&args.mailbox) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	if decoder.PeekSpecial('(') {
		var rawFlags []string
		if err := decoder.ExpectFlagList(&rawFlags); err != nil {
			return nil, 0, err
		}
		for _, flag := range rawFlags {
			args.flags = append(args.flags, imap.Flag(flag))
		}
		if !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
	}
	if decoder.PeekSpecial('"') {
		if !decoder.ExpectDateTime(&args.internalDate) || !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
	}
	literal, ok := decoder.Literal()
	if !ok {
		return nil, 0, decoder.Err()
	}
	if literal.Binary() {
		if err := literal.Discard(); err != nil {
			return nil, 0, err
		}
		return nil, 0, fmt.Errorf("literal8 APPEND requires BINARY")
	}
	args.literal = literal
	size := int64(len(args.mailbox) + len(args.flags)*16)
	return args, size, nil
}

func handleAppend(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*appendArgs)
	if args == nil || args.literal == nil {
		return c.writeBad(command.tag, "invalid APPEND arguments")
	}
	literal := &appendLiteral{reader: args.literal, remaining: args.literal.Size()}
	origin := nextCommandOrigin()
	data, backendErr := c.state.session.Append(ctx, args.mailbox, literal, &AppendOptions{
		MutationOptions: MutationOptions{Origin: origin},
		Flags:           args.flags,
		InternalDate:    args.internalDate,
	})
	if literal.remaining != 0 {
		if err := args.literal.Discard(); err != nil {
			return err
		}
		literal.remaining = 0
	}
	if backendErr != nil {
		return writeBackendError(c, command.tag, "APPEND", backendErr)
	}
	if err := c.drainUpdates(updateAccounting{origin: origin}); err != nil {
		return err
	}
	if data != nil && data.HasUID && data.UIDValidity != 0 && data.UID != 0 {
		return writeTaggedCondition(c, command.tag, "OK", imap.CodeAppendUID,
			fmt.Sprintf("%d %d", data.UIDValidity, data.UID), "APPEND completed")
	}
	return c.writeTagged(command.tag, "OK", "APPEND completed")
}

type appendLiteral struct {
	reader    io.Reader
	remaining int64
}

func (r *appendLiteral) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	if r.remaining == 0 && err == io.EOF {
		err = nil
	}
	return n, err
}
