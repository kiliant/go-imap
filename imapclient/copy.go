package imapclient

import (
	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// Copy copies messages addressed by sequence number into destination.
func (c *Client) Copy(set imap.SeqSet, destination string) *Command {
	return c.copy("COPY", set.String(), destination)
}

// CopyUID copies messages addressed by UID into destination.
func (c *Client) CopyUID(set imap.UIDSet, destination string) *Command {
	return c.copy("UID COPY", set.String(), destination)
}

func (c *Client) copy(name, set, destination string) *Command {
	if set == "" || destination == "" {
		return rejectedCommand(c, name, "COPY requires a non-empty set and destination")
	}
	return c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) { enc.SP().Atom(set).SP().Mailbox(destination) }, nil)
}

// Expunge permanently removes messages marked with \Deleted from the selected
// mailbox. CLOSE also expunges, while UNSELECT does not.
func (c *Client) Expunge() *Command { return c.beginCommand("EXPUNGE", stateSelected, nil, nil) }
