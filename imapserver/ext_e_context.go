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
// The context is held by the framework rather than the backend. It already sees
// every change to the selected mailbox through the update queue, so it can
// decide whether a change affects a registered result without asking anyone.
// A backend therefore needs to know nothing about CONTEXT at all.

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
		!requested[sortReturnAll] && !requested[sortReturnCount] {
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
		case searchReturnUpd, searchReturnCtx:
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
// A second UPDATE under the same tag replaces the first, which is what lets a
// client refresh its registration without a CANCELUPDATE round trip.
func registerSearchContext(c *conn, tag string, uids []imap.UID, uidMode bool) {
	if c.state.selected == nil {
		return
	}
	held := make(map[imap.UID]struct{}, len(uids))
	for _, uid := range uids {
		held[uid] = struct{}{}
	}
	contexts := c.state.selected.contexts
	for i := range contexts {
		if contexts[i].tag == tag {
			contexts[i].uids, contexts[i].uidMode = held, uidMode
			return
		}
	}
	c.state.selected.contexts = append(contexts, &searchContext{tag: tag, uids: held, uidMode: uidMode})
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
	if err := requireCapability(c, "CONTEXT=SEARCH"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	if c.state.selected != nil {
		c.state.selected.contexts = slices.DeleteFunc(c.state.selected.contexts, func(held *searchContext) bool {
			return slices.Contains(tags, held.tag)
		})
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

// notifySearchContexts reports how a batch of selected-mailbox changes affected
// each registered search result.
//
// Only removals are reported. A message that newly *matches* a stored search
// cannot be recognised without re-running the criteria against it, which would
// mean calling the backend from the update path — the re-entrancy the design
// forbids. Reporting the removals the framework can see for certain, and not
// guessing at additions, is the honest half: RFC 5267 section 4.3 allows a
// server to send REMOVEFROM without ADDTO, and a wrong ADDTO would put a message
// in the client's result set that never matched.
func notifySearchContexts(c *conn, removed []imap.UID) error {
	selected := c.state.selected
	if selected == nil || len(selected.contexts) == 0 || len(removed) == 0 {
		return nil
	}
	for _, held := range selected.contexts {
		var gone []uint32
		for _, uid := range removed {
			if _, ok := held.uids[uid]; !ok {
				continue
			}
			delete(held.uids, uid)
			gone = append(gone, uint32(uid))
		}
		if len(gone) == 0 {
			continue
		}
		slices.Sort(gone)
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("ESEARCH").SP().
			Special('(').Atom("TAG").SP().Quoted(held.tag).Special(')')
		if held.uidMode {
			c.encoder.SP().Atom("UID")
		}
		// REMOVEFROM's argument is a position/set pair; position 0 means the
		// removal is not relative to a partial window. RFC 5267 section 4.3.
		c.encoder.SP().Atom("REMOVEFROM").SP().
			Special('(').Number(0).SP().Atom(numberSetString(gone)).Special(')').CRLF()
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	return nil
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
