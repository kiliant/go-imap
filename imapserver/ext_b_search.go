package imapserver

import (
	"context"
	"fmt"
	"math"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
)

func searchHasModSeq(criteria imap.SearchCriteria) bool {
	if _, ok := criteria.(imap.SearchModSeq); ok {
		return true
	}
	children, _ := searchCriteriaChildren(criteria)
	for _, child := range children {
		if searchHasModSeq(child) {
			return true
		}
	}
	return false
}

// RFC 7162 3.1.6 and RFC 4731 3.2 select the reported clock by the response
// shape. MIN/MAX alone describe only the endpoint messages, whose clocks can
// be read through the existing FETCH contract without extending backend APIs.
func searchResponseModSeq(ctx context.Context, c *conn, args *searchArgs, result *SearchResult, uids []imap.UID) (uint64, error) {
	selected := uids
	subset := false
	if args.extended || c.state.revision == revisionIMAP4rev2 {
		requested := searchReturnSet(args)
		// RFC 4731 3.2: the clock describes the messages being returned.
		// RETURN (UPDATE) alone (RFC 5267) returns none, so there is no clock.
		// RELEVANCY returns every match's score unless MIN, MAX or PARTIAL
		// restrict the response, so it counts as returning them.
		if !requested[searchReturnAll] && !requested[searchReturnCount] && !requested[searchReturnMin] &&
			!requested[searchReturnMax] && !requested[searchReturnPartial] && !requested[searchReturnRelevancy] {
			return 0, nil
		}
		if (requested[searchReturnMin] || requested[searchReturnMax] || requested[searchReturnPartial]) &&
			!requested[searchReturnAll] && !requested[searchReturnCount] {
			selected = nil
			if requested[searchReturnMin] {
				selected = append(selected, uids[0])
			}
			if requested[searchReturnMax] {
				selected = append(selected, uids[len(uids)-1])
			}
			if requested[searchReturnPartial] && args.partial != nil {
				numbers := make([]uint32, len(uids))
				for i, uid := range uids {
					numbers[i] = uint32(uid)
				}
				for _, uid := range partialWindow(args.partial, numbers) {
					selected = append(selected, imap.UID(uid))
				}
			}
			subset = true
		}
	}
	if len(selected) == 0 {
		return 0, nil
	}
	if !subset && len(uids) == len(result.UIDs) && result.ModSeq != 0 {
		if result.ModSeq > math.MaxInt64 {
			return 0, fmt.Errorf("imapserver: invalid SEARCH modification sequence")
		}
		return result.ModSeq, nil
	}
	// Older backends may omit SearchResult.ModSeq. A CONDSTORE backend already
	// supports UID and MODSEQ FETCH, so this is also the compatibility fallback.
	wanted := make(map[imap.UID]bool, len(selected))
	for _, uid := range selected {
		wanted[uid] = true
	}
	seen := make(map[imap.UID]bool, len(wanted))
	var highest uint64
	writer := &FetchWriter{WriteFunc: func(_ context.Context, data *imap.FetchMessageData) error {
		uid, uidOK := imapcodec.FetchValue[imap.FetchDataUID](data, "UID")
		clock, clockOK := imapcodec.FetchValue[imap.FetchDataModSeq](data, "MODSEQ")
		if !uidOK || !clockOK || !wanted[imap.UID(uid)] || seen[imap.UID(uid)] || clock == 0 || uint64(clock) > math.MaxInt64 {
			return fmt.Errorf("imapserver: invalid SEARCH clock FETCH result")
		}
		seen[imap.UID(uid)] = true
		highest = max(highest, uint64(clock))
		return nil
	}}
	if err := c.state.selected.mailbox.Fetch(ctx, writer, imap.UIDSetNum(selected...), &FetchOptions{Items: []imap.FetchItem{imap.FetchItemUID, imap.FetchItemModSeq}}); err != nil {
		return 0, err
	}
	// A message can be expunged between Search and this Fetch: the backend
	// does not hold the mailbox across the two calls, and the client's view
	// still lists the message because its removal is not yet announced. That
	// is a race, not a backend fault, so the clock covers the messages that
	// are still readable, and the backend's aggregate stands in only when
	// none are.
	if len(seen) == 0 {
		if result.ModSeq == 0 || result.ModSeq > math.MaxInt64 {
			return 0, fmt.Errorf("imapserver: SEARCH result changed before reading modification sequences")
		}
		return result.ModSeq, nil
	}
	return highest, nil
}
