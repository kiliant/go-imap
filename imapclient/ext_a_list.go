package imapclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ListReturnSpecialUse asks the server to report special-use attributes on the
// mailboxes it lists. SPECIAL-USE, RFC 6154 section 2. It is implied by the
// [ListSelectSpecialUse] selection option.
const ListReturnSpecialUse ListReturnOptionKeyword = "SPECIAL-USE"

// ListReturnStatus is the LIST-STATUS return option: it asks the server to send
// the STATUS of every selectable mailbox it lists, in the same round trip.
// LIST-STATUS, RFC 5819; also part of IMAP4rev2, RFC 9051 section 6.3.9.7.
//
// It is a structured [ListReturnOption] rather than a keyword because RFC 5819
// section 4 gives the option an argument list:
//
//	return-option =/ status-option
//	status-option = "STATUS" SP "(" status-att *(SP status-att) ")"
//
// It reaches the wire through [Client.ListMailboxes], which is the entry point
// that can also emulate it. [Client.List] is the plain command handle and
// accepts keyword return options only; handed this type it fails the command
// without writing anything.
//
// Construct with keyed fields only; fields may be added in a future release.
type ListReturnStatus struct {
	// Items are the STATUS data items requested for each listed mailbox. It is
	// the open [imap.StatusItem] set for the reason [StatusOptions.Items] is:
	// STATUS items are added by nearly every extension. An empty slice requests
	// the same base counters as a nil [StatusOptions].
	Items []imap.StatusItem

	// Handler receives one [StatusData] per untagged STATUS response, in
	// arrival order. The mailbox each one describes is [StatusData.Mailbox]:
	// RFC 5819 correlates the two responses by name, not by position, and
	// deliberately sends no STATUS at all for a mailbox that cannot be
	// selected. A nil Handler discards the data.
	//
	// It is called on the reader goroutine and must not block. Appending to a
	// slice the caller owns is nevertheless safe: [Client.ListMailboxes]
	// returns only after the command has completed, which orders every
	// Handler call before the return.
	Handler func(*StatusData)

	_ struct{}
}

func (*ListReturnStatus) listReturnOption() {}

// HasChildren reports whether attrs say the mailbox has child mailboxes.
// CHILDREN, RFC 3348 section 3.
//
// known is false when the server reported neither \HasChildren nor
// \HasNoChildren, which RFC 3348 section 3 explicitly permits: a server that
// cannot compute the answer cheaply is allowed to omit both, and a client "can
// not make any assumptions about whether a mailbox has children based upon the
// absence of a single attribute". Treating the absence as "no children" hides
// an entire subtree.
//
// Even when known is true the answer is a hint. RFC 3348 section 3 allows
// \HasChildren on a mailbox whose children a later LIST does not show, because
// the user may lack access to them.
func HasChildren(attrs []imap.MailboxAttr) (hasChildren, known bool) {
	if imap.ContainsAttr(attrs, imap.MailboxAttrHasChildren) {
		return true, true
	}
	if imap.ContainsAttr(attrs, imap.MailboxAttrHasNoChildren) {
		return false, true
	}
	// \Noinferiors means the mailbox can never have children, and RFC 3348
	// section 3 says \HasNoChildren is then redundant and should be omitted.
	if imap.ContainsAttr(attrs, imap.MailboxAttrNoInferiors) {
		return false, true
	}
	return false, false
}

// CreateOptions configures CREATE. A nil pointer creates a plain mailbox.
//
// Construct with keyed fields only; fields may be added in a future release.
type CreateOptions struct {
	// SpecialUse requests the RFC 6154 USE create parameter, designating the
	// new mailbox as the user's \Drafts, \Sent, \Junk, \Trash, \Archive,
	// \All or \Flagged mailbox. It requires the CREATE-SPECIAL-USE
	// capability, which RFC 6154 section 3 makes a separate advertisement
	// from SPECIAL-USE: a server may report special-use attributes without
	// letting a client set them.
	SpecialUse []imap.MailboxAttr

	_ struct{}
}

// CreateMailbox creates mailbox with the RFC 4466 create parameters in options.
//
// It is a sibling of [Client.Create] rather than a change to it, because
// [Client.Create] takes no options struct and adding a parameter would break
// every caller. A nil options pointer makes the two identical.
//
// A server that does not support a requested special use answers NO with the
// [imap.CodeUseAttr] response code; RFC 6154 section 3 leaves whether the
// mailbox was still created up to the server, so a caller that cares must LIST
// afterwards.
func (c *Client) CreateMailbox(mailbox string, options *CreateOptions) *Command {
	if options == nil || len(options.SpecialUse) == 0 {
		return c.mailboxCommand("CREATE", mailbox)
	}
	if !c.supportsAny("CREATE-SPECIAL-USE") {
		return unsupportedCommand("CREATE with the USE parameter", "CREATE-SPECIAL-USE")
	}
	uses := make([]string, len(options.SpecialUse))
	for i, use := range options.SpecialUse {
		name := string(use)
		if !strings.HasPrefix(name, "\\") || !isListKeyword(strings.TrimPrefix(name, "\\")) {
			return rejectedCommand(c, "CREATE", fmt.Sprintf("invalid special-use attribute %q", name))
		}
		uses[i] = name
	}
	return c.beginCommand("CREATE", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		// create = "CREATE" SP mailbox [create-params], RFC 4466 section 2.3;
		// create-param =/ "USE" SP "(" [use-attr *(SP use-attr)] ")",
		// RFC 6154 section 6.
		enc.SP().Mailbox(mailbox).SP().Special('(')
		// use-attr is a backslash-prefixed atom, and "\" is not an ATOM-CHAR,
		// so it has to go through the flag writer rather than Atom.
		enc.Atom("USE").SP().List(len(uses), func(i int) { enc.Flag(uses[i]) })
		enc.Special(')')
	}, nil)
}

// ListMailboxes lists mailboxes, using LIST-EXTENDED where the server has it
// and emulating the parts of it that plain LIST can express where it does not.
// LIST-EXTENDED, RFC 5258.
//
// It is a blocking sibling of [Client.List], not a replacement: the extended
// options themselves live in [ListOptions] and reach the wire through
// [Client.List] unchanged. What this method adds is the fallback, which needs
// more than one round trip and therefore cannot be a command handle.
//
// # Emulated fallback
//
// Without LIST-EXTENDED, a server accepts neither several patterns in one
// command nor any selection or return option. The emulation then issues one
// plain LIST per pattern and concatenates the results, dropping duplicates by
// mailbox name. Two differences from a real LIST-EXTENDED survive:
//
//   - the patterns are matched in separate commands, so a mailbox matching two
//     patterns is reported once here but its attributes come from whichever
//     command reached it first;
//   - the mailbox set is sampled at several different instants rather than one,
//     so a mailbox created or deleted between the commands may appear in the
//     result of one pattern and not another.
//
// [ListSelectSubscribed] alone is emulated with LSUB, whose result set RFC 3501
// section 6.3.9 defines as the subscription list. The \Subscribed attribute of
// RFC 5258 is added to each result, because LSUB does not report it and a caller
// that switched on it would otherwise see the fallback as "nothing is
// subscribed". Any other selection or return option has no plain-LIST
// equivalent and yields [ErrCapabilityNotAdvertised] without writing anything.
//
// A [ListReturnStatus] in the return options additionally requests LIST-STATUS.
// That option carries a second, independent emulation, described below under
// "Emulated LIST-STATUS fallback".
//
// # Emulated LIST-STATUS fallback
//
// Without LIST-STATUS (RFC 5819) and without IMAP4rev2, the listing is
// performed as above and then one STATUS command is issued per selectable
// mailbox — the N+1 round trips RFC 5819 section 2 exists to remove. Mailboxes
// carrying \Noselect or \NonExistent are skipped, matching the servers'
// obligation in RFC 5819 section 2 to send no STATUS for a mailbox that cannot
// be selected. The emulation is not atomic: the mailbox set and each mailbox's
// counters are sampled at different instants, so a mailbox may be deleted
// between the LIST and its STATUS, and two mailboxes' counters never describe
// the same moment. A mailbox whose STATUS is refused with NO is skipped rather
// than failing the whole listing, because RFC 5819 section 2 permits a server to
// drop a STATUS reply and still answer the LIST with a tagged OK.
func (c *Client) ListMailboxes(ctx context.Context, reference, pattern string, options *ListOptions) ([]*ListData, error) {
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "LIST requires a non-nil context"}
	}
	status, rest, err := splitListStatusOption(options)
	if err != nil {
		return nil, err
	}
	if status != nil {
		return c.listStatus(ctx, reference, pattern, rest, status)
	}
	if c.supportsAny("LIST-EXTENDED") || c.rev2Enabled() {
		return c.List(reference, pattern, options).Wait(ctx)
	}

	patterns := []string{pattern}
	subscribed := false
	if options != nil {
		patterns = append(patterns, options.Patterns...)
		if len(options.ReturnOptions) != 0 {
			return nil, capabilityError("LIST return options", "LIST-EXTENDED")
		}
		for _, option := range options.SelectionOptions {
			keyword, ok := option.(ListSelectOptionKeyword)
			if !ok || !strings.EqualFold(string(keyword), string(ListSelectSubscribed)) {
				return nil, capabilityError("LIST selection options", "LIST-EXTENDED")
			}
			subscribed = true
		}
	}

	var merged []*ListData
	seen := make(map[string]bool)
	for _, p := range patterns {
		var data []*ListData
		var err error
		if subscribed {
			data, err = c.list("LSUB", reference, p, nil).Wait(ctx)
		} else {
			data, err = c.list("LIST", reference, p, nil).Wait(ctx)
		}
		if err != nil {
			return nil, err
		}
		for _, item := range data {
			if seen[item.Mailbox] {
				continue
			}
			seen[item.Mailbox] = true
			if subscribed && !imap.ContainsAttr(item.Attrs, imap.MailboxAttrSubscribed) {
				item.Attrs = append(item.Attrs, imap.MailboxAttrSubscribed)
			}
			merged = append(merged, item)
		}
	}
	return merged, nil
}

// splitListStatusOption separates a [ListReturnStatus] from the other return
// options, so the remaining options can travel the ordinary LIST path. options
// is never modified.
func splitListStatusOption(options *ListOptions) (*ListReturnStatus, *ListOptions, error) {
	if options == nil {
		return nil, nil, nil
	}
	var status *ListReturnStatus
	kept := make([]ListReturnOption, 0, len(options.ReturnOptions))
	for _, option := range options.ReturnOptions {
		found, ok := option.(*ListReturnStatus)
		if !ok {
			kept = append(kept, option)
			continue
		}
		if status != nil {
			// RFC 5819 section 4 registers one STATUS return option per LIST;
			// two would leave the requested item set ambiguous.
			return nil, nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "LIST accepts at most one STATUS return option"}
		}
		status = found
	}
	if status == nil {
		return nil, options, nil
	}
	rest := *options
	rest.Patterns = append([]string(nil), options.Patterns...)
	rest.SelectionOptions = append([]ListSelectOption(nil), options.SelectionOptions...)
	rest.ReturnOptions = kept
	return status, &rest, nil
}

// listStatus performs a LIST with the RFC 5819 STATUS return option, or emulates
// it. See [Client.ListMailboxes] for the emulation's cost.
func (c *Client) listStatus(ctx context.Context, reference, pattern string, options *ListOptions, status *ListReturnStatus) ([]*ListData, error) {
	items, err := statusItems(&StatusOptions{Items: status.Items})
	if err != nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: err.Error()}
	}
	// LIST-STATUS is defined as a LIST-EXTENDED return option, so a server
	// advertising it advertises the RETURN syntax as well. IMAP4rev2 folds both
	// in, but only once ENABLEd: an un-ENABLEd rev2 server is still a rev1
	// server on the wire.
	if c.supportsAny("LIST-STATUS") || c.rev2Enabled() {
		return c.listStatusCommand(reference, pattern, options, items, status.Handler).Wait(ctx)
	}

	data, err := c.ListMailboxes(ctx, reference, pattern, options)
	if err != nil {
		return nil, err
	}
	if status.Handler == nil {
		return data, nil
	}
	for _, item := range data {
		if imap.ContainsAttr(item.Attrs, imap.MailboxAttrNoSelect) || imap.ContainsAttr(item.Attrs, imap.MailboxAttrNonExistent) {
			continue
		}
		mailboxStatus, err := c.Status(item.Mailbox, &StatusOptions{Items: status.Items}).Wait(ctx)
		if err != nil {
			var protocolErr *imap.Error
			if errors.As(err, &protocolErr) && protocolErr.Type == imap.ErrorTypeNo {
				continue
			}
			return nil, err
		}
		status.Handler(mailboxStatus)
	}
	return data, nil
}

// listStatusCommand issues LIST ... RETURN (... STATUS (...)). It builds the
// command here rather than through [Client.List] because the RETURN list then
// holds an item with an argument list of its own, which the keyword-only
// encoding cannot express.
func (c *Client) listStatusCommand(reference, pattern string, options *ListOptions, items []imap.StatusItemKeyword, handler func(*StatusData)) *ListCommand {
	patterns := []string{pattern}
	var selection, returns []string
	if options != nil {
		patterns = append(patterns, options.Patterns...)
		for _, option := range options.SelectionOptions {
			keyword, ok := option.(ListSelectOptionKeyword)
			if !ok {
				return &ListCommand{Command: failedCommand("LIST", fmt.Errorf("imapclient: unsupported LIST selection option %T", option))}
			}
			selection = append(selection, string(keyword))
		}
		for _, option := range options.ReturnOptions {
			keyword, ok := option.(ListReturnOptionKeyword)
			if !ok {
				return &ListCommand{Command: failedCommand("LIST", fmt.Errorf("imapclient: unsupported LIST return option %T", option))}
			}
			returns = append(returns, string(keyword))
		}
	}

	data := make([]*ListData, 0)
	limit := c.maxUntaggedResponses()
	lists := listCollector("LIST", &data, limit)
	statuses := listStatusCollector(handler, limit)
	cmd := c.beginCommand("LIST", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		if len(selection) != 0 {
			enc.SP().List(len(selection), func(i int) { enc.Atom(selection[i]) })
		}
		enc.SP().Mailbox(reference).SP()
		if len(patterns) == 1 {
			enc.ListMailbox(patterns[0])
		} else {
			enc.List(len(patterns), func(i int) { enc.ListMailbox(patterns[i]) })
		}
		// status-option is one return-option carrying a nested list, so the
		// RETURN list is written element by element rather than with List.
		enc.SP().Atom("RETURN").SP().Special('(')
		for _, keyword := range returns {
			enc.Atom(keyword).SP()
		}
		enc.Atom("STATUS").SP().List(len(items), func(i int) { enc.Atom(string(items[i])) })
		enc.Special(')')
	}, func(resp *untaggedResponse) (bool, error) {
		if claimed, err := lists(resp); claimed || err != nil {
			return claimed, err
		}
		return statuses(resp)
	})
	return &ListCommand{Command: cmd, data: &data}
}

// listStatusCollector claims the untagged STATUS responses that RFC 5819
// section 2 interleaves with the LIST responses. It reuses the STATUS parser
// rather than repeating it, so an extension STATUS item this package does not
// model yet survives here exactly as it does after a plain STATUS command.
func listStatusCollector(handler func(*StatusData), limit int) commandCollector {
	count := 0
	return func(resp *untaggedResponse) (bool, error) {
		if resp.name != "STATUS" || resp.hasNum || resp.cond != nil {
			return false, nil
		}
		// A server sending unbounded STATUS responses is bounded here as the
		// LIST responses are; the two limits are deliberately separate counters
		// so one cannot mask the other.
		if err := countUntaggedResponse(&count, limit, "LIST RETURN (STATUS)"); err != nil {
			return true, err
		}
		data := &StatusData{Values: make(map[imap.StatusItemKeyword]any)}
		claimed, err := statusCollector(data)(resp)
		if !claimed || err != nil {
			return claimed, err
		}
		if handler != nil {
			handler(data)
		}
		return true, nil
	}
}

// SpecialUseSource records how a special-use assignment was determined. It is a
// string-backed open type, so a future source — an RFC 5464 METADATA entry, for
// instance, which RFC 6154 section 5.4 describes — is a new constant rather
// than a change to [SpecialUseData].
type SpecialUseSource string

const (
	// SpecialUseSourceServer means the server reported the attributes itself
	// through SPECIAL-USE. RFC 6154. The result is authoritative.
	SpecialUseSourceServer SpecialUseSource = "SPECIAL-USE"

	// SpecialUseSourceXList means the attributes came from the non-standard
	// XLIST command, which predates RFC 6154 and is still advertised by Gmail
	// and by Cyrus. The mapping to RFC 6154 attributes is this client's, so
	// the result is a translation rather than a guess.
	SpecialUseSourceXList SpecialUseSource = "XLIST"

	// SpecialUseSourceNameHeuristic means nothing on the server said anything
	// about special use and the mailboxes were matched by name. **The result
	// is a guess.** It is wrong for any user who renamed a mailbox, chose a
	// language this client does not have a table for, or keeps a mailbox
	// called "Archive" that is not their archive. Never destroy mail based on
	// it without asking the user.
	SpecialUseSourceNameHeuristic SpecialUseSource = "name-heuristic"
)

// SpecialUseData maps special-use attributes to mailbox names. RFC 6154.
//
// Construct with keyed fields only; fields may be added in a future release.
type SpecialUseData struct {
	// Source records how the assignment was determined, and therefore how
	// much it can be trusted. See [SpecialUseSource].
	Source SpecialUseSource

	// Mailboxes maps each attribute to every mailbox carrying it. RFC 6154
	// section 2 notes that although there is usually at most one mailbox per
	// attribute, some message stores allow several, so this is a slice rather
	// than a single name.
	Mailboxes map[imap.MailboxAttr][]string

	_ struct{}
}

// Guessed reports whether the assignment came from name matching rather than
// from the server. See [SpecialUseSourceNameHeuristic].
func (d *SpecialUseData) Guessed() bool {
	return d != nil && d.Source == SpecialUseSourceNameHeuristic
}

// Mailbox returns the first mailbox carrying attr, and whether there was one.
func (d *SpecialUseData) Mailbox(attr imap.MailboxAttr) (string, bool) {
	if d == nil {
		return "", false
	}
	for candidate, names := range d.Mailboxes {
		if candidate.Equal(attr) && len(names) != 0 {
			return names[0], true
		}
	}
	return "", false
}

// SpecialUseOptions configures [Client.SpecialUse]. A nil pointer searches the
// whole personal namespace.
//
// Construct with keyed fields only; fields may be added in a future release.
type SpecialUseOptions struct {
	// Reference is the LIST reference name. The empty default is the personal
	// namespace root.
	Reference string

	// Pattern is the LIST pattern. Empty means "*".
	Pattern string

	// AllowNameHeuristic permits the name-matching guess described on
	// [SpecialUseSourceNameHeuristic] when the server offers neither
	// SPECIAL-USE nor XLIST. It is false by default, because a guess that the
	// caller did not ask for is indistinguishable from a fact once it has been
	// returned.
	AllowNameHeuristic bool

	_ struct{}
}

// SpecialUse reports which mailboxes hold the user's drafts, sent mail, junk,
// trash and archive. SPECIAL-USE, RFC 6154.
//
// # Fallbacks
//
// The result always says where it came from, in [SpecialUseData.Source]:
//
//   - SPECIAL-USE advertised: the server's own attributes, from
//     LIST (SPECIAL-USE) where LIST-EXTENDED is available and from an ordinary
//     LIST otherwise, since RFC 6154 section 6 adds the use attributes to
//     mbx-list-oflag and they therefore appear in any LIST response.
//   - Otherwise XLIST advertised: the non-standard XLIST command, translated.
//     \Spam becomes \Junk and \Starred becomes \Flagged; \Inbox has no RFC 6154
//     equivalent and is dropped.
//   - Otherwise, and only when [SpecialUseOptions.AllowNameHeuristic] is set:
//     mailbox names are matched against a table of common English names. **This
//     is a guess**, reported as [SpecialUseSourceNameHeuristic] and by
//     [SpecialUseData.Guessed]. Without the opt-in this returns
//     [ErrCapabilityNotAdvertised] instead.
func (c *Client) SpecialUse(ctx context.Context, options *SpecialUseOptions) (*SpecialUseData, error) {
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "SPECIAL-USE lookup requires a non-nil context"}
	}
	o := SpecialUseOptions{}
	if options != nil {
		o = *options
	}
	pattern := o.Pattern
	if pattern == "" {
		pattern = "*"
	}

	switch {
	case c.supportsAny("SPECIAL-USE"):
		var listOptions *ListOptions
		if c.supportsAny("LIST-EXTENDED") || c.rev2Enabled() {
			listOptions = &ListOptions{
				SelectionOptions: []ListSelectOption{ListSelectSpecialUse},
				ReturnOptions:    []ListReturnOption{ListReturnSpecialUse},
			}
		}
		data, err := c.List(o.Reference, pattern, listOptions).Wait(ctx)
		if err != nil {
			return nil, err
		}
		return collectSpecialUse(SpecialUseSourceServer, data, knownSpecialUse, nil), nil

	case c.supportsAny("XLIST"):
		data, err := c.xlist(o.Reference, pattern).Wait(ctx)
		if err != nil {
			return nil, err
		}
		return collectSpecialUse(SpecialUseSourceXList, data, knownSpecialUse, xlistAliases), nil

	case o.AllowNameHeuristic:
		data, err := c.list("LIST", o.Reference, pattern, nil).Wait(ctx)
		if err != nil {
			return nil, err
		}
		return guessSpecialUse(data), nil

	default:
		return nil, capabilityError("a special-use mailbox lookup", "SPECIAL-USE or XLIST")
	}
}

// xlist issues the non-standard XLIST command. It predates RFC 6154 and has no
// specification; its response is a LIST response under another name, which is
// why the reply is handed to the ordinary LIST collector rather than to a
// second copy of the same parser.
func (c *Client) xlist(reference, pattern string) *ListCommand {
	data := make([]*ListData, 0)
	inner := listCollector("XLIST", &data, c.maxUntaggedResponses())
	cmd := c.beginCommand("XLIST", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(reference).SP().ListMailbox(pattern)
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.name != "XLIST" {
			return false, nil
		}
		resp.name = "LIST"
		return inner(resp)
	})
	return &ListCommand{Command: cmd, data: &data}
}

// knownSpecialUse is the RFC 6154 section 2 attribute set, plus \Important from
// RFC 8457. Unknown "\" attributes are deliberately not collected: RFC 6154
// section 6 reserves use-attr-ext for future standards and tells clients to
// ignore attributes they do not understand, and \Marked, \Noselect and the
// LIST-EXTENDED attributes are not special uses at all.
var knownSpecialUse = []imap.MailboxAttr{
	imap.MailboxAttrAll,
	imap.MailboxAttrArchive,
	imap.MailboxAttrDrafts,
	imap.MailboxAttrFlagged,
	imap.MailboxAttrJunk,
	imap.MailboxAttrSent,
	imap.MailboxAttrTrash,
	imap.MailboxAttrImportant,
}

// xlistAliases maps the pre-standard XLIST attributes onto their RFC 6154
// equivalents. Gmail's \Spam and \Starred are the two that differ in spelling;
// \Inbox has no RFC 6154 equivalent and is intentionally absent.
var xlistAliases = map[imap.MailboxAttr]imap.MailboxAttr{
	"\\Spam":    imap.MailboxAttrJunk,
	"\\Starred": imap.MailboxAttrFlagged,
	"\\AllMail": imap.MailboxAttrAll,
}

func collectSpecialUse(source SpecialUseSource, data []*ListData, known []imap.MailboxAttr, aliases map[imap.MailboxAttr]imap.MailboxAttr) *SpecialUseData {
	result := &SpecialUseData{Source: source, Mailboxes: make(map[imap.MailboxAttr][]string)}
	for _, item := range data {
		for _, attr := range item.Attrs {
			canonical, ok := canonicalSpecialUse(attr, known, aliases)
			if !ok {
				continue
			}
			result.Mailboxes[canonical] = append(result.Mailboxes[canonical], item.Mailbox)
		}
	}
	return result
}

func canonicalSpecialUse(attr imap.MailboxAttr, known []imap.MailboxAttr, aliases map[imap.MailboxAttr]imap.MailboxAttr) (imap.MailboxAttr, bool) {
	for _, candidate := range known {
		if attr.Equal(candidate) {
			return candidate, true
		}
	}
	for alias, canonical := range aliases {
		if attr.Equal(alias) {
			return canonical, true
		}
	}
	return "", false
}

// specialUseNameGuesses maps lower-cased mailbox leaf names to the special use
// they most often denote. It is English-only on purpose: a table that guessed
// across languages would collide — Italian "Bozze" is drafts, but German
// "Papierkorb" and Dutch "Prullenbak" are both trash and neither resembles the
// other — and every added language raises the chance of a false positive on a
// mailbox the user created for something else.
var specialUseNameGuesses = map[string]imap.MailboxAttr{
	"drafts":           imap.MailboxAttrDrafts,
	"draft":            imap.MailboxAttrDrafts,
	"sent":             imap.MailboxAttrSent,
	"sent items":       imap.MailboxAttrSent,
	"sent messages":    imap.MailboxAttrSent,
	"sent mail":        imap.MailboxAttrSent,
	"trash":            imap.MailboxAttrTrash,
	"deleted":          imap.MailboxAttrTrash,
	"deleted items":    imap.MailboxAttrTrash,
	"deleted messages": imap.MailboxAttrTrash,
	"junk":             imap.MailboxAttrJunk,
	"junk e-mail":      imap.MailboxAttrJunk,
	"spam":             imap.MailboxAttrJunk,
	"bulk mail":        imap.MailboxAttrJunk,
	"archive":          imap.MailboxAttrArchive,
	"archives":         imap.MailboxAttrArchive,
	"all mail":         imap.MailboxAttrAll,
	"flagged":          imap.MailboxAttrFlagged,
	"starred":          imap.MailboxAttrFlagged,
}

// guessSpecialUse matches mailbox names against specialUseNameGuesses. The
// result is explicitly a guess; see [SpecialUseSourceNameHeuristic].
func guessSpecialUse(data []*ListData) *SpecialUseData {
	result := &SpecialUseData{Source: SpecialUseSourceNameHeuristic, Mailboxes: make(map[imap.MailboxAttr][]string)}
	for _, item := range data {
		if imap.ContainsAttr(item.Attrs, imap.MailboxAttrNoSelect) {
			continue
		}
		leaf := item.Mailbox
		if item.Delimiter != 0 {
			if index := strings.LastIndex(leaf, string(item.Delimiter)); index >= 0 {
				leaf = leaf[index+len(string(item.Delimiter)):]
			}
		}
		attr, ok := specialUseNameGuesses[strings.ToLower(leaf)]
		if !ok {
			continue
		}
		result.Mailboxes[attr] = append(result.Mailboxes[attr], item.Mailbox)
	}
	return result
}
