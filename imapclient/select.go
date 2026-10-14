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

// MailboxStatus is the state reported while selecting a mailbox: the shared
// [imap.MailboxStatus], plus the one observation only a client can make.
//
// The embedded fields are promoted, so status.UIDValidity and the rest are read
// exactly as before. A keyed literal must name the embedded value:
//
//	&MailboxStatus{MailboxStatus: imap.MailboxStatus{Mailbox: name}}
//
// UIDValidityChanged is a cache-invalidation event. When it is true, callers
// must discard every cached UID and message state for Mailbox before using the
// newly selected mailbox: UIDs are meaningful only within one UIDVALIDITY.
//
// It is not part of [imap.MailboxStatus] because no server produces it. It is
// derived here, by comparing the reported UIDVALIDITY against the one this
// client last saw for the same mailbox, and it is therefore client state rather
// than protocol vocabulary.
//
// Construct with keyed fields only; fields may be added in a future release.
type MailboxStatus struct {
	imap.MailboxStatus

	// UIDValidityChanged reports that the mailbox's UIDVALIDITY differs from
	// the one this client last observed for it.
	UIDValidityChanged bool

	_ struct{}
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
	data := &MailboxStatus{MailboxStatus: imap.MailboxStatus{Mailbox: normalisedMailbox(mailbox), ReadOnly: name == "EXAMINE"}}
	cmd := c.beginCommandWithCompletion(name, stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox)
	}, selectCollector(c, data), func(success bool, _, _ string) {
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

// selectCollector claims the untagged responses that describe the mailbox
// being selected into data.
//
// That can include one LIST. RFC 9051 section 6.3.2 has an IMAP4rev2 server
// answer SELECT and EXAMINE with a LIST response for the selected mailbox,
// carrying OLDNAME when the name asked for was not the canonical one. That
// LIST belongs to the selection and must not reach
// [UnilateralDataHandler.List], where it would read as a NOTIFY MailboxName
// event or a rename. It is recognised by name: the listed name, or its
// OLDNAME, is the one this command selected. Every other LIST that arrives
// meanwhile is passed on as an event. The collector chain reads each response
// once, so this also passes on a pipelined LIST command's responses if a
// server ever interleaved them with this SELECT's.
//
// Only an IMAP4rev2 session is answered that way, so only there is a LIST
// claimed. On IMAP4rev1 a LIST during SELECT can only be an event, such as a
// NOTIFY MailboxName or SubscriptionChange for the mailbox being selected
// (RFC 5465 sections 5.4 and 5.5), and every one is passed on. Whether
// IMAP4rev2 is enabled is read when a LIST arrives, not when SELECT is issued.
// ENABLE records IMAP4rev2 in its completion, which the reader goroutine runs
// before it decodes any response to a later command; a snapshot at issue could
// be taken in the moment between ENABLE leaving the pending queue and that
// completion running, and read rev1 on a rev2 session.
//
// IMAP4rev2 leaves one case no client can resolve: an event for the selected
// mailbox that arrives before the SELECT's own LIST is identical to it on the
// wire. The first is claimed, and the SELECT's own LIST, arriving second, is
// passed on in its place.
func selectCollector(c *Client, data *MailboxStatus) commandCollector {
	claimed := false
	return func(resp *untaggedResponse) (bool, error) {
		if !resp.hasNum && resp.cond == nil && resp.name == "LIST" {
			var list []*ListData
			if _, err := listCollector("LIST", &list, 1)(resp); err != nil {
				return true, err
			}
			if !claimed && len(list) == 1 && c.rev2Enabled() && (normalisedMailbox(list[0].Mailbox) == data.Mailbox ||
				list[0].OldName != "" && normalisedMailbox(list[0].OldName) == data.Mailbox) {
				claimed = true
				return true, nil
			}
			c.deliverUnilateralList(list)
			return true, nil
		}
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
			case "NOMODSEQ":
				// RFC 7162 section 3.1.2.2: the mailbox does not support
				// persistent mod-sequences. Distinct from HighestModSeq simply
				// being absent from this response.
				data.NoModSeq = true
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
