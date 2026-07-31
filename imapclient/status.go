package imapclient

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// StatusOptions configures a STATUS request. Items is deliberately an
// open-ended []imap.StatusItem rather than a struct of booleans: IMAP
// extensions continuously add STATUS data items.
//
// Construct with keyed fields only; fields may be added in a future release.
type StatusOptions struct {
	Items []imap.StatusItem
	_     struct{}
}

// StatusData is mailbox data returned by STATUS. Values preserves every item
// received, including extension items not yet given a convenience field by this
// package. Numeric values are uint64; string-valued extensions are strings.
//
// Construct with keyed fields only; fields may be added in a future release.
type StatusData struct {
	Mailbox       string
	NumMessages   uint32
	UIDNext       imap.UID
	UIDValidity   uint32
	NumUnseen     uint32
	NumRecent     uint32
	HighestModSeq uint64
	Values        map[imap.StatusItemKeyword]any
	_             struct{}
}

// StatusCommand is an in-flight STATUS command.
type StatusCommand struct {
	*Command
	data *StatusData
}

// Wait waits for STATUS and returns its data.
func (cmd *StatusCommand) Wait(ctx context.Context) (*StatusData, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil status command")
	}
	if err := cmd.Command.Wait(ctx); err != nil {
		return nil, err
	}
	return cmd.data, nil
}

// Status requests items for mailbox. A nil options pointer is valid and
// requests the base mailbox counters.
func (c *Client) Status(mailbox string, options *StatusOptions) *StatusCommand {
	items, err := statusItems(options)
	if err != nil {
		return &StatusCommand{Command: failedCommand("STATUS", err)}
	}
	data := &StatusData{Mailbox: normalisedMailbox(mailbox), Values: make(map[imap.StatusItemKeyword]any)}
	cmd := c.beginCommand("STATUS", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox).SP().List(len(items), func(i int) {
			enc.Atom(string(items[i]))
		})
	}, statusCollector(data))
	return &StatusCommand{Command: cmd, data: data}
}

func statusItems(options *StatusOptions) ([]imap.StatusItemKeyword, error) {
	if options == nil || len(options.Items) == 0 {
		return []imap.StatusItemKeyword{
			imap.StatusItemMessages,
			imap.StatusItemUIDNext,
			imap.StatusItemUIDValidity,
			imap.StatusItemUnseen,
			imap.StatusItemRecent,
		}, nil
	}
	items := make([]imap.StatusItemKeyword, len(options.Items))
	for i, item := range options.Items {
		keyword, ok := item.(imap.StatusItemKeyword)
		if !ok {
			return nil, fmt.Errorf("imapclient: unsupported STATUS item %T", item)
		}
		if keyword == "" || strings.ContainsAny(string(keyword), " (){%*\\\"") {
			return nil, fmt.Errorf("imapclient: invalid STATUS item %q", keyword)
		}
		items[i] = keyword
	}
	return items, nil
}

func statusCollector(data *StatusData) commandCollector {
	return func(resp *untaggedResponse) (bool, error) {
		if resp.name != "STATUS" || resp.hasNum || resp.cond != nil {
			return false, nil
		}
		if !resp.dec.ExpectSP() {
			return true, resp.dec.Err()
		}
		var mailbox string
		if !resp.dec.ExpectMailbox(&mailbox) || !resp.dec.ExpectSP() {
			return true, resp.dec.Err()
		}
		values := make(map[imap.StatusItemKeyword]any)
		err := resp.dec.ExpectList(func() error {
			var item string
			if !resp.dec.ExpectAtom(&item) || !resp.dec.ExpectSP() {
				return resp.dec.Err()
			}
			var rawValue string
			if !resp.dec.ExpectAstring(&rawValue) {
				return resp.dec.Err()
			}
			if value, err := strconv.ParseUint(rawValue, 10, 64); err == nil {
				values[imap.StatusItemKeyword(strings.ToUpper(item))] = value
			} else {
				// STATUS extensions such as MAILBOXID return an astring rather
				// than a number. Keep it intact until that extension grows a
				// typed convenience field.
				values[imap.StatusItemKeyword(strings.ToUpper(item))] = rawValue
			}
			return nil
		})
		if err != nil {
			return true, err
		}
		if !resp.dec.ExpectCRLF() {
			return true, resp.dec.Err()
		}
		data.Mailbox = mailbox
		for item, value := range values {
			data.Values[item] = value
			number, isNumber := value.(uint64)
			switch item {
			case imap.StatusItemMessages:
				if !isNumber || number > uint64(^uint32(0)) {
					return true, fmt.Errorf("invalid numeric STATUS value for %s", item)
				}
				data.NumMessages = uint32(number)
			case imap.StatusItemUIDNext:
				if !isNumber || number > uint64(^uint32(0)) {
					return true, fmt.Errorf("invalid numeric STATUS value for %s", item)
				}
				data.UIDNext = imap.UID(number)
			case imap.StatusItemUIDValidity:
				if !isNumber || number > uint64(^uint32(0)) {
					return true, fmt.Errorf("invalid numeric STATUS value for %s", item)
				}
				data.UIDValidity = uint32(number)
			case imap.StatusItemUnseen:
				if !isNumber || number > uint64(^uint32(0)) {
					return true, fmt.Errorf("invalid numeric STATUS value for %s", item)
				}
				data.NumUnseen = uint32(number)
			case imap.StatusItemRecent:
				if !isNumber || number > uint64(^uint32(0)) {
					return true, fmt.Errorf("invalid numeric STATUS value for %s", item)
				}
				data.NumRecent = uint32(number)
			case imap.StatusItemHighestModSeq:
				if !isNumber {
					return true, fmt.Errorf("invalid numeric STATUS value for %s", item)
				}
				data.HighestModSeq = number
			}
		}
		return true, nil
	}
}

func failedCommand(name string, err error) *Command {
	cmd := &Command{name: name, done: make(chan struct{})}
	cmd.complete(err)
	return cmd
}
