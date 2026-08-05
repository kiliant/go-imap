package imapclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// CopyData is the UIDPLUS response-code data for a command that placed
// messages into a mailbox. It is an alias for [imap.CopyData], which both
// protocol directions share.
type CopyData = imap.CopyData

// CopyCommand is an in-flight COPY or UID COPY command.
type CopyCommand struct {
	*Command
	data *CopyData
}

// Wait waits for COPY and returns its response data.
func (cmd *CopyCommand) Wait(ctx context.Context) (*CopyData, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil copy command")
	}
	if err := cmd.Command.Wait(ctx); err != nil {
		return nil, err
	}
	return cmd.data, nil
}

// CopyOptions configures COPY and UID COPY. A nil pointer selects the
// defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type CopyOptions struct {
	_ struct{}
}

// Copy copies messages addressed by sequence number into destination. A nil
// options pointer selects the defaults.
func (c *Client) Copy(set imap.SeqSet, destination string, options *CopyOptions) *CopyCommand {
	return c.copy("COPY", set.String(), destination)
}

// CopyUID copies messages addressed by UID into destination. A nil options
// pointer selects the defaults.
func (c *Client) CopyUID(set imap.UIDSet, destination string, options *CopyOptions) *CopyCommand {
	return c.copy("UID COPY", set.String(), destination)
}

func (c *Client) copy(name, set, destination string) *CopyCommand {
	data := &CopyData{}
	if set == "" || destination == "" {
		return &CopyCommand{Command: rejectedCommand(c, name, "COPY requires a non-empty set and destination"), data: data}
	}
	cmd := c.beginCommandWithCompletion(name, stateSelected, func(enc *imapwire.Encoder) {
		enc.SP()
		writeNumSet(enc, set)
		enc.SP().Mailbox(destination)
	}, nil, func(success bool, code, args string) {
		if !success || !strings.EqualFold(code, string(imap.CodeCopyUID)) {
			return
		}
		parsed, err := parseCopyUID(args)
		if err != nil {
			return
		}
		*data = *parsed
	})
	return &CopyCommand{Command: cmd, data: data}
}

// ExpungeOptions configures EXPUNGE. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type ExpungeOptions struct {
	_ struct{}
}

// Expunge permanently removes messages marked with \Deleted from the selected
// mailbox. CLOSE also expunges, while UNSELECT does not. A nil options pointer
// selects the defaults.
func (c *Client) Expunge(options *ExpungeOptions) *Command {
	return c.beginCommand("EXPUNGE", stateSelected, nil, nil)
}
