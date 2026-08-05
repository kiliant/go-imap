package imapclient

import (
	"context"
	"strings"

	"github.com/kiliant/go-imap"
)

// SearchReturnRelevancy requests FUZZY relevancy scores in an ESEARCH response.
// SEARCH=FUZZY, RFC 6203 section 4. It is a keyword RETURN option, so it passes
// through [Client.SearchExtended] once SEARCH=FUZZY is advertised.
const SearchReturnRelevancy SearchReturnOptionKeyword = "RELEVANCY"

// ESearchReturnKeyRelevancy is the RELEVANCY return-data item. RFC 6203.
const ESearchReturnKeyRelevancy = imap.ESearchReturnKeyRelevancy

// SearchFuzzy issues SEARCH with a FUZZY-wrapped criterion, gated on the
// SEARCH=FUZZY capability. SEARCH=FUZZY, RFC 6203.
//
// The core [Client.Search] encoder already knows how to write
// [imap.SearchFuzzy]; this helper is the capability gate. Prefer it over
// passing SearchFuzzy to Search directly so a server that never advertised
// SEARCH=FUZZY is not sent a command that may close the connection.
func (c *Client) SearchFuzzy(criteria imap.SearchCriteria, options *SearchOptions) *SearchCommand {
	return c.searchFuzzy(false, criteria, options)
}

// SearchFuzzyUID issues UID SEARCH with a FUZZY-wrapped criterion.
func (c *Client) SearchFuzzyUID(criteria imap.SearchCriteria, options *SearchOptions) *SearchCommand {
	return c.searchFuzzy(true, criteria, options)
}

func (c *Client) searchFuzzy(uid bool, criteria imap.SearchCriteria, options *SearchOptions) *SearchCommand {
	name := "SEARCH"
	if uid {
		name = "UID SEARCH"
	}
	sc := &SearchCommand{}
	if !c.Supports("SEARCH=FUZZY") {
		sc.Command = failedCommand(name, capabilityError("SEARCH FUZZY", "SEARCH=FUZZY"))
		return sc
	}
	if criteria == nil {
		sc.Command = rejectedCommand(c, name, "SEARCH requires criteria")
		return sc
	}
	if _, ok := criteria.(imap.SearchFuzzy); !ok {
		criteria = imap.SearchFuzzy{Criteria: criteria}
	}
	if uid {
		return c.SearchUID(criteria, options)
	}
	return c.Search(criteria, options)
}

// SearchExtendedFuzzy is [Client.SearchExtended] with a FUZZY capability gate
// and an optional RELEVANCY return option.
func (c *Client) SearchExtendedFuzzy(criteria imap.SearchCriteria, options *ESearchOptions) *ESearchCommand {
	return c.searchExtendedFuzzy(false, criteria, options)
}

// SearchExtendedFuzzyUID is the UID form of [Client.SearchExtendedFuzzy].
func (c *Client) SearchExtendedFuzzyUID(criteria imap.SearchCriteria, options *ESearchOptions) *ESearchCommand {
	return c.searchExtendedFuzzy(true, criteria, options)
}

func (c *Client) searchExtendedFuzzy(uid bool, criteria imap.SearchCriteria, options *ESearchOptions) *ESearchCommand {
	name := "SEARCH"
	if uid {
		name = "UID SEARCH"
	}
	cmd := &ESearchCommand{data: &ESearchData{ESearchData: imap.ESearchData{UID: uid, Values: make(map[ESearchReturnKey]string)}}, uid: uid}
	if !c.Supports("SEARCH=FUZZY") {
		cmd.Command = failedCommand(name, capabilityError("SEARCH FUZZY", "SEARCH=FUZZY"))
		return cmd
	}
	if criteria == nil {
		cmd.Command = rejectedCommand(c, name, "SEARCH requires criteria")
		return cmd
	}
	if _, ok := criteria.(imap.SearchFuzzy); !ok {
		criteria = imap.SearchFuzzy{Criteria: criteria}
	}
	if uid {
		return c.SearchExtendedUID(criteria, options)
	}
	return c.SearchExtended(criteria, options)
}

// SortFuzzy is [Client.Sort] gated so RELEVANCY may appear among the keys.
// The SEARCH=FUZZY capability is required, and the criteria are wrapped in
// FUZZY when they are not already (RFC 6203 requires a FUZZY key whenever
// RELEVANCY is used).
func (c *Client) SortFuzzy(ctx context.Context, keys []SortKeySpec, criteria imap.SearchCriteria, options *SortOptions) (*SortData, error) {
	if !c.Supports("SEARCH=FUZZY") {
		return nil, capabilityError("SORT FUZZY/RELEVANCY", "SEARCH=FUZZY")
	}
	if criteria == nil {
		criteria = imap.SearchAll
	}
	if _, ok := criteria.(imap.SearchFuzzy); !ok {
		criteria = imap.SearchFuzzy{Criteria: criteria}
	}
	return c.Sort(ctx, keys, criteria, options)
}

// SortFuzzyUID is the UID form of [Client.SortFuzzy].
func (c *Client) SortFuzzyUID(ctx context.Context, keys []SortKeySpec, criteria imap.SearchCriteria, options *SortOptions) (*SortData, error) {
	if !c.Supports("SEARCH=FUZZY") {
		return nil, capabilityError("UID SORT FUZZY/RELEVANCY", "SEARCH=FUZZY")
	}
	if criteria == nil {
		criteria = imap.SearchAll
	}
	if _, ok := criteria.(imap.SearchFuzzy); !ok {
		criteria = imap.SearchFuzzy{Criteria: criteria}
	}
	return c.SortUID(ctx, keys, criteria, options)
}

func sortKeysWantRelevancy(keys []SortKeySpec) bool {
	for _, k := range keys {
		if strings.EqualFold(string(k.Key), string(SortKeyRelevancy)) {
			return true
		}
	}
	return false
}
