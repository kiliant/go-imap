package imapclient

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ListSelectOption is a LIST-EXTENDED selection option. It is closed to this
// package so extensions can add structured options without changing
// ListOptions or introducing a second List method.
type ListSelectOption interface{ listSelectOption() }

// ListSelectOptionKeyword is a bare LIST selection option.
type ListSelectOptionKeyword string

func (ListSelectOptionKeyword) listSelectOption() {}

// LIST selection options defined by RFC 5258 and related extensions.
const (
	ListSelectSubscribed     ListSelectOptionKeyword = "SUBSCRIBED"
	ListSelectRemote         ListSelectOptionKeyword = "REMOTE"
	ListSelectRecursiveMatch ListSelectOptionKeyword = "RECURSIVEMATCH"
	ListSelectSpecialUse     ListSelectOptionKeyword = "SPECIAL-USE"
)

// ListReturnOption is a LIST-EXTENDED return option. It is intentionally an
// open family of package-defined types: LIST-STATUS needs a structured STATUS
// option, while CHILDREN is a bare atom.
type ListReturnOption interface{ listReturnOption() }

// ListReturnOptionKeyword is a bare LIST return option.
type ListReturnOptionKeyword string

func (ListReturnOptionKeyword) listReturnOption() {}

// LIST return options currently representable without arguments.
const (
	ListReturnSubscribed ListReturnOptionKeyword = "SUBSCRIBED"
	ListReturnChildren   ListReturnOptionKeyword = "CHILDREN"
)

// ListOptions configures LIST. Patterns supplements pattern with additional
// mailbox patterns; it is included now because LIST-EXTENDED supports multiple
// patterns. T08 can add structured selection and return option types without
// changing this method's signature.
//
// Construct with keyed fields only; fields may be added in a future release.
type ListOptions struct {
	Patterns         []string
	SelectionOptions []ListSelectOption
	ReturnOptions    []ListReturnOption
	_                struct{}
}

// ListData describes one mailbox returned by LIST or LSUB.
//
// A zero Delimiter means the server reported NIL: this mailbox has no hierarchy
// delimiter. Callers must use this server-provided value rather than assuming
// '/', '.', or '\\'.
//
// Construct with keyed fields only; fields may be added in a future release.
type ListData struct {
	Attrs     []imap.MailboxAttr
	Delimiter rune
	Mailbox   string
	_         struct{}
}

// ListCommand is an in-flight LIST or LSUB command.
type ListCommand struct {
	*Command
	data *[]*ListData
}

// Wait waits for LIST or LSUB and returns all matching mailboxes.
func (cmd *ListCommand) Wait(ctx context.Context) ([]*ListData, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil list command")
	}
	if err := cmd.Command.Wait(ctx); err != nil {
		return nil, err
	}
	return *cmd.data, nil
}

// List lists mailboxes below reference that match pattern. A nil options
// pointer selects the base LIST syntax. Selection and return options are sent
// using LIST-EXTENDED syntax; callers should use them only when the server
// advertises LIST-EXTENDED.
func (c *Client) List(reference, pattern string, options *ListOptions) *ListCommand {
	return c.list("LIST", reference, pattern, options)
}

// Lsub lists subscribed mailboxes. On a server advertising LIST-EXTENDED (or
// IMAP4rev2), it uses LIST (SUBSCRIBED), because LSUB is deprecated in rev2.
// Older servers receive the legacy LSUB command.
func (c *Client) Lsub(reference, pattern string, options *ListOptions) *ListCommand {
	if c.hasCapability("LIST-EXTENDED") || c.hasCapability("IMAP4REV2") {
		var copied ListOptions
		if options != nil {
			copied = *options
			copied.Patterns = append([]string(nil), options.Patterns...)
			copied.SelectionOptions = append([]ListSelectOption(nil), options.SelectionOptions...)
			copied.ReturnOptions = append([]ListReturnOption(nil), options.ReturnOptions...)
		}
		copied.SelectionOptions = append([]ListSelectOption{ListSelectSubscribed}, copied.SelectionOptions...)
		return c.list("LIST", reference, pattern, &copied)
	}
	if options != nil && (len(options.SelectionOptions) != 0 || len(options.ReturnOptions) != 0 || len(options.Patterns) != 0) {
		return &ListCommand{Command: failedCommand("LSUB", fmt.Errorf("imapclient: LSUB does not support LIST-EXTENDED options"))}
	}
	return c.list("LSUB", reference, pattern, nil)
}

func (c *Client) list(name, reference, pattern string, options *ListOptions) *ListCommand {
	patterns, selection, returns, err := listArguments(pattern, options)
	if err != nil {
		return &ListCommand{Command: failedCommand(name, err)}
	}
	data := make([]*ListData, 0)
	cmd := c.beginCommand(name, stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		if len(selection) != 0 {
			enc.SP().List(len(selection), func(i int) { enc.Atom(selection[i]) })
		}
		enc.SP().Mailbox(reference).SP()
		if len(patterns) == 1 {
			enc.ListMailbox(patterns[0])
		} else {
			enc.List(len(patterns), func(i int) { enc.ListMailbox(patterns[i]) })
		}
		if len(returns) != 0 {
			enc.SP().Atom("RETURN").SP().List(len(returns), func(i int) { enc.Atom(returns[i]) })
		}
	}, listCollector(&data))
	return &ListCommand{Command: cmd, data: &data}
}

func listArguments(pattern string, options *ListOptions) ([]string, []string, []string, error) {
	patterns := []string{pattern}
	if options == nil {
		return patterns, nil, nil, nil
	}
	patterns = append(patterns, options.Patterns...)
	selection := make([]string, len(options.SelectionOptions))
	for i, option := range options.SelectionOptions {
		keyword, ok := option.(ListSelectOptionKeyword)
		if !ok {
			return nil, nil, nil, fmt.Errorf("imapclient: unsupported LIST selection option %T", option)
		}
		selection[i] = string(keyword)
	}
	returns := make([]string, len(options.ReturnOptions))
	for i, option := range options.ReturnOptions {
		keyword, ok := option.(ListReturnOptionKeyword)
		if !ok {
			return nil, nil, nil, fmt.Errorf("imapclient: unsupported LIST return option %T", option)
		}
		returns[i] = string(keyword)
	}
	return patterns, selection, returns, nil
}

func listCollector(data *[]*ListData) commandCollector {
	return func(resp *untaggedResponse) (bool, error) {
		if resp.name != "LIST" && resp.name != "LSUB" {
			return false, nil
		}
		if resp.hasNum || resp.cond != nil || !resp.dec.ExpectSP() {
			if resp.dec.Err() != nil {
				return true, resp.dec.Err()
			}
			return false, nil
		}
		var rawAttrs []string
		if err := resp.dec.ExpectFlagList(&rawAttrs); err != nil {
			return true, err
		}
		if !resp.dec.ExpectSP() {
			return true, resp.dec.Err()
		}
		var rawDelimiter string
		var nilDelimiter bool
		if !resp.dec.ExpectNString(&rawDelimiter, &nilDelimiter) || !resp.dec.ExpectSP() {
			return true, resp.dec.Err()
		}
		var mailbox string
		if !resp.dec.ExpectMailbox(&mailbox) {
			return true, resp.dec.Err()
		}
		if !resp.dec.ExpectCRLF() {
			// LIST-EXTENDED can add return data after the mailbox. T08 owns the
			// typed parsing; discard it here without losing stream alignment.
			if err := resp.dec.DiscardLine(); err != nil {
				return true, err
			}
		}
		attrs := make([]imap.MailboxAttr, len(rawAttrs))
		for i, attr := range rawAttrs {
			attrs[i] = imap.MailboxAttr(attr)
		}
		var delimiter rune
		if !nilDelimiter {
			var size int
			delimiter, size = utf8.DecodeRuneInString(rawDelimiter)
			if delimiter == utf8.RuneError && size == 1 || size != len(rawDelimiter) {
				return true, fmt.Errorf("invalid LIST hierarchy delimiter %q", rawDelimiter)
			}
		}
		*data = append(*data, &ListData{Attrs: attrs, Delimiter: delimiter, Mailbox: mailbox})
		return true, nil
	}
}

func isListKeyword(s string) bool {
	return s != "" && !strings.ContainsAny(s, " (){%*\\\"")
}
