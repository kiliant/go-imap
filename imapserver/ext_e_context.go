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
// UID membership survives renumbering; REMOVEFROM uses the old sequence number
// for an expunge and the current one for a flag change.
type searchContextDelta struct {
	context     *searchContext
	added, gone []uint32
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
			delta, changed, err := refreshSortContext(ctx, c, held, beforeSequences)
			if err != nil {
				writeNoUpdate(c, held.tag, "sort context could not be refreshed")
				continue
			}
			live = append(live, held)
			if changed {
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
		current := make(map[imap.UID]struct{}, len(result.UIDs))
		var added, gone []uint32
		for _, uid := range result.UIDs {
			seq, visible := selected.sequence(uid)
			if !visible {
				continue
			}
			if _, duplicate := current[uid]; duplicate {
				continue
			}
			current[uid] = struct{}{}
			if _, known := held.uids[uid]; !known {
				number := uint32(seq)
				if held.uidMode {
					number = uint32(uid)
				}
				added = append(added, number)
			}
		}
		for uid := range held.uids {
			if _, matches := current[uid]; matches {
				continue
			}
			seq, expunged := removed[uid]
			if !expunged {
				var visible bool
				seq, visible = selected.sequence(uid)
				if visible && selected.backendUIDs != nil {
					if _, active := slices.BinarySearch(selected.backendUIDs, uid); !active {
						current[uid] = struct{}{}
						continue // its EXPUNGE is still withheld
					}
				}
				if !visible {
					continue
				}
			}
			if before, ok := beforeSequences[uid]; ok {
				seq = before
			}
			number := uint32(seq)
			if held.uidMode {
				number = uint32(uid)
			}
			gone = append(gone, number)
		}
		held.uids = current
		live = append(live, held)
		if len(added) == 0 && len(gone) == 0 {
			continue
		}
		slices.Sort(added)
		slices.Sort(gone)
		deltas = append(deltas, searchContextDelta{context: held, added: added, gone: gone})
	}
	selected.contexts = live
	return deltas
}

// A full replacement is a valid ordered delta: remove the old list from
// position one, then insert the new list there. The two halves straddle the
// mailbox notifications, preserving pre-EXPUNGE and post-EXISTS numbering.
func refreshSortContext(ctx context.Context, c *conn, held *searchContext, before map[imap.UID]imap.SeqNum) (searchContextDelta, bool, error) {
	selected := c.state.selected
	for uid := range held.uids {
		if _, visible := selected.sequence(uid); visible && selected.backendUIDs != nil {
			if _, active := slices.BinarySearch(selected.backendUIDs, uid); !active {
				// A command still withholds this removal. Keep its context view
				// until the same drain can deliver the corresponding EXPUNGE.
				return searchContextDelta{}, false, nil
			}
		}
	}
	sorter, ok := selected.mailbox.(SortMailbox)
	if !ok {
		return searchContextDelta{}, false, fmt.Errorf("SORT is no longer available")
	}
	uids, err := sorter.Sort(ctx, held.query, held.keys, &SortOptions{Charset: held.charset})
	if err == nil {
		err = ctx.Err() // a late success is a miss; see refreshSearchContexts
	}
	if err != nil || len(uids) > maxCommandSearchResults {
		if err == nil {
			err = fmt.Errorf("SORT result limit exceeded")
		}
		return searchContextDelta{}, false, err
	}
	current := make(map[imap.UID]struct{}, len(uids))
	ordered := make([]imap.UID, 0, len(uids))
	for _, uid := range uids {
		if _, visible := selected.sequence(uid); !visible {
			continue
		}
		if _, duplicate := current[uid]; duplicate {
			continue
		}
		current[uid] = struct{}{}
		ordered = append(ordered, uid)
	}
	if slices.Equal(held.ordered, ordered) {
		return searchContextDelta{}, false, nil
	}
	delta := searchContextDelta{context: held}
	for _, uid := range held.ordered {
		number := uint32(uid)
		if !held.uidMode {
			number = uint32(before[uid])
		}
		delta.gone = append(delta.gone, number)
	}
	for _, uid := range ordered {
		number := uint32(uid)
		if !held.uidMode {
			seq, _ := selected.sequence(uid)
			number = uint32(seq)
		}
		delta.added = append(delta.added, number)
	}
	held.uids, held.ordered = current, ordered
	return delta, true, nil
}

// NOUPDATE's argument is a quoted tag. Write that typed production directly:
// a valid tag can contain ']', which generic response-code arguments prohibit.
func writeNoUpdate(c *conn, tag, text string) {
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("NO").SP().
		Special('[').Atom(string(imap.CodeNoUpdate)).SP().Quoted(tag).Special(']').SP().
		RespText(imapwire.RespText{Text: text}).CRLF()
}

// REMOVEFROM precedes EXPUNGE renumbering; ADDTO follows the corresponding
// EXISTS. Separate responses are legal and keep both number spaces coherent.
func writeSearchContextDeltas(c *conn, deltas []searchContextDelta, removals bool) error {
	for _, delta := range deltas {
		numbers, key := delta.added, "ADDTO"
		if removals {
			numbers, key = delta.gone, "REMOVEFROM"
		}
		if len(numbers) == 0 {
			continue
		}
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("ESEARCH").SP().Special('(').Atom("TAG").SP().Quoted(delta.context.tag)
		if delta.context.mailbox != "" {
			c.encoder.SP().Atom("MAILBOX").SP().Mailbox(delta.context.mailbox).
				SP().Atom("UIDVALIDITY").SP().Number(delta.context.uidValidity)
		}
		c.encoder.Special(')')
		if delta.context.uidMode {
			c.encoder.SP().Atom("UID")
		}
		position, set := uint32(0), numberSetString(numbers)
		if delta.context.sorted {
			position, set = 1, numberListString(numbers)
		}
		c.encoder.SP().Atom(key).SP().Special('(').Number(position).SP().Atom(set).Special(')').CRLF()
	}
	return c.encoder.Flush()
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
