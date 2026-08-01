package imapclient

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// SelectOptions configures SELECT and EXAMINE. The zero value is valid.
//
// CONDSTORE and QRESYNC select parameters are not fields here: they live on
// [SyncSelectOptions] and reach the wire through [Client.SelectSync] /
// [Client.ExamineSync]. Future plain select parameters that are not sync
// enabling commands belong on this type.
//
// Construct with keyed fields only; fields may be added in a future release.
type SelectOptions struct {
	_ struct{}
}

// MailboxStatus is the state reported while selecting a mailbox.
//
// UIDValidityChanged is a cache-invalidation event. When it is true, callers
// must discard every cached UID and message state for Mailbox before using the
// newly selected mailbox: UIDs are meaningful only within one UIDVALIDITY.
//
// Construct with keyed fields only; fields may be added in a future release.
type MailboxStatus struct {
	Mailbox            string
	Flags              []imap.Flag
	PermanentFlags     []imap.Flag
	NumMessages        uint32
	NumRecent          uint32
	UIDNext            imap.UID
	UIDValidity        uint32
	Unseen             uint32
	HighestModSeq      uint64
	ReadOnly           bool
	UIDValidityChanged bool
	_                  struct{}
}

// SelectData is kept as an alias for the data returned by SELECT and EXAMINE.
// New code may use the more descriptive MailboxStatus name.
type SelectData = MailboxStatus

// SelectCommand is an in-flight SELECT or EXAMINE command.
type SelectCommand struct {
	*Command
	data *MailboxStatus
}

// Wait waits for the SELECT or EXAMINE command to finish and returns the
// mailbox status collected from its untagged responses.
func (cmd *SelectCommand) Wait(ctx context.Context) (*MailboxStatus, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil select command")
	}
	if err := cmd.Command.Wait(ctx); err != nil {
		return nil, err
	}
	return cmd.data, nil
}

// Select selects mailbox for read-write access. A nil options selects the base
// command with default settings. A successful SELECT replaces any previously
// selected mailbox.
func (c *Client) Select(mailbox string, options *SelectOptions) *SelectCommand {
	return c.selectMailbox("SELECT", mailbox, options)
}

// Examine selects mailbox for read-only access. A nil options selects the base
// command with default settings. EXAMINE never permits flag changes or
// expunges through this session.
func (c *Client) Examine(mailbox string, options *SelectOptions) *SelectCommand {
	return c.selectMailbox("EXAMINE", mailbox, options)
}

func (c *Client) selectMailbox(name, mailbox string, _ *SelectOptions) *SelectCommand {
	data := &MailboxStatus{Mailbox: normalisedMailbox(mailbox), ReadOnly: name == "EXAMINE"}
	cmd := c.beginCommandWithCompletion(name, stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox)
	}, selectCollector(data), func(success bool, _, _ string) {
		if !success {
			// RFC 3501 section 6.3.1: a failed SELECT or EXAMINE leaves no
			// mailbox selected, so a session that had one drops back to the
			// authenticated state. Staying in the selected state would let the
			// next FETCH address the previous mailbox.
			c.mu.Lock()
			if c.state == StateSelected {
				c.state = StateAuthenticated
				c.selectedMailbox = ""
			}
			c.mu.Unlock()
			return
		}
		c.mu.Lock()
		if data.UIDValidity != 0 {
			if old := c.mailboxUIDValidity[data.Mailbox]; old != 0 && old != data.UIDValidity {
				data.UIDValidityChanged = true
			}
			c.mailboxUIDValidity[data.Mailbox] = data.UIDValidity
		}
		c.state = StateSelected
		c.selectedMailbox = data.Mailbox
		c.mu.Unlock()
	})
	return &SelectCommand{Command: cmd, data: data}
}

func selectCollector(data *MailboxStatus) commandCollector {
	return func(resp *untaggedResponse) (bool, error) {
		if resp.cond != nil && resp.name == "OK" {
			switch resp.cond.Text.Code {
			case "UIDNEXT":
				n, err := responseCodeUint32(resp.cond.Text.Args)
				if err != nil {
					return true, err
				}
				data.UIDNext = imap.UID(n)
			case "UIDVALIDITY":
				n, err := responseCodeUint32(resp.cond.Text.Args)
				if err != nil {
					return true, err
				}
				data.UIDValidity = n
			case "UNSEEN":
				n, err := responseCodeUint32(resp.cond.Text.Args)
				if err != nil {
					return true, err
				}
				data.Unseen = n
			case "HIGHESTMODSEQ":
				n, err := responseCodeUint64(resp.cond.Text.Args)
				if err != nil {
					return true, err
				}
				data.HighestModSeq = n
			case "PERMANENTFLAGS":
				flags, err := responseCodeFlags(resp.cond.Text.Args)
				if err != nil {
					return true, err
				}
				data.PermanentFlags = flags
			case "READ-ONLY":
				data.ReadOnly = true
			case "READ-WRITE":
				data.ReadOnly = false
			default:
				return false, nil
			}
			return true, nil
		}

		if !resp.hasNum {
			if resp.name != "FLAGS" {
				return false, nil
			}
			if !resp.dec.ExpectSP() {
				return true, resp.dec.Err()
			}
			var raw []string
			if err := resp.dec.ExpectFlagList(&raw); err != nil {
				return true, err
			}
			if !resp.dec.ExpectCRLF() {
				return true, resp.dec.Err()
			}
			data.Flags = flagsFromRaw(raw)
			return true, nil
		}

		switch resp.name {
		case "EXISTS":
			data.NumMessages = resp.number
		case "RECENT":
			data.NumRecent = resp.number
		default:
			return false, nil
		}
		if !resp.dec.ExpectCRLF() {
			return true, resp.dec.Err()
		}
		return true, nil
	}
}

func flagsFromRaw(raw []string) []imap.Flag {
	flags := make([]imap.Flag, len(raw))
	for i, flag := range raw {
		flags[i] = imap.Flag(flag)
	}
	return flags
}

func responseCodeUint32(args string) (uint32, error) {
	n, err := strconv.ParseUint(args, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid numeric response code %q", args)
	}
	return uint32(n), nil
}

func responseCodeUint64(args string) (uint64, error) {
	n, err := strconv.ParseUint(args, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid numeric response code %q", args)
	}
	return n, nil
}

func responseCodeFlags(args string) ([]imap.Flag, error) {
	dec := imapwire.NewDecoderString(args, nil)
	var raw []string
	if err := dec.ExpectFlagList(&raw); err != nil {
		return nil, err
	}
	if !dec.AtEOF() {
		if err := dec.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("invalid PERMANENTFLAGS response code %q", args)
	}
	return flagsFromRaw(raw), nil
}

func (c *Client) finishCloseMailbox(success bool, _, _ string) {
	if !success {
		return
	}
	c.mu.Lock()
	c.state = StateAuthenticated
	c.selectedMailbox = ""
	c.mu.Unlock()
}

// CloseMailboxOptions configures CLOSE. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type CloseMailboxOptions struct {
	_ struct{}
}

// CloseMailbox issues CLOSE. CLOSE expunges messages marked \Deleted before
// returning to the authenticated state. Use Unselect when abandoning a
// selected mailbox without expunging; confusing the two silently loses mail.
//
// A nil options pointer selects the defaults.
func (c *Client) CloseMailbox(options *CloseMailboxOptions) *Command {
	return c.beginCommandWithCompletion("CLOSE", stateSelected, nil, nil, c.finishCloseMailbox)
}

// UnselectOptions configures UNSELECT. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type UnselectOptions struct {
	_ struct{}
}

// Unselect issues UNSELECT (RFC 3691), returning to the authenticated state
// without expunging messages marked \Deleted.
//
// A nil options pointer selects the defaults.
func (c *Client) Unselect(options *UnselectOptions) *Command {
	return c.beginCommandWithCompletion("UNSELECT", stateSelected, nil, nil, c.finishCloseMailbox)
}

// CheckOptions configures CHECK. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type CheckOptions struct {
	_ struct{}
}

// Check requests a checkpoint of the selected mailbox. Servers may treat it
// as a no-op; it does not expunge messages.
//
// A nil options pointer selects the defaults.
func (c *Client) Check(options *CheckOptions) *Command {
	return c.beginCommand("CHECK", stateSelected, nil, nil)
}

// Create creates mailbox. A nil options pointer creates a plain mailbox; see
// [CreateOptions] for the RFC 4466 create parameters, including the RFC 6154
// USE parameter.
func (c *Client) Create(mailbox string, options *CreateOptions) *Command {
	return c.createMailbox(mailbox, options)
}

// DeleteOptions configures DELETE. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type DeleteOptions struct {
	_ struct{}
}

// Delete deletes mailbox. A nil options pointer selects the defaults.
func (c *Client) Delete(mailbox string, options *DeleteOptions) *Command {
	return c.mailboxCommand("DELETE", mailbox)
}

// RenameOptions configures RENAME. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type RenameOptions struct {
	_ struct{}
}

// Rename renames mailbox to newName. A nil options pointer selects the
// defaults.
func (c *Client) Rename(mailbox, newName string, options *RenameOptions) *Command {
	return c.beginCommand("RENAME", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox).SP().Mailbox(newName)
	}, nil)
}

// SubscribeOptions configures SUBSCRIBE. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type SubscribeOptions struct {
	_ struct{}
}

// Subscribe subscribes the session's user to mailbox. A nil options pointer
// selects the defaults.
func (c *Client) Subscribe(mailbox string, options *SubscribeOptions) *Command {
	return c.mailboxCommand("SUBSCRIBE", mailbox)
}

// UnsubscribeOptions configures UNSUBSCRIBE. A nil pointer selects the
// defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type UnsubscribeOptions struct {
	_ struct{}
}

// Unsubscribe removes the session's user's subscription to mailbox. A nil
// options pointer selects the defaults.
func (c *Client) Unsubscribe(mailbox string, options *UnsubscribeOptions) *Command {
	return c.mailboxCommand("UNSUBSCRIBE", mailbox)
}

func (c *Client) mailboxCommand(name, mailbox string) *Command {
	return c.beginCommand(name, stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox)
	}, nil)
}

func normalisedMailbox(name string) string {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}
