package imapclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// CopyData is the UIDPLUS response-code data for a command that placed
// messages into a mailbox — COPYUID for COPY/MOVE, and APPENDUID when the
// destination UIDs are reported as a set (RFC 4315 section 3).
//
// The zero value means the server sent no such response code. RFC 4315 permits
// that: a server omits COPYUID and APPENDUID when the destination mailbox is
// not selectable by this user, and when the destination has UIDNOTSTICKY
// status. A caller that finds UIDValidity zero must fall back to locating the
// messages with SEARCH or FETCH, and must accept the race that implies —
// another client may have appended between the two commands, so a search for a
// Message-ID can legitimately match more than the message just written.
//
// Plain COPY fills this from the tagged OK carrying COPYUID. Native MOVE
// prefers the untagged COPYUID form advised by RFC 6851 section 4.3 and falls
// back to the tagged form. APPENDUID for APPEND is reported on [AppendData]
// (a single UID) rather than here.
//
// Construct with keyed fields only; fields may be added in a future release.
type CopyData struct {
	// UIDValidity is the UIDVALIDITY of the destination mailbox. Zero means
	// no response code was received.
	UIDValidity uint32

	// SourceUIDs are the UIDs in the source mailbox, in the order the
	// messages were copied or moved. It is empty for APPENDUID.
	SourceUIDs imap.UIDSet

	// DestinationUIDs are the UIDs assigned in the destination mailbox, in
	// the same order as SourceUIDs (or the order of append for APPENDUID).
	DestinationUIDs imap.UIDSet

	_ struct{}
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

// Expunge permanently removes messages marked with \Deleted from the selected
// mailbox. CLOSE also expunges, while UNSELECT does not.
func (c *Client) Expunge() *Command { return c.beginCommand("EXPUNGE", stateSelected, nil, nil) }
