package imapserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ESORT and CONTEXT=SEARCH / CONTEXT=SORT (RFC 5267).
//
// ESORT is the SORT counterpart of ESEARCH: MIN, MAX, ALL and COUNT computed
// from the ordered result the backend already returns, so it needs no backend
// surface beyond the SORT support that must be there anyway.
//
// CONTEXT is the harder half. `SEARCH RETURN (UPDATE)` asks the server to keep
// reporting changes to a search result *after* the command completes, as ADDTO
// and REMOVEFROM items on further untagged ESEARCH responses. That is a
// notification lifetime outliving the command, which is why this was left
// unimplemented until NOTIFY's session-scoped machinery existed — and why it
// became tractable once it did.
//
// The framework holds each context and refreshes its query after queued mailbox
// changes, on the event loop after backend mutations return. Backends need no
// additional interface for CONTEXT=SEARCH.

// SORT and SEARCH return options from RFC 5267 section 4.
const (
	sortReturnMin    = "MIN"
	sortReturnMax    = "MAX"
	sortReturnAll    = "ALL"
	sortReturnCount  = "COUNT"
	searchReturnCtx  = "CONTEXT"
	searchReturnUpd  = "UPDATE"
	cancelUpdateName = "CANCELUPDATE"
)

// searchContext is one registered search result, kept for the life of the
// selection or until CANCELUPDATE.
//
// It stores the matching UIDs rather than sequence numbers: the mailbox changes
// underneath it by definition, and a sequence number would go stale on the first
// expunge — which is precisely the event the client registered to hear about.
type searchContext struct {
	tag     string
	uids    map[imap.UID]struct{}
	uidMode bool
	query   *SearchQuery
	charset string
	// A sorted context also retains its requested order. Its update positions
	// are explicit; the zero position is reserved for mailbox-order SEARCH.
	sorted  bool
	keys    []imap.SortKeySpec
	ordered []imap.UID
	// comparatorSelections is the connection's count when the backend last
	// ordered every entry of ordered.
	comparatorSelections uint64
	// mailbox and uidValidity are set for a MULTISEARCH registration, whose
	// updates carry the MAILBOX and UIDVALIDITY correlators (RFC 7377 2.1).
	mailbox     string
	uidValidity uint32
}

func init() {
	registerCapabilities(
		// ESORT rides on SORT: without an ordered result there is nothing to
		// return a window of.
		capabilityDescriptor{
			Name:    "ESORT",
			States:  stateMaskAuthenticated | stateMaskSelected,
			Depends: []string{"SORT", "ESEARCH"},
		},
		capabilityDescriptor{
			Name:    "CONTEXT=SEARCH",
			States:  stateMaskAuthenticated | stateMaskSelected,
			Depends: []string{"ESEARCH"},
		},
		capabilityDescriptor{
			Name:    "CONTEXT=SORT",
			States:  stateMaskAuthenticated | stateMaskSelected,
			Depends: []string{"SORT", "ESEARCH"},
		},
	)
	registerCommand(cancelUpdateName, stateMaskSelected, false, parseCancelUpdate, handleCancelUpdate)
}

// sortReturnSet resolves which items an ESORT response carries, on the same
// terms as ESEARCH: an empty RETURN list means ALL.
func sortReturnSet(options []string) map[string]bool {
	requested := make(map[string]bool, len(options))
	for _, option := range options {
		requested[option] = true
	}
	if !requested[sortReturnMin] && !requested[sortReturnMax] &&
		!requested[sortReturnAll] && !requested[sortReturnCount] && !requested[searchReturnPartial] {
		requested[sortReturnAll] = true
	}
	return requested
}

// validateSortReturnOptions rejects options this server does not implement.
func validateSortReturnOptions(c *conn, options []string) error {
	if len(options) == 0 {
		return nil
	}
	for _, option := range options {
		switch option {
		case sortReturnMin, sortReturnMax, sortReturnAll, sortReturnCount:
			if err := requireCapability(c, "ESORT"); err != nil {
				return err
			}
		case searchReturnUpd, searchReturnCtx, searchReturnPartial:
			// PARTIAL is RFC 5267 section 4.4's, which CONTEXT=SORT brings to
			// SORT; RFC 9394 extends the same option for SEARCH.
			if err := requireCapability(c, "CONTEXT=SORT"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported SORT return option %q", option)
		}
	}
	return nil
}

// registerSearchContext records a result the client asked to keep hearing about.
//
// Active context tags have already been checked before executing the command.
func registerSearchContext(c *conn, tag string, uids []imap.UID, uidMode bool, args *searchArgs) {
	if c.state.selected == nil {
		return
	}
	putSearchContext(c, &searchContext{tag: tag, uidMode: uidMode, query: newSearchQuery(args.criteria, c.state.selected.uids, c.state.selected.savedSearch), charset: args.charset}, uids)
}

func registerSortContext(c *conn, tag string, uids []imap.UID, uidMode bool, query *SearchQuery, args *sortArgs) {
	putSearchContext(c, &searchContext{
		tag: tag, uidMode: uidMode, query: query, charset: args.charset,
		sorted: true, keys: slices.Clone(args.keys), ordered: slices.Clone(uids),
		comparatorSelections: c.comparatorSelections,
	}, uids)
}

// UPDATE tags stay in use until cancellation or selection teardown. Refusing
// reuse before backend access also leaves the original registration intact.
func validateContextTag(c *conn, tag string, options []string) error {
	if c.state.selected == nil || !slices.Contains(options, searchReturnUpd) {
		return nil
	}
	for _, held := range c.state.selected.contexts {
		if held.tag == tag {
			return fmt.Errorf("UPDATE tag is already in use")
		}
	}
	return nil
}

// putSearchContext installs held for uids, replacing a registration with the
// same tag.
func putSearchContext(c *conn, held *searchContext, uids []imap.UID) {
	held.uids = make(map[imap.UID]struct{}, len(uids))
	for _, uid := range uids {
		held.uids[uid] = struct{}{}
	}
	contexts := c.state.selected.contexts
	for i := range contexts {
		if contexts[i].tag == held.tag {
			contexts[i] = held
			return
		}
	}
	c.state.selected.contexts = append(contexts, held)
}

func parseCancelUpdate(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	var tags []string
	for {
		var tag string
		if !decoder.ExpectAstring(&tag) {
			return nil, 0, decoder.Err()
		}
		tags = append(tags, tag)
		if !decoder.SP() {
			break
		}
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return tags, int64(len(tags) * 16), nil
}

// handleCancelUpdate drops registrations. RFC 5267 section 4.4.
//
// Cancelling a tag the server does not hold is not an error: the client may be
// cancelling a context the server already dropped when the mailbox was
// reselected, and failing would make that race visible as a spurious error.
func handleCancelUpdate(_ context.Context, c *conn, command *queuedCommand) error {
	tags, _ := command.args.([]string)
	// RFC 5267 section 4.4 defines CANCELUPDATE for contexts created by either
	// extension. Requiring CONTEXT=SEARCH alone left a backend advertising only
	// CONTEXT=SORT able to create an updating context through SORT and unable to
	// cancel it — the client's only remaining exit being to drop the selection.
	if err := requireAnyCapability(c, "CONTEXT=SEARCH", "CONTEXT=SORT"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	if c.state.selected != nil {
		c.state.selected.contexts = slices.DeleteFunc(c.state.selected.contexts, func(held *searchContext) bool {
			return slices.Contains(tags, held.tag)
		})
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

// Refresh contexts on the event-loop after backend mutations have returned and
// their queued changes have been applied. No backend callback is re-entered.
// UID membership survives renumbering. An expunged match is reported before
// the EXPUNGE, in the numbers the client still holds (RFC 5267 section 4.3.4);
// every other change follows the FETCH or EXISTS that caused it, in current
// numbers (RFC 5465 sections 5.1 and 5.2).
type searchContextDelta struct {
	context               *searchContext
	expunged, gone, added []contextRun
}

// contextRun is one context position with the results removed starting there.
// A mailbox-order SEARCH context uses position zero.
type contextRun struct {
	position uint32
	numbers  []uint32
}

// contextChangeWaits judges, against the queue as it stands now, whether a
// change in a message's context membership must wait. Take it after the
// backend call: a change the call already saw is queued by then.
//
// A message the client knew before this drain, and that the drain did not
// expunge, changes membership because of a flag change or a removal. When the
// client has not been sent that change — it is still queued, or NOTIFY
// retains the flags — the ADDTO or REMOVEFROM waits for the drain that
// reports it. Otherwise it would precede its FETCH (RFC 5465 section 5.1),
// announce a removal from a message the client still sees, or leak an event
// NOTIFY suppresses. Messages new to the client are judged as they are: their
// EXISTS is what the ADDTO follows.
func contextChangeWaits(selected *selectedState, removed, before map[imap.UID]imap.SeqNum) func(imap.UID) bool {
	queued := selected.queue.pendingMessageChanges()
	return func(uid imap.UID) bool {
		if _, known := before[uid]; !known {
			return false
		}
		if _, expunged := removed[uid]; expunged {
			return false
		}
		if _, retained := selected.pendingFlags[uid]; retained {
			return true
		}
		return queued(uid)
	}
}

func refreshSearchContexts(c *conn, removed, beforeSequences map[imap.UID]imap.SeqNum) []searchContextDelta {
	selected := c.state.selected
	if selected == nil || len(selected.contexts) == 0 {
		return nil
	}
	// A drain runs before, between or after commands, so no command context
	// bounds these backend calls. One CommandTimeout covers the whole pass:
	// the client chooses how many contexts it registers, and a per-call bound
	// would let that number multiply how long the event loop is held. A
	// context whose refresh misses the deadline gets NOUPDATE and is dropped,
	// as for any other refresh failure.
	ctx, cancel := context.WithTimeout(c.ctx, c.server.options.Limits.CommandTimeout)
	defer cancel()
	var deltas []searchContextDelta
	var live []*searchContext
	for _, held := range selected.contexts {
		if held.sorted {
			delta, err := refreshSortContext(ctx, c, held, removed, beforeSequences)
			if err != nil {
				writeNoUpdate(c, held.tag, "sort context could not be refreshed")
				continue
			}
			live = append(live, held)
			if delta.changed() {
				deltas = append(deltas, delta)
			}
			continue
		}
		result, err := selected.mailbox.Search(ctx, held.query, &SearchOptions{Charset: held.charset})
		if err == nil {
			// Backends need not return promptly on cancellation. A success
			// after the pass's deadline is a miss all the same.
			err = ctx.Err()
		}
		if err != nil || result == nil || len(result.UIDs) > maxCommandSearchResults {
			writeNoUpdate(c, held.tag, "search context could not be refreshed")
			continue
		}
		waits := contextChangeWaits(selected, removed, beforeSequences)
		current := make(map[imap.UID]struct{}, len(result.UIDs))
		var added, gone, expunged []uint32
		for _, uid := range result.UIDs {
			seq, visible := selected.sequence(uid)
			if !visible {
				continue
			}
			if _, duplicate := current[uid]; duplicate {
				continue
			}
			if _, known := held.uids[uid]; !known {
				if waits(uid) {
					continue
				}
				added = append(added, held.number(uid, seq))
			}
			current[uid] = struct{}{}
		}
		for uid := range held.uids {
			if _, matches := current[uid]; matches {
				continue
			}
			if seq, ok := removed[uid]; ok {
				expunged = append(expunged, held.number(uid, seq))
				continue
			}
			seq, visible := selected.sequence(uid)
			if !visible {
				continue
			}
			if removalWithheld(selected, uid) || waits(uid) {
				current[uid] = struct{}{}
				continue
			}
			gone = append(gone, held.number(uid, seq))
		}
		held.uids = current
		live = append(live, held)
		slices.Sort(added)
		slices.Sort(gone)
		slices.Sort(expunged)
		delta := searchContextDelta{context: held, expunged: unsortedRun(expunged), gone: unsortedRun(gone), added: unsortedRun(added)}
		if delta.changed() {
			deltas = append(deltas, delta)
		}
	}
	selected.contexts = live
	return deltas
}

func (d searchContextDelta) changed() bool {
	return len(d.expunged) != 0 || len(d.gone) != 0 || len(d.added) != 0
}

// number is how updates name a message: its UID for a UID SEARCH or UID SORT
// context, otherwise seq.
func (held *searchContext) number(uid imap.UID, seq imap.SeqNum) uint32 {
	if held.uidMode {
		return uint32(uid)
	}
	return uint32(seq)
}

// removalWithheld reports whether uid is still in the client's view although
// the backend view has lost it: a command or NOTIFY withholds its EXPUNGE.
func removalWithheld(selected *selectedState, uid imap.UID) bool {
	if _, visible := selected.sequence(uid); !visible || selected.backendUIDs == nil {
		return false
	}
	_, active := slices.BinarySearch(selected.backendUIDs, uid)
	return !active
}

// unsortedRun is a SEARCH context's update set, at position zero.
func unsortedRun(numbers []uint32) []contextRun {
	if len(numbers) == 0 {
		return nil
	}
	return []contextRun{{numbers: numbers}}
}

// contextRuns groups the entries of list that pick selects into runs of
// adjacent positions, in list order.
func contextRuns(list []imap.UID, pick func(imap.UID) bool, number func(imap.UID) uint32) []contextRun {
	var runs []contextRun
	for i, uid := range list {
		if !pick(uid) {
			continue
		}
		if n := len(runs); n != 0 && pick(list[i-1]) {
			runs[n-1].numbers = append(runs[n-1].numbers, number(uid))
			continue
		}
		runs = append(runs, contextRun{position: uint32(i + 1), numbers: []uint32{number(uid)}})
	}
	return runs
}

// A sorted context is updated entry by entry, at explicit positions. An
// expunged entry leaves before the EXPUNGE, in the numbers the client holds;
// every other entry enters or leaves after the mailbox notifications, in
// current numbers. Removal runs go highest position first and insertion runs
// lowest first, so each pair stays valid as a client processes them in order
// (RFC 5267 sections 4.3.3 and 4.3.4).
//
// An entry whose change waits keeps the membership the client has, and so
// does one whose removal is withheld from the client; every other entry
// changes regardless. When such a kept entry is missing from the result, a
// second Sort over the target's UIDs places it, since sort keys never change.
// That cannot place a message the backend has already removed. It keeps its
// place between its neighbours, and an entry that would enter between those
// neighbours waits until it leaves: the context must stay in the requested
// order (RFC 5267 section 4.3.2), and its order against the removed message is
// no longer known. Once a comparator has been selected since the backend last
// ordered the whole list, the removed message's place shows nothing about the
// new order, so nothing enters until it leaves. A reorder replaces the list
// from position one.
func refreshSortContext(ctx context.Context, c *conn, held *searchContext, removed, before map[imap.UID]imap.SeqNum) (searchContextDelta, error) {
	selected := c.state.selected
	sorter, ok := selected.mailbox.(SortMailbox)
	if !ok {
		return searchContextDelta{}, fmt.Errorf("SORT is no longer available")
	}
	expunged := func(uid imap.UID) bool {
		_, ok := removed[uid]
		return ok
	}
	delta := searchContextDelta{context: held}
	delta.expunged = contextRuns(held.ordered, expunged, func(uid imap.UID) uint32 {
		return held.number(uid, removed[uid])
	})
	slices.Reverse(delta.expunged)
	remaining := slices.DeleteFunc(slices.Clone(held.ordered), expunged)
	held.ordered = remaining
	held.uids = make(map[imap.UID]struct{}, len(remaining))
	for _, uid := range remaining {
		held.uids[uid] = struct{}{}
	}
	uids, err := sortContext(ctx, sorter, held, held.query)
	if err != nil {
		return searchContextDelta{}, err
	}
	waits := contextChangeWaits(selected, removed, before)
	target := make(map[imap.UID]struct{}, len(uids))
	result := make([]imap.UID, 0, len(uids))
	for _, uid := range uids {
		if _, visible := selected.sequence(uid); !visible {
			continue
		}
		if _, duplicate := target[uid]; duplicate {
			continue
		}
		if _, had := held.uids[uid]; !had && waits(uid) {
			continue
		}
		target[uid] = struct{}{}
		result = append(result, uid)
	}
	kept := false
	for _, uid := range remaining {
		if _, in := target[uid]; !in && (removalWithheld(selected, uid) || waits(uid)) {
			target[uid] = struct{}{}
			kept = true
		}
	}
	ordered, settled := result, true
	if kept {
		if ordered, settled, err = orderKeptEntries(ctx, c, sorter, held, target); err != nil {
			return searchContextDelta{}, err
		}
	}
	delta.gone, delta.added = sortedContextChanges(selected, held, remaining, ordered)
	if settled {
		held.comparatorSelections = c.comparatorSelections
	}
	held.ordered = ordered
	held.uids = make(map[imap.UID]struct{}, len(ordered))
	for _, uid := range ordered {
		held.uids[uid] = struct{}{}
	}
	return delta, nil
}

// sortContext is one refresh Sort under the pass's deadline.
func sortContext(ctx context.Context, sorter SortMailbox, held *searchContext, query *SearchQuery) ([]imap.UID, error) {
	uids, err := sorter.Sort(ctx, query, held.keys, &SortOptions{Charset: held.charset})
	if err == nil {
		err = ctx.Err() // a late success is a miss; see refreshSearchContexts
	}
	if err == nil && len(uids) > maxCommandSearchResults {
		err = fmt.Errorf("SORT result limit exceeded")
	}
	return uids, err
}

// orderKeptEntries orders target, which includes entries the context keeps
// although the result no longer holds them. settled reports that the backend
// placed every entry. See refreshSortContext.
func orderKeptEntries(ctx context.Context, c *conn, sorter SortMailbox, held *searchContext, target map[imap.UID]struct{}) (ordered []imap.UID, settled bool, err error) {
	var set imap.UIDSet
	for uid := range target {
		set.AddNum(uid)
	}
	uids, err := sortContext(ctx, sorter, held, newSearchQuery(imap.SearchUID{Set: set.Normalized()}, c.state.selected.uids, nil))
	if err != nil {
		return nil, false, err
	}
	at := make(map[imap.UID]int, len(uids))
	var sorted []imap.UID
	for _, uid := range uids {
		if _, in := target[uid]; !in {
			continue
		}
		if _, duplicate := at[uid]; duplicate {
			continue
		}
		at[uid] = len(sorted)
		sorted = append(sorted, uid)
	}
	keep := slices.DeleteFunc(slices.Clone(held.ordered), func(uid imap.UID) bool {
		_, in := target[uid]
		return !in
	})
	placed := slices.DeleteFunc(slices.Clone(keep), func(uid imap.UID) bool {
		_, ok := at[uid]
		return !ok
	})
	if len(placed) == len(keep) {
		return sorted, true, nil
	}
	if held.comparatorSelections != c.comparatorSelections || !slices.Equal(placed, slices.DeleteFunc(slices.Clone(sorted), func(uid imap.UID) bool {
		_, had := held.uids[uid]
		return !had
	})) {
		// A comparator was selected since the backend last ordered the whole
		// list, so the removed message's place no longer shows where an
		// entry belongs. Nothing enters until it leaves.
		return keep, false, nil
	}
	// after holds each run of unplaceable entries under the entry before it,
	// zero standing for the start; gaps are the stretches of the backend's
	// order between such a run's neighbours.
	after := make(map[imap.UID][]imap.UID)
	var gaps [][2]int
	var anchor imap.UID
	from, open := -1, false
	for _, uid := range keep {
		position, placeable := at[uid]
		if !placeable {
			after[anchor] = append(after[anchor], uid)
			open = true
			continue
		}
		if open {
			gaps = append(gaps, [2]int{from, position})
			open = false
		}
		anchor, from = uid, position
	}
	if open {
		gaps = append(gaps, [2]int{from, len(sorted)})
	}
	ordered = append(make([]imap.UID, 0, len(target)), after[0]...)
	for _, uid := range sorted {
		if _, had := held.uids[uid]; !had && slices.ContainsFunc(gaps, func(gap [2]int) bool {
			return at[uid] > gap[0] && at[uid] < gap[1]
		}) {
			continue // its place against the removed neighbour is unknown
		}
		ordered = append(ordered, uid)
		ordered = append(ordered, after[uid]...)
	}
	return ordered, false, nil
}

// sortedContextChanges is the positioned delta from the client's list to
// ordered, in current numbers. Entries both lists hold keep their relative
// order, since sort keys never change; if the backend reorders them anyway,
// as after a COMPARATOR change, the delta replaces the list from position one.
func sortedContextChanges(selected *selectedState, held *searchContext, from, to []imap.UID) (gone, added []contextRun) {
	inFrom := make(map[imap.UID]struct{}, len(from))
	for _, uid := range from {
		inFrom[uid] = struct{}{}
	}
	inTo := make(map[imap.UID]struct{}, len(to))
	for _, uid := range to {
		inTo[uid] = struct{}{}
	}
	leaves := func(uid imap.UID) bool {
		_, in := inTo[uid]
		return !in
	}
	enters := func(uid imap.UID) bool {
		_, in := inFrom[uid]
		return !in
	}
	number := func(uid imap.UID) uint32 {
		seq, _ := selected.sequence(uid)
		return held.number(uid, seq)
	}
	if !slices.Equal(slices.DeleteFunc(slices.Clone(from), leaves), slices.DeleteFunc(slices.Clone(to), enters)) {
		everything := func(imap.UID) bool { return true }
		return contextRuns(from, everything, number), contextRuns(to, everything, number)
	}
	gone = contextRuns(from, leaves, number)
	slices.Reverse(gone)
	return gone, contextRuns(to, enters, number)
}

// NOUPDATE's argument is a quoted tag. Write that typed production directly:
// a valid tag can contain ']', which generic response-code arguments prohibit.
func writeNoUpdate(c *conn, tag, text string) {
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("NO").SP().
		Special('[').Atom(string(imap.CodeNoUpdate)).SP().Quoted(tag).Special(']').SP().
		RespText(imapwire.RespText{Text: text}).CRLF()
}

// The expunged phase precedes the mailbox notifications, so its REMOVEFROM
// comes before EXPUNGE renumbering; the other phase follows them, so its
// REMOVEFROM and ADDTO come after the causing FETCH or EXISTS. Separate
// responses are legal and keep both number spaces coherent.
func writeSearchContextDeltas(c *conn, deltas []searchContextDelta, expunged bool) error {
	for _, delta := range deltas {
		if expunged {
			writeContextUpdate(c, delta.context, "REMOVEFROM", delta.expunged)
			continue
		}
		writeContextUpdate(c, delta.context, "REMOVEFROM", delta.gone)
		writeContextUpdate(c, delta.context, "ADDTO", delta.added)
	}
	return c.encoder.Flush()
}

func writeContextUpdate(c *conn, held *searchContext, key string, runs []contextRun) {
	if len(runs) == 0 {
		return
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("ESEARCH").SP().Special('(').Atom("TAG").SP().Quoted(held.tag)
	if held.mailbox != "" {
		c.encoder.SP().Atom("MAILBOX").SP().Mailbox(held.mailbox).
			SP().Atom("UIDVALIDITY").SP().Number(held.uidValidity)
	}
	c.encoder.Special(')')
	if held.uidMode {
		c.encoder.SP().Atom("UID")
	}
	c.encoder.SP().Atom(key).SP().Special('(')
	for i, run := range runs {
		if i > 0 {
			c.encoder.SP()
		}
		set := numberSetString(run.numbers)
		if held.sorted {
			set = numberListString(run.numbers)
		}
		c.encoder.Number(run.position).SP().Atom(set)
	}
	c.encoder.Special(')').CRLF()
}

// searchContextTag reports the tag a SEARCH should register under, and whether
// it asked to register at all.
func searchContextTag(command *queuedCommand, options []string) (string, bool) {
	for _, option := range options {
		if option == searchReturnUpd {
			return command.tag, true
		}
	}
	return "", false
}

// validateContextReturnOptions accepts CONTEXT's two SEARCH return options.
func validateContextReturnOptions(c *conn, option string) error {
	switch option {
	case searchReturnUpd, searchReturnCtx:
		return requireCapability(c, "CONTEXT=SEARCH")
	default:
		return fmt.Errorf("unsupported SEARCH return option %q", strings.ToUpper(option))
	}
}
