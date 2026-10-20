package imapserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// MULTISEARCH (RFC 7377): the ESEARCH command, which searches mailboxes other
// than the selected one — or several at once.
//
// This is the one search that cannot go through SelectedMailbox: the whole point
// is to search mailboxes the connection has not selected, and selecting each in
// turn would destroy the current selection. It therefore needs a Session-level
// interface.

// MultiSearchSession is the optional MULTISEARCH support of RFC 7377.
//
// The framework resolves the source mailboxes and owns the response encoding;
// the backend evaluates the criteria against each named mailbox and reports the
// matching UIDs per mailbox. The framework expands personal sources using
// NamespaceSession when available (otherwise the default LIST namespace).
// Inboxes uses the personal-source fallback of RFC 5465 section 6.3 because
// this interface does not expose the backend's delivery destinations.
//
// Results are keyed by mailbox because RFC 7377 requires each ESEARCH response
// to name the mailbox and its UIDVALIDITY: a UID means nothing without them, and
// a flat UID list across mailboxes would be unusable.
//
// The criteria carry the same guarantee as [SearchQuery.Criteria], at every
// nesting depth: no [imap.SearchFilter] reaches the backend, because the
// framework substitutes it for the criteria it names first, and no
// [imap.SearchSeqNum] or [imap.SearchSavedResult] reaches it either.
// With no IN clause the source is the selection, so sequence numbers and saved
// results are resolved to UIDs; with an IN clause those keys are refused. The refusal covers an IN clause naming the selected
// mailbox too — the framework compares no names, because a number that resolved
// for one spelling of a mailbox and not another would be worse than a uniform
// refusal.
type MultiSearchSession interface {
	MultiSearch(ctx context.Context, mailboxes []string, criteria imap.SearchCriteria, options *MultiSearchOptions) ([]MultiSearchMailboxResult, error)
}

// MultiSearchOptions configures ESEARCH. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type MultiSearchOptions struct {
	// Charset is the declared charset for string criteria, or empty for the
	// protocol default.
	Charset string `imapfeature:"multisearch"`
	_       struct{}
}

// MultiSearchMailboxResult is one mailbox's contribution to a MULTISEARCH
// result.
// Construct with keyed fields only; fields may be added in a future release.
type MultiSearchMailboxResult struct {
	// Mailbox is the mailbox these UIDs belong to.
	Mailbox string
	// UIDValidity qualifies the UIDs, without which they are meaningless.
	UIDValidity uint32
	// UIDs are the matching messages.
	UIDs []imap.UID
	_    struct{}
}

const featureMultiSearch featureID = "multisearch"

func init() {
	registerFeatures(featureDescriptor{
		ID: featureMultiSearch,
		Active: func(_ *sessionState, advertised map[string]bool) bool {
			return advertised["MULTISEARCH"]
		},
	})
	registerCapabilities(
		capabilityDescriptor{
			Name:            "MULTISEARCH",
			States:          stateMaskAuthenticated | stateMaskSelected,
			Depends:         []string{"ESEARCH"},
			RequiresBackend: sessionImplements[MultiSearchSession](),
		},
	)
	// ESEARCH is valid in the authenticated state, unlike SEARCH: not needing a
	// selection is the extension's whole purpose.
	registerCommand("ESEARCH", stateMaskAuthenticated|stateMaskSelected, false, parseMultiSearch, handleMultiSearch)
}

type multiSearchSource struct {
	kind  string
	names []string
}

type multiSearchArgs struct {
	sources  []multiSearchSource
	charset  string
	criteria imap.SearchCriteria
}

// parseMultiSearch reads the ESEARCH command:
//
//	ESEARCH [IN (source)] [RETURN (...)] [CHARSET x] criteria
//
// RFC 7377 section 2.2. The source list defaults to the selected mailbox.
func parseMultiSearch(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &multiSearchArgs{}
	if decoder.PeekAtomEqual("IN") {
		var keyword string
		if !decoder.ExpectAtom(&keyword) || !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
		if err := decoder.ExpectList(func() error {
			var source multiSearchSource
			if !decoder.ExpectAtom(&source.kind) {
				return decoder.Err()
			}
			source.kind = strings.ToLower(source.kind)
			switch source.kind {
			case "selected", "personal", "subscribed", "inboxes":
			case "mailboxes", "subtree", "subtree-one":
				if !decoder.ExpectSP() {
					return decoder.Err()
				}
				readName := func() error {
					var name string
					if !decoder.ExpectMailbox(&name) {
						return decoder.Err()
					}
					source.names = append(source.names, name)
					return nil
				}
				if decoder.PeekSpecial('(') {
					if err := decoder.ExpectList(readName); err != nil {
						return err
					}
				} else if err := readName(); err != nil {
					return err
				}
				if len(source.names) == 0 {
					return fmt.Errorf("empty ESEARCH mailbox list")
				}
			default:
				return fmt.Errorf("unknown ESEARCH source %q", source.kind)
			}
			args.sources = append(args.sources, source)
			return nil
		}); err != nil {
			return nil, 0, err
		}
		if len(args.sources) == 0 {
			return nil, 0, fmt.Errorf("empty ESEARCH source list")
		}
		if !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
	}
	// This handler currently supports ALL (including the empty RETURN list).
	// Refuse other result options explicitly until their per-mailbox responses
	// are implemented.
	if decoder.PeekAtomEqual("RETURN") {
		var keyword string
		if !decoder.ExpectAtom(&keyword) || !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
		if err := decoder.ExpectList(func() error {
			var option string
			if !decoder.ExpectAtom(&option) {
				return decoder.Err()
			}
			if !strings.EqualFold(option, searchReturnAll) {
				return fmt.Errorf("unsupported ESEARCH return option %q", option)
			}
			return nil
		}); err != nil {
			return nil, 0, err
		}
		if !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
	}
	if decoder.PeekAtomEqual("CHARSET") {
		var keyword string
		if !decoder.ExpectAtom(&keyword) || !decoder.ExpectSP() ||
			!decoder.ExpectAstring(&args.charset) || !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
	}
	criteria, err := imapcodec.ReadSearchCriteria(decoder)
	if err != nil {
		return nil, 0, err
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	args.criteria = criteria
	cost := int64(len(args.charset)) + 64
	for _, source := range args.sources {
		cost += int64(len(source.kind)) + 32
		for _, name := range source.names {
			cost += int64(len(name)) + 16
		}
	}
	return args, cost, nil
}

func handleMultiSearch(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*multiSearchArgs)
	if args == nil || args.criteria == nil {
		return c.writeBad(command.tag, "invalid ESEARCH arguments")
	}
	if err := requireCapability(c, "MULTISEARCH"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(MultiSearchSession)
	if !ok {
		return c.writeBad(command.tag, "MULTISEARCH is not available")
	}
	selectedIsTheSource := len(args.sources) == 0
	mailboxes, err := resolveMultiSearchSources(ctx, c, args.sources)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	criteria, err := applySearchFilters(ctx, c, args.criteria)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	// A sequence number indexes into one mailbox's message list, and MultiSearch
	// hands the backend raw criteria with no selection attached. Where the source
	// *is* the selection, the framework resolves them to UIDs exactly as SEARCH
	// does. Where it is not, there is nothing to resolve against and the number
	// would reach the backend meaning nothing — so it is refused rather than
	// answered wrongly, since an empty result reads as a successful search.
	if searchMentionsSavedResult(criteria) && !selectedIsTheSource {
		return c.writeBad(command.tag, "ESEARCH with an IN clause cannot use a saved search result; use UIDs")
	}
	if searchMentionsSeqNum(criteria) && !selectedIsTheSource {
		return c.writeBad(command.tag,
			"ESEARCH with an IN clause cannot use message sequence numbers; use UIDs")
	}
	if err := requireCriteriaCapabilities(c, criteria); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if selectedIsTheSource {
		criteria = normalizeSearchCriteria(criteria, c.state.selected.uids, c.state.selected.savedSearch)
	}
	results, err := session.MultiSearch(ctx, mailboxes, criteria, &MultiSearchOptions{Charset: args.charset})
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	for _, result := range results {
		// A mailbox that matched nothing is skipped: RFC 7377 section 2.1 sends
		// no ESEARCH response for it, which is how a client tells "no matches"
		// from "mailbox not searched".
		if len(result.UIDs) == 0 {
			continue
		}
		if err := writeMultiSearchResult(c, command.tag, &result); err != nil {
			return err
		}
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

// resolveMultiSearchSources expands filters without changing the selection.
// List supplies canonical names and delimiters; deduplication also bounds a
// backend hierarchy with aliases or loops to one visit per mailbox name.
func resolveMultiSearchSources(ctx context.Context, c *conn, sources []multiSearchSource) ([]string, error) {
	if len(sources) == 0 {
		sources = []multiSearchSource{{kind: "selected"}}
	}
	var names []string
	seen := make(map[string]bool)
	add := func(name string) {
		key := name
		if strings.EqualFold(name, "INBOX") {
			key = "INBOX"
		}
		if !seen[key] {
			seen[key] = true
			names = append(names, name)
		}
	}
	for _, source := range sources {
		switch source.kind {
		case "selected":
			if c.state.selected == nil {
				return nil, &imap.Error{Type: imap.ErrorTypeBad, Text: "ESEARCH selected source requires a selected mailbox"}
			}
			add(c.state.selected.name)
		case "mailboxes":
			for _, name := range source.names {
				add(name)
			}
		default:
			// Without NAMESPACE, LIST's default namespace is the personal one.
			prefixes := []string{""}
			if source.kind == "personal" || source.kind == "inboxes" {
				if ns, ok := c.state.session.(NamespaceSession); ok {
					data, err := ns.Namespace(ctx, nil)
					if err != nil {
						return nil, err
					}
					if data == nil {
						return nil, fmt.Errorf("NAMESPACE returned no data")
					}
					prefixes = nil
					for _, namespace := range data.Personal {
						prefixes = append(prefixes, namespace.Prefix)
					}
				}
			}
			writer := newListWriter(func(_ context.Context, data *imap.ListData) error {
				if data == nil || slices.Contains(data.Attrs, imap.MailboxAttrNoSelect) || slices.Contains(data.Attrs, imap.MailboxAttrNonExistent) {
					return nil
				}
				match := source.kind == "subscribed"
				if source.kind == "personal" || source.kind == "inboxes" {
					// INBOX is personal even under a prefix such as "INBOX.".
					match = strings.EqualFold(data.Mailbox, "INBOX")
					for _, prefix := range prefixes {
						match = match || strings.HasPrefix(data.Mailbox, prefix)
					}
				} else if source.kind == "subtree" || source.kind == "subtree-one" {
					for _, parent := range source.names {
						if mailboxNamesEqual(data.Mailbox, parent) {
							match = true
						} else if data.Delimiter != 0 {
							prefix := strings.TrimSuffix(parent, string(data.Delimiter)) + string(data.Delimiter)
							if child, ok := strings.CutPrefix(data.Mailbox, prefix); ok {
								match = match || source.kind == "subtree" || !strings.ContainsRune(child, data.Delimiter)
							}
						}
					}
				}
				if match {
					add(data.Mailbox)
				}
				return nil
			})
			err := c.state.session.List(ctx, writer, "", []string{"*"}, &ListOptions{Subscribed: source.kind == "subscribed"})
			writer.core.close()
			if err != nil {
				return nil, err
			}
		}
	}
	return names, nil
}

// writeMultiSearchResult writes one mailbox's ESEARCH response, carrying the
// MAILBOX and UIDVALIDITY correlators of RFC 7377 section 2.1 so the UIDs mean
// something.
func writeMultiSearchResult(c *conn, tag string, result *MultiSearchMailboxResult) error {
	numbers := make([]uint32, 0, len(result.UIDs))
	for _, uid := range result.UIDs {
		if uid != 0 {
			numbers = append(numbers, uint32(uid))
		}
	}
	if len(numbers) == 0 {
		return nil
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("ESEARCH").SP().
		Special('(').Atom("TAG").SP().Quoted(tag).SP().
		Atom("MAILBOX").SP().String(result.Mailbox).SP().
		Atom("UIDVALIDITY").SP().Number(result.UIDValidity).Special(')').
		SP().Atom("UID").
		SP().Atom(searchReturnAll).SP().Atom(numberSetString(numbers)).CRLF()
	return c.encoder.Flush()
}
