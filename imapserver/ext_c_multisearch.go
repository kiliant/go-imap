package imapserver

import (
	"cmp"
	"context"
	"fmt"
	"math"
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
// Result options are the framework's too. It derives MIN, MAX, ALL, COUNT and
// PARTIAL per mailbox from the returned UIDs (RFC 7377 section 2.1), applies
// SAVE and CONTEXT=SEARCH UPDATE to the selected mailbox only (section 2.2),
// and reports MODSEQ from [MultiSearchMailboxResult.ModSeqs]. A backend
// therefore returns every matching UID and nothing else. A mailbox the user
// may not read is omitted, not reported as an error (RFC 7377 section 2.2).
//
// Results are keyed by mailbox because RFC 7377 requires each ESEARCH response
// to name the mailbox and its UIDVALIDITY: a UID means nothing without them, and
// a flat UID list across mailboxes would be unusable.
//
// The criteria carry the same guarantee as [SearchQuery.Criteria], at every
// nesting depth: no [imap.SearchFilter] reaches the backend, because the
// framework substitutes it for the criteria it names first, and no
// [imap.SearchSeqNum] or [imap.SearchSavedResult] reaches it either.
// When the selection is the only source — no IN clause, or "selected" alone,
// which RFC 7377 section 2.2 makes equivalent — sequence numbers and saved
// results are resolved to UIDs; with any other source those keys are refused.
// The refusal covers a "mailboxes" source naming the selected mailbox too —
// the framework compares no names, because a number that resolved for one
// spelling of a mailbox and not another would be worse than a uniform refusal.
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
	// ModSeqs optionally pairs each UID with its modification sequence, by
	// position, and then has the same length as UIDs. Supply it when the
	// criteria contain MODSEQ: RFC 7162 section 3.1.5 makes the ESEARCH response
	// report the highest modification sequence of the messages it returns, and
	// for MIN, MAX or PARTIAL (RFC 4731 section 3.2) only the framework knows
	// which those are. Without it the framework omits MODSEQ.
	ModSeqs []uint64
	_       struct{}
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
	// result holds the RETURN options, parsed and validated as for SEARCH.
	result searchArgs
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
	// RFC 7377 section 2 defines no result options of its own: ESEARCH takes
	// those of extended SEARCH (RFC 4731, 5182, 5267, 9394), so it shares their
	// parser. An absent clause means ALL here (section 2.1).
	if err := parseSearchReturnOptions(decoder, &args.result); err != nil {
		return nil, 0, err
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
	if err := validateSearchReturnOptions(c, &args.result); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	if err := validateUTF8AcceptCharset(c, args.charset, false); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	// RFC 7377 section 2.2: an absent source list means "selected", so either
	// form makes the selection the sole source. SAVE is valid only then; UPDATE
	// applies only to the selected mailbox and needs one.
	selectedOnly := !slices.ContainsFunc(args.sources, func(source multiSearchSource) bool { return source.kind != "selected" })
	selectedIsTheSource := selectedOnly
	requested := searchReturnSet(&args.result)
	if requested[searchReturnSave] && !selectedOnly {
		return c.writeBad(command.tag, "ESEARCH SAVE requires the selected mailbox as the only source")
	}
	if requested[searchReturnUpd] && c.state.selected == nil {
		return c.writeBad(command.tag, "ESEARCH UPDATE requires a selected mailbox")
	}
	mailboxes, err := resolveMultiSearchSources(ctx, c, args.sources)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if requested[searchReturnUpd] && !slices.ContainsFunc(mailboxes, func(name string) bool { return mailboxNamesEqual(name, c.state.selected.name) }) {
		return c.writeBad(command.tag, "ESEARCH UPDATE applies only to the selected mailbox, which is not searched")
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
	// RFC 7162 section 3.1: a SEARCH using MODSEQ enables CONDSTORE. A NOMODSEQ
	// selection refuses it only when that mailbox is among those searched.
	if searchHasModSeq(criteria) {
		enable := enableCondStoreForCommand
		if c.state.selected != nil && slices.ContainsFunc(mailboxes, func(name string) bool { return mailboxNamesEqual(name, c.state.selected.name) }) {
			enable = validateSelectedCondStoreUse
		}
		if err := enable(ctx, c, true, "ESEARCH MODSEQ"); err != nil {
			return writeCondStoreUseError(c, command.tag, command.name, err)
		}
	}
	if selectedIsTheSource {
		criteria = normalizeSearchCriteria(criteria, c.state.selected.uids, c.state.selected.savedSearch)
	}
	results, err := session.MultiSearch(ctx, mailboxes, criteria, &MultiSearchOptions{Charset: args.charset})
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	args.result.criteria, args.result.charset = criteria, args.charset
	withModSeq := searchHasModSeq(criteria)
	selectedSeen := false
	for i := range results {
		result := &results[i]
		if len(result.UIDs) > maxCommandSearchResults {
			return writeTaggedCondition(c, command.tag, "NO", imap.CodeLimit, "", "ESEARCH result limit exceeded")
		}
		selected := c.state.selected != nil && mailboxNamesEqual(result.Mailbox, c.state.selected.name)
		uids, modSeqs, err := multiSearchMatches(c, result, selected)
		if err != nil {
			return writeBackendError(c, command.tag, command.name, err)
		}
		if selected && !selectedSeen {
			selectedSeen = true
			saveSearchResult(c, &args.result, uids)
			if requested[searchReturnUpd] {
				putSearchContext(c, &searchContext{
					tag: command.tag, uidMode: true, charset: args.charset,
					query:   newSearchQuery(criteria, c.state.selected.uids, c.state.selected.savedSearch),
					mailbox: result.Mailbox, uidValidity: result.UIDValidity,
				}, uids)
			}
		}
		// A mailbox that matched nothing is skipped: RFC 7377 section 2.1 sends
		// no ESEARCH response for it, which is how a client tells "no matches"
		// from "mailbox not searched". That holds for MIN, MAX and COUNT too.
		// SAVE alone returns no data (RFC 5182).
		if len(uids) == 0 || len(requested) == 1 && requested[searchReturnSave] {
			continue
		}
		var modSeq uint64
		if withModSeq && modSeqs != nil {
			modSeq = multiSearchModSeq(&args.result, requested, modSeqs)
		}
		numbers := make([]uint32, len(uids))
		for j, uid := range uids {
			numbers[j] = uint32(uid)
		}
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("ESEARCH").SP().
			Special('(').Atom("TAG").SP().Quoted(command.tag).SP().
			Atom("MAILBOX").SP().Mailbox(result.Mailbox).SP().
			Atom("UIDVALIDITY").SP().Number(result.UIDValidity).Special(')').
			SP().Atom("UID")
		writeESearchData(c, &args.result, requested, numbers, modSeq)
		c.encoder.CRLF()
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	if !selectedSeen && c.state.selected != nil {
		// The backend reported nothing for the selected mailbox: an empty
		// result still replaces a saved result and starts an empty context.
		saveSearchResult(c, &args.result, nil)
		if requested[searchReturnUpd] {
			putSearchContext(c, &searchContext{
				tag: command.tag, uidMode: true, charset: args.charset,
				query:   newSearchQuery(criteria, c.state.selected.uids, c.state.selected.savedSearch),
				mailbox: c.state.selected.name, uidValidity: c.state.selected.uidValidity,
			}, nil)
		}
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

// multiSearchMatches returns one mailbox's matching UIDs sorted and
// deduplicated, with their modification sequences when the backend supplied
// them. For the selected mailbox it keeps only the UIDs in the client's view,
// as SEARCH does.
func multiSearchMatches(c *conn, result *MultiSearchMailboxResult, selected bool) ([]imap.UID, []uint64, error) {
	paired := len(result.ModSeqs) == len(result.UIDs)
	if result.ModSeqs != nil && !paired {
		return nil, nil, fmt.Errorf("imapserver: MULTISEARCH returned %d mod-sequences for %d UIDs", len(result.ModSeqs), len(result.UIDs))
	}
	type match struct {
		uid    imap.UID
		modSeq uint64
	}
	matches := make([]match, 0, len(result.UIDs))
	for i, uid := range result.UIDs {
		if uid == 0 {
			return nil, nil, fmt.Errorf("imapserver: backend MULTISEARCH returned UID zero")
		}
		if selected {
			if _, visible := c.state.selected.sequence(uid); !visible {
				continue
			}
		}
		var modSeq uint64
		if paired {
			modSeq = result.ModSeqs[i]
			if modSeq > math.MaxInt64 {
				return nil, nil, fmt.Errorf("imapserver: invalid MULTISEARCH modification sequence")
			}
		}
		matches = append(matches, match{uid: uid, modSeq: modSeq})
	}
	slices.SortFunc(matches, func(a, b match) int { return cmp.Compare(a.uid, b.uid) })
	matches = slices.CompactFunc(matches, func(a, b match) bool { return a.uid == b.uid })
	uids := make([]imap.UID, len(matches))
	var modSeqs []uint64
	if paired {
		modSeqs = make([]uint64, len(matches))
	}
	for i, m := range matches {
		uids[i] = m.uid
		if paired {
			modSeqs[i] = m.modSeq
		}
	}
	return uids, modSeqs, nil
}

// multiSearchModSeq applies RFC 4731 section 3.2 to one mailbox's result:
// MIN and MAX alone report their messages' clock, PARTIAL alone its window's,
// and anything else the highest clock among all the matches.
func multiSearchModSeq(args *searchArgs, requested map[string]bool, modSeqs []uint64) uint64 {
	if !requested[searchReturnAll] && !requested[searchReturnCount] && !requested[searchReturnMin] &&
		!requested[searchReturnMax] && !requested[searchReturnPartial] {
		return 0
	}
	subset := modSeqs
	if !requested[searchReturnAll] && !requested[searchReturnCount] {
		subset = nil
		if requested[searchReturnMin] {
			subset = append(subset, modSeqs[0])
		}
		if requested[searchReturnMax] {
			subset = append(subset, modSeqs[len(modSeqs)-1])
		}
		if requested[searchReturnPartial] && args.partial != nil {
			positions := make([]uint32, len(modSeqs))
			for i := range positions {
				positions[i] = uint32(i)
			}
			for _, i := range partialWindow(args.partial, positions) {
				subset = append(subset, modSeqs[i])
			}
		}
	}
	var highest uint64
	for _, modSeq := range subset {
		highest = max(highest, modSeq)
	}
	return highest
}

// resolveMultiSearchSources expands filters without changing the selection.
// List supplies canonical names and delimiters; deduplication also bounds a
// backend hierarchy with aliases or loops to one visit per mailbox name. Each
// LIST is made at most once per command and bounded like any LIST result, and
// the source list as a whole is held to the same bound (RFC 7377 section 2.4).
func resolveMultiSearchSources(ctx context.Context, c *conn, sources []multiSearchSource) ([]string, error) {
	if len(sources) == 0 {
		sources = []multiSearchSource{{kind: "selected"}}
	}
	var names []string
	seen := make(map[string]bool)
	add := func(name string) error {
		key := name
		if strings.EqualFold(name, "INBOX") {
			key = "INBOX"
		}
		if seen[key] {
			return nil
		}
		if len(names) >= maxCommandListResults {
			return commandLimitError("ESEARCH source limit exceeded")
		}
		seen[key] = true
		names = append(names, name)
		return nil
	}
	listed := make(map[bool][]*imap.ListData)
	list := func(subscribed bool) ([]*imap.ListData, error) {
		if data, ok := listed[subscribed]; ok {
			return data, nil
		}
		results, err := collectList(ctx, c.state.session, "", []string{"*"}, &ListOptions{Subscribed: subscribed})
		if err != nil {
			return nil, err
		}
		data := make([]*imap.ListData, 0, len(results))
		for _, item := range results {
			if !slices.Contains(item.Attrs, imap.MailboxAttrNoSelect) && !slices.Contains(item.Attrs, imap.MailboxAttrNonExistent) {
				data = append(data, item)
			}
		}
		slices.SortFunc(data, func(a, b *imap.ListData) int { return strings.Compare(a.Mailbox, b.Mailbox) })
		listed[subscribed] = data
		return data, nil
	}
	var personal func(string) bool
	for _, source := range sources {
		switch source.kind {
		case "selected":
			if c.state.selected == nil {
				return nil, &imap.Error{Type: imap.ErrorTypeBad, Text: "ESEARCH selected source requires a selected mailbox"}
			}
			if err := add(c.state.selected.name); err != nil {
				return nil, err
			}
			continue
		case "mailboxes":
			for _, name := range source.names {
				if err := add(name); err != nil {
					return nil, err
				}
			}
			continue
		case "personal", "inboxes":
			if personal == nil {
				var err error
				if personal, err = personalNamespaceMatcher(ctx, c); err != nil {
					return nil, err
				}
			}
		}
		mailboxes, err := list(source.kind == "subscribed")
		if err != nil {
			return nil, err
		}
		for _, data := range mailboxes {
			match := source.kind == "subscribed"
			switch source.kind {
			case "personal", "inboxes":
				match = personal(data.Mailbox)
			case "subtree", "subtree-one":
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
				if err := add(data.Mailbox); err != nil {
					return nil, err
				}
			}
		}
	}
	return names, nil
}

// personalNamespaceMatcher reports whether a listed name lies in the user's
// personal namespaces (RFC 2342, RFC 5465 section 6.2). Without NAMESPACE the
// LIST default namespace is the personal one. With it, a name belongs to the
// namespace with the longest matching prefix, so a shared "#shared/" namespace
// is not swallowed by an empty personal prefix. INBOX is always personal, even
// under a prefix such as "INBOX.".
func personalNamespaceMatcher(ctx context.Context, c *conn) (func(string) bool, error) {
	ns, ok := c.state.session.(NamespaceSession)
	if !ok {
		return func(string) bool { return true }, nil
	}
	data, err := ns.Namespace(ctx, nil)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, fmt.Errorf("NAMESPACE returned no data")
	}
	return func(name string) bool {
		if strings.EqualFold(name, "INBOX") {
			return true
		}
		best, isPersonal := -1, false
		for class, namespaces := range [][]imap.NamespaceDescriptor{data.Personal, data.OtherUsers, data.Shared} {
			for _, namespace := range namespaces {
				if strings.HasPrefix(name, namespace.Prefix) && len(namespace.Prefix) > best {
					best, isPersonal = len(namespace.Prefix), class == 0
				}
			}
		}
		return isPersonal
	}, nil
}
