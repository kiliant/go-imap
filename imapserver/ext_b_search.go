package imapserver

import (
	"context"
	"fmt"
	"math"

	"github.com/kiliant/go-imap"
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
	endpoints := false
	if args.extended || c.state.revision == revisionIMAP4rev2 {
		requested := searchReturnSet(args)
		if (requested[searchReturnMin] || requested[searchReturnMax]) &&
			!requested[searchReturnAll] && !requested[searchReturnCount] && !requested[searchReturnPartial] {
			selected = nil
			if requested[searchReturnMin] {
				selected = append(selected, uids[0])
			}
			if requested[searchReturnMax] {
				selected = append(selected, uids[len(uids)-1])
			}
			endpoints = true
		}
	}
	if !endpoints && len(uids) == len(result.UIDs) && result.ModSeq != 0 {
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
		if data == nil || len(data.Items["UID"]) != 1 || len(data.Items["MODSEQ"]) != 1 {
			return fmt.Errorf("imapserver: SEARCH clock FETCH requires UID and MODSEQ")
		}
		uid, uidOK := data.Items["UID"][0].(imap.FetchDataUID)
		clock, clockOK := data.Items["MODSEQ"][0].(imap.FetchDataModSeq)
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
	if len(seen) != len(wanted) {
		return 0, fmt.Errorf("imapserver: SEARCH result changed before reading modification sequences")
	}
	return highest, nil
}
