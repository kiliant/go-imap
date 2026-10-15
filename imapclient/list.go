package imapclient

import (
	"context"
	"errors"
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
// patterns. Structured selection and return option types can be added without
// changing this method's signature.
//
// ReturnOptions is shared by [Client.List] and [Client.ListMailboxes], but the
// accepted element set depends on the entry point: [Client.List] encodes
// keyword return options only ([ListReturnOptionKeyword]); structured options
// that carry arguments — today [*ListReturnStatus], and later LIST-METADATA /
// LIST-MYRIGHTS — must go through [Client.ListMailboxes]. Handing a structured
// option to [Client.List] fails the command without writing anything.
//
// Construct with keyed fields only; fields may be added in a future release.
type ListOptions struct {
	Patterns         []string
	SelectionOptions []ListSelectOption
	ReturnOptions    []ListReturnOption
	_                struct{}
}

// ListData describes one mailbox returned by LIST or LSUB. It is an alias for
// [imap.ListData], which both protocol directions share.
type ListData = imap.ListData

// ListExtendedItem is one LIST extended data item as received, kept in
// [ListData.Extended]. It is an alias for [imap.ListExtendedItem], which both
// protocol directions share.
type ListExtendedItem = imap.ListExtendedItem

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
//
// ReturnOptions may contain [ListReturnOptionKeyword] values only. Structured
// return options such as [*ListReturnStatus] are rejected here; use
// [Client.ListMailboxes] for those. See [ListOptions.ReturnOptions].
func (c *Client) List(reference, pattern string, options *ListOptions) *ListCommand {
	return c.list("LIST", reference, pattern, options)
}

// Lsub lists subscribed mailboxes. On a server advertising LIST-EXTENDED (or
// with IMAP4rev2 enabled), it uses LIST (SUBSCRIBED), because LSUB is removed
// from the rev2 command set.
// Older servers receive the legacy LSUB command.
func (c *Client) Lsub(reference, pattern string, options *ListOptions) *ListCommand {
	if c.hasCapability("LIST-EXTENDED") || c.rev2Enabled() {
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
	}, listCollector(name, &data, c.maxUntaggedResponses()))
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

func listCollector(name string, data *[]*ListData, limit int) commandCollector {
	count := 0
	responseName := "LIST"
	if name == "LSUB" {
		responseName = "LSUB"
	}
	return func(resp *untaggedResponse) (bool, error) {
		if resp.name != responseName || resp.hasNum || resp.cond != nil {
			return false, nil
		}
		if err := countUntaggedResponse(&count, limit, name); err != nil {
			return true, err
		}
		if err := resp.decodeData(); err != nil {
			return true, err
		}
		*data = append(*data, resp.data.list)
		return true, nil
	}
}

func readListResponse(dec *imapwire.Decoder) (*ListData, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	var rawAttrs []string
	if err := dec.ExpectFlagList(&rawAttrs); err != nil {
		return nil, err
	}
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	var rawDelimiter string
	var nilDelimiter bool
	if !dec.ExpectNString(&rawDelimiter, &nilDelimiter) || !dec.ExpectSP() {
		return nil, dec.Err()
	}
	var mailbox string
	if !dec.ExpectMailbox(&mailbox) {
		return nil, dec.Err()
	}
	extended, err := readListExtendedData(dec)
	if err != nil {
		return nil, err
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
			return nil, fmt.Errorf("invalid LIST hierarchy delimiter %q", rawDelimiter)
		}
	}
	return &ListData{Attrs: attrs, Delimiter: delimiter, Mailbox: mailbox,
		OldName: extended.OldName, ChildInfo: extended.ChildInfo, Extended: extended.Extended}, nil
}

// readListExtendedData reads the rest of a LIST response after the mailbox
// name: the extended data items of RFC 5258 section 6, if any. It returns them
// in a ListData that carries only Extended, which keeps every item raw in wire
// order, and the items decoded from it: OldName (RFC 5465 section 5.4) and
// ChildInfo (RFC 5258 section 3.5).
//
// It is as lenient as the client has always been here. Extended data it cannot
// parse is discarded to the end of the line, and the response keeps its
// attributes and name rather than failing the command. Each value is captured
// before it is decoded, so an OLDNAME or CHILDINFO value that is well-formed
// but not of its item's shape is kept raw and decoded to nothing, without
// costing the other items. Of repeated OLDNAME or CHILDINFO items the first
// that decodes is used.
func readListExtendedData(dec *imapwire.Decoder) (ListData, error) {
	var extended ListData
	ok := true
	if dec.PeekSpecial(' ') {
		ok = dec.SP() && dec.ExpectList(func() error {
			var tag string
			if !dec.ExpectAstring(&tag) || !dec.ExpectSP() {
				return dec.Err()
			}
			if tag == "" {
				// The tag is an astring, which may be an empty quoted string,
				// but its content must be a vendor or standard tag (RFC 5258
				// section 6, mbox-list-extended-item-tag), never empty.
				return errEmptyExtendedTag
			}
			raw, err := captureListExtendedValue(dec)
			if err != nil {
				return err
			}
			tag = strings.ToUpper(tag)
			extended.Extended = append(extended.Extended, imap.ListExtendedItem{Tag: tag, Value: raw})
			switch tag {
			case "OLDNAME":
				if extended.OldName == "" {
					extended.OldName = parseOldName(raw, valueDecoderOptions(dec))
				}
			case "CHILDINFO":
				if extended.ChildInfo == nil {
					extended.ChildInfo = parseChildInfo(raw, valueDecoderOptions(dec))
				}
			}
			return nil
		}) == nil
	}
	if ok && dec.ExpectCRLF() {
		return extended, nil
	}
	return ListData{}, dec.DiscardLine()
}

// valueDecoderOptions returns the options for decoding a value captured from
// dec: the connection's own limits and its current mailbox-name encoding, so
// the captured bytes decode exactly as they would have in place.
func valueDecoderOptions(dec *imapwire.Decoder) *imapwire.Options {
	opts := dec.Options()
	opts.UTF8Accept = dec.UTF8Accept()
	return &opts
}

// parseOldName reads an OLDNAME value, a parenthesised mailbox name, and
// returns the name or "" when the value is malformed.
func parseOldName(raw string, opts *imapwire.Options) string {
	dec := imapwire.NewDecoderString(raw, opts)
	var name string
	count := 0
	err := dec.ExpectList(func() error {
		if count++; count > 1 || !dec.ExpectMailbox(&name) {
			return errMalformedOldName
		}
		return nil
	})
	if err != nil || count != 1 || !dec.AtEOF() {
		return ""
	}
	return name
}

var errMalformedOldName = errors.New("malformed OLDNAME")

var errEmptyExtendedTag = errors.New("empty LIST extended data item tag")

// captureListExtendedValue reads one tagged-ext-val as its wire bytes. A value
// too large to keep is consumed and returned empty, as for FetchDataRaw.
func captureListExtendedValue(dec *imapwire.Decoder) (string, error) {
	var raw []byte
	if err := dec.CaptureValue(&raw); err != nil {
		if dec.Err() != nil {
			return "", err
		}
		return "", nil
	}
	return string(raw), nil
}

var errEmptyChildInfoOption = errors.New("CHILDINFO option without a name")

// parseChildInfo reads a CHILDINFO value, a parenthesised list of selection
// options (RFC 5258 section 6, childinfo-extended-item), and returns them with
// each option's name upper-cased, or nil when the value is malformed. The
// grammar quotes each option; a bare atom is accepted too.
func parseChildInfo(raw string, opts *imapwire.Options) []string {
	dec := imapwire.NewDecoderString(raw, opts)
	var options []string
	err := dec.ExpectList(func() error {
		var option string
		if !dec.ExpectAstring(&option) {
			return dec.Err()
		}
		if name, _, _ := strings.Cut(option, " "); name == "" {
			return errEmptyChildInfoOption
		}
		options = append(options, upperOptionName(option))
		return nil
	})
	if err != nil || !dec.AtEOF() || len(options) == 0 {
		return nil
	}
	return options
}

func isListKeyword(s string) bool {
	return s != "" && !strings.ContainsAny(s, " (){%*\\\"")
}

// upperOptionName upper-cases a selection option's name and leaves any value
// after it as sent: an option-extension is a tag, then optionally SP and a
// value whose case may matter (RFC 5258 section 6).
func upperOptionName(option string) string {
	if i := strings.IndexByte(option, ' '); i >= 0 {
		return strings.ToUpper(option[:i]) + option[i:]
	}
	return strings.ToUpper(option)
}
