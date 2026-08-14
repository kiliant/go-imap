package imapserver

import (
	"context"
	"fmt"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type statusArgs struct {
	mailbox string
	items   []imap.StatusItem
}

type renameArgs struct {
	oldName string
	newName string
}

func parseStatus(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &statusArgs{}
	if !decoder.ExpectMailbox(&args.mailbox) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	items, err := imapcodec.ReadStatusItems(decoder)
	if err != nil {
		return nil, 0, err
	}
	if len(items) == 0 {
		return nil, 0, fmt.Errorf("STATUS requires at least one item")
	}
	for _, item := range items {
		args.items = append(args.items, item)
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(args.mailbox) + len(items)*16), nil
}

func parseRename(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &renameArgs{}
	if !decoder.ExpectMailbox(&args.oldName) || !decoder.ExpectSP() || !decoder.ExpectMailbox(&args.newName) || !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(args.oldName) + len(args.newName)), nil
}

func handleStatus(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*statusArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid STATUS arguments")
	}
	data, err := c.state.session.Status(ctx, args.mailbox, &StatusOptions{Items: args.items})
	if err != nil {
		return writeBackendError(c, command.tag, "STATUS", err)
	}
	if err := imapcodec.WriteStatusResponse(c.encoder, data); err != nil {
		return writeBackendError(c, command.tag, "STATUS", err)
	}
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", "STATUS completed")
}

func handleCreate(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*createArgs)
	if args == nil || args.mailbox == "" {
		return c.writeBad(command.tag, "invalid CREATE arguments")
	}
	// The USE parameter of CREATE-SPECIAL-USE. See ext_a_create.go.
	options, err := createOptions(c, args)
	if err != nil {
		return writeBackendError(c, command.tag, "CREATE", err)
	}
	if err := c.state.session.Create(ctx, args.mailbox, options); err != nil {
		return writeBackendError(c, command.tag, "CREATE", err)
	}
	return c.writeTagged(command.tag, "OK", "CREATE completed")
}

func handleDelete(ctx context.Context, c *conn, command *queuedCommand) error {
	return handleMailboxMutation(ctx, c, command, "DELETE", func(mailbox string) error {
		return c.state.session.Delete(ctx, mailbox, nil)
	})
}

func handleSubscribe(ctx context.Context, c *conn, command *queuedCommand) error {
	return handleMailboxMutation(ctx, c, command, "SUBSCRIBE", func(mailbox string) error {
		return c.state.session.Subscribe(ctx, mailbox, nil)
	})
}

func handleUnsubscribe(ctx context.Context, c *conn, command *queuedCommand) error {
	return handleMailboxMutation(ctx, c, command, "UNSUBSCRIBE", func(mailbox string) error {
		return c.state.session.Unsubscribe(ctx, mailbox, nil)
	})
}

func handleMailboxMutation(_ context.Context, c *conn, command *queuedCommand, operation string, call func(string) error) error {
	mailbox, ok := command.args.(string)
	if !ok || mailbox == "" {
		return c.writeBad(command.tag, "invalid "+operation+" arguments")
	}
	if err := call(mailbox); err != nil {
		return writeBackendError(c, command.tag, operation, err)
	}
	return c.writeTagged(command.tag, "OK", operation+" completed")
}

func handleRename(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*renameArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid RENAME arguments")
	}
	if err := c.state.session.Rename(ctx, args.oldName, args.newName, nil); err != nil {
		return writeBackendError(c, command.tag, "RENAME", err)
	}
	return c.writeTagged(command.tag, "OK", "RENAME completed")
}
