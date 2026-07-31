package imapclient

import (
	"context"
	"fmt"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// CopyData is data returned by COPY. UIDValidity, SourceUIDs and
// DestinationUIDs are zero until UIDPLUS COPYUID response-code parsing is
// enabled.
//
// Construct with keyed fields only; fields may be added in a future release.
type CopyData struct {
	UIDValidity     uint32
	SourceUIDs      imap.UIDSet
	DestinationUIDs imap.UIDSet
	_               struct{}
}

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

// Copy copies messages addressed by sequence number into destination.
func (c *Client) Copy(set imap.SeqSet, destination string) *CopyCommand {
	return c.copy("COPY", set.String(), destination)
}

// CopyUID copies messages addressed by UID into destination.
func (c *Client) CopyUID(set imap.UIDSet, destination string) *CopyCommand {
	return c.copy("UID COPY", set.String(), destination)
}

func (c *Client) copy(name, set, destination string) *CopyCommand {
	data := &CopyData{}
	if set == "" || destination == "" {
		return &CopyCommand{Command: rejectedCommand(c, name, "COPY requires a non-empty set and destination"), data: data}
	}
	cmd := c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) { enc.SP().Atom(set).SP().Mailbox(destination) }, nil)
	return &CopyCommand{Command: cmd, data: data}
}

// Expunge permanently removes messages marked with \Deleted from the selected
// mailbox. CLOSE also expunges, while UNSELECT does not.
func (c *Client) Expunge() *Command { return c.beginCommand("EXPUNGE", stateSelected, nil, nil) }
