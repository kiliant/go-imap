package imapclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ListReturnSpecialUse asks the server to report special-use attributes on the
// mailboxes it lists. SPECIAL-USE, RFC 6154 section 2. It is implied by the
// [ListSelectSpecialUse] selection option.
const ListReturnSpecialUse ListReturnOptionKeyword = "SPECIAL-USE"

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
func (c *Client) ListMailboxes(ctx context.Context, reference, pattern string, options *ListOptions) ([]*ListData, error) {
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "LIST requires a non-nil context"}
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
