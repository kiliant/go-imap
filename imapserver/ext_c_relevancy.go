package imapserver

import (
	"context"
	"fmt"
	"slices"

	"github.com/kiliant/go-imap"
)

// The RELEVANCY return option of SEARCH=FUZZY (RFC 6203 sections 4 and 6).
//
// RELEVANCY reports how relevant each match of a FUZZY search is, as a score
// from 1 to 100. The scores are the backend's: it supplies them in
// SearchResult.Relevancy or MultiSearchMailboxResult.Relevancy when
// ReturnRelevancy asks for them. SortMailbox returns only an order, so SORT
// RETURN (RELEVANCY) takes its scores from a Search of the same query.
//
// A backend that supplies no scores still gets a valid response, in which
// every match scores 100. RFC 6203 makes scoring a SHOULD, and a server that
// advertises SEARCH=FUZZY with ESEARCH or ESORT has offered the option, so
// refusing it would be the same failure as not implementing it. Equal scores
// rank no match above another, which is all the framework can claim for a
// backend that did not score them.
//
// The score list covers the messages the response returns, as MODSEQ does
// (RFC 4731 section 3.2): only the MIN, MAX and PARTIAL messages when those
// are requested without ALL or COUNT, and every match otherwise. Scores are in
// result order: ascending for SEARCH and MULTISEARCH, the sorted order for
// SORT. That is the order of ALL in RFC 6203's examples, so a client that
// asked for both can pair them.

// searchReturnRelevancy is the RELEVANCY return option and data item.
const searchReturnRelevancy = "RELEVANCY"

// relevancyFallback is the score of a match the backend did not score.
const relevancyFallback = 100

// validateRelevancyOption checks that SEARCH=FUZZY is advertised together with
// returnCapability, the extension that gives the command its RETURN clause:
// ESEARCH for SEARCH and MULTISEARCH, ESORT for SORT.
func validateRelevancyOption(c *conn, returnCapability string) error {
	advertised := advertisedCapabilities(c)
	if !advertised["SEARCH=FUZZY"] {
		return fmt.Errorf("return option RELEVANCY requires SEARCH=FUZZY")
	}
	if !advertised[returnCapability] {
		return fmt.Errorf("return option RELEVANCY requires %s", returnCapability)
	}
	return nil
}

// requireRelevancyFuzzy enforces RFC 6203 section 4: the RELEVANCY return
// option MUST NOT be used without a FUZZY search key. It runs on the criteria
// after FILTER substitution, as SORT RELEVANCY's check does.
func requireRelevancyFuzzy(returnOptions []string, criteria imap.SearchCriteria) error {
	if !slices.Contains(returnOptions, searchReturnRelevancy) || searchHasFuzzy(criteria) {
		return nil
	}
	return fmt.Errorf("return option RELEVANCY requires a FUZZY search key")
}

// pairRelevancy maps a backend's scores to the UIDs they pair with by
// position. No scores, nil or empty, yield a nil map, for which every match
// takes the fallback. Where a UID repeats, its first score is kept.
func pairRelevancy(uids []imap.UID, scores []uint8, command string) (map[imap.UID]uint8, error) {
	if len(scores) == 0 {
		return nil, nil
	}
	if len(scores) != len(uids) {
		return nil, fmt.Errorf("imapserver: backend %s returned %d relevancy scores for %d UIDs", command, len(scores), len(uids))
	}
	paired := make(map[imap.UID]uint8, len(uids))
	for i, uid := range uids {
		score := scores[i]
		if score < 1 || score > 100 {
			return nil, fmt.Errorf("imapserver: backend %s returned relevancy score %d, outside 1 to 100", command, score)
		}
		if _, repeated := paired[uid]; !repeated {
			paired[uid] = score
		}
	}
	return paired, nil
}

// alignRelevancy returns one score per UID, in the order of uids.
func alignRelevancy(uids []imap.UID, scores map[imap.UID]uint8) []uint8 {
	aligned := make([]uint8, len(uids))
	for i, uid := range uids {
		score, ok := scores[uid]
		if !ok {
			score = relevancyFallback
		}
		aligned[i] = score
	}
	return aligned
}

// sortRelevancy obtains scores for a SORT result through Search, since
// SortMailbox returns only an order. A sorted UID that the Search did not
// return takes the fallback.
func sortRelevancy(ctx context.Context, c *conn, query *SearchQuery, charset string, uids []imap.UID) ([]uint8, error) {
	result, err := c.state.selected.mailbox.Search(ctx, query, &SearchOptions{Charset: charset, ReturnRelevancy: true})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("imapserver: backend SEARCH returned nil")
	}
	scores, err := pairRelevancy(result.UIDs, result.Relevancy, "SEARCH")
	if err != nil {
		return nil, err
	}
	return alignRelevancy(uids, scores), nil
}

// relevancyPositions selects the result positions the score list covers, in
// result order. requested is the resolved RETURN set and n the result size.
func relevancyPositions(requested map[string]bool, partial *searchPartialRange, n int) []uint32 {
	positions := make([]uint32, n)
	for i := range positions {
		positions[i] = uint32(i)
	}
	if !requested[searchReturnMin] && !requested[searchReturnMax] && !requested[searchReturnPartial] ||
		requested[searchReturnAll] || requested[searchReturnCount] || n == 0 {
		return positions
	}
	covered := make([]bool, n)
	if requested[searchReturnMin] {
		covered[0] = true
	}
	if requested[searchReturnMax] {
		covered[n-1] = true
	}
	if requested[searchReturnPartial] && partial != nil {
		for _, position := range partialWindow(partial, positions) {
			covered[position] = true
		}
	}
	selected := positions[:0]
	for i, ok := range covered {
		if ok {
			selected = append(selected, uint32(i))
		}
	}
	return selected
}

// writeRelevancy writes the RELEVANCY data item for the positions it covers.
// An empty list is still written: score-list allows it, and it confirms that
// the option was honoured.
//
//	search-return-data =/ "RELEVANCY" SP score-list
//	score-list         = "(" [score *(SP score)] ")"
func writeRelevancy(c *conn, scores []uint8, positions []uint32) {
	c.encoder.SP().Atom(searchReturnRelevancy).SP().Special('(')
	for i, position := range positions {
		if i > 0 {
			c.encoder.SP()
		}
		c.encoder.Number(uint32(scores[position]))
	}
	c.encoder.Special(')')
}
