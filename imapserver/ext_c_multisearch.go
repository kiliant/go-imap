package imapserver

import (
	"context"
	"fmt"
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
// matching UIDs per mailbox.
//
// Results are keyed by mailbox because RFC 7377 requires each ESEARCH response
// to name the mailbox and its UIDVALIDITY: a UID means nothing without them, and
// a flat UID list across mailboxes would be unusable.
//
// The criteria carry the same guarantee as [SearchQuery.Criteria], at every
// nesting depth: no [imap.SearchFilter] reaches the backend, because the
// framework substitutes it for the criteria it names first, and no
// [imap.SearchSeqNum] reaches it either. With no IN clause the source is the
// selection, so sequence numbers are resolved to UIDs; with an IN clause the
// command is refused. The refusal covers an IN clause naming the selected
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

type multiSearchArgs struct {
	mailboxes []string
	charset   string
	criteria  imap.SearchCriteria
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
			var scope string
			if !decoder.ExpectAstring(&scope) {
				return decoder.Err()
			}
			// RFC 7377 section 2.2 defines scope *options* such as "subtree"
			// alongside plain mailbox names. Only plain names are supported;
			// a scope option is refused rather than silently read as a mailbox
			// name, which would search the wrong thing.
			if strings.HasPrefix(scope, "subtree") || strings.HasPrefix(scope, "mailboxes") ||
				strings.EqualFold(scope, "personal") || strings.EqualFold(scope, "inboxes") {
				return fmt.Errorf("unsupported ESEARCH scope option %q", scope)
			}
			args.mailboxes = append(args.mailboxes, scope)
			return nil
		}); err != nil {
			return nil, 0, err
		}
		if !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
	}
	// A RETURN clause is accepted and ignored beyond ALL: RFC 7377 inherits
	// ESEARCH's options, and ALL is the only one whose meaning is unambiguous
	// across several mailboxes. MIN and MAX over a union of mailboxes would have
	// to pick a mailbox to be the minimum of, which the RFC does not define.
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
	return args, int64(len(args.charset)+len(args.mailboxes)*32) + 64, nil
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
	mailboxes := args.mailboxes
	selectedIsTheSource := len(mailboxes) == 0
	if selectedIsTheSource {
		// With no IN clause the source is the selected mailbox, and there must
		// be one. RFC 7377 section 2.2.
		if c.state.selected == nil {
			return c.writeBad(command.tag, "ESEARCH without IN requires a selected mailbox")
		}
		mailboxes = []string{c.state.selected.name}
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
	if searchMentionsSeqNum(criteria) {
		if !selectedIsTheSource {
			return c.writeBad(command.tag,
				"ESEARCH with an IN clause cannot use message sequence numbers; use UIDs")
		}
		criteria = normalizeSearchCriteria(criteria, c.state.selected.uids)
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
