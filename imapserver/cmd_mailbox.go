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

// statusReply narrows a backend's STATUS data to the items the client asked
// for.
//
// RFC 3501 section 6.3.10 defines the untagged STATUS response as the answer to
// the request, not as everything the server happens to know. A backend that
// fills its whole StatusData for convenience — the obvious way to write one —
// would otherwise make the server volunteer items the client never named, and
// the client has no way to tell a volunteered item from one it forgot it asked
// for.
//
// The backend's value is not modified: it may be shared, cached, or reused.
//
// An item that is not a bare keyword is skipped, because it has nowhere to go:
// imap.StatusData.Values is keyed by StatusItemKeyword, so an argument-carrying
// StatusItem has no representation in the reply type at all. That is a frozen
// root-package constraint rather than a decision here — when package imap grows
// such an item, StatusData and this helper change together. Saying so is worth
// more than the comment this replaced, which claimed a pass-through the type
// system forbids.
func statusReply(data *imap.StatusData, items []imap.StatusItem) *imap.StatusData {
	if data == nil || len(data.Values) == 0 {
		return data
	}
	values := make(map[imap.StatusItemKeyword]any, len(items))
	for _, item := range items {
		keyword, ok := item.(imap.StatusItemKeyword)
		if !ok {
			continue
		}
		if value, ok := data.Values[keyword]; ok {
			values[keyword] = value
		}
	}
	narrowed := *data
	narrowed.Values = values
	return &narrowed
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
	if err := imapcodec.WriteStatusResponse(c.encoder, statusReply(data, args.items)); err != nil {
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
