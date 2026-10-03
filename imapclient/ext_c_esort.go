package imapclient

import (
	"context"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ESortOptions configures an extended SORT. ESORT, RFC 5267 section 3.
// A nil pointer requests an ESEARCH response with no RETURN options, which
// RFC 4731 section 3.1 defines as equivalent to RETURN (ALL).
//
// Construct with keyed fields only; fields may be added in a future release.
type ESortOptions struct {
	// Charset is sent as the SORT charset argument. Empty defaults to "UTF-8".
	Charset string
	// ReturnOptions is the RETURN list: MIN, MAX, ALL and COUNT are the
	// options RFC 5267 section 3.1 defines for SORT, and a later extension's
	// options are passed through as written.
	ReturnOptions []SearchReturnOption

	_ struct{}
}

// SortExtended issues SORT with a RETURN list and returns the result as
// ESEARCH data. ESORT, RFC 5267 section 3.
//
// ALL lists the matches in the requested sort order (RFC 5267 section 3.1).
// It is returned as the server wrote it and must not be normalised by the
// caller, which would re-sort it numerically.
//
// MIN and MAX are "the lowest/highest sorted message" in the RFC's words,
// and servers read that differently: Dovecot returns the first and last
// message in sort order, Cyrus and Stalwart the numerically lowest and
// highest match. This method reports what the server sent; a caller that
// needs the ends of the sort order should take them from ALL.
//
// # Fallback when ESORT is absent
//
// When the server advertises SORT but not ESORT, this issues an ordinary SORT
// and computes MIN, MAX, ALL and COUNT from the sorted result client-side,
// marking the returned data [ESearchData.Emulated]. The emulation takes MIN
// and MAX as the first and last message in sort order. Any other RETURN option
// has no faithful emulation and returns an [imap.Error] wrapping
// [ErrCapabilityNotAdvertised] without writing anything.
//
// RETURN (SAVE) is not supported by this method.
func (c *Client) SortExtended(ctx context.Context, keys []SortKeySpec, criteria imap.SearchCriteria, options *ESortOptions) (*ESearchData, error) {
	return c.sortExtended(ctx, false, keys, criteria, options)
}

// SortExtendedUID issues UID SORT with a RETURN list. See
// [Client.SortExtended].
func (c *Client) SortExtendedUID(ctx context.Context, keys []SortKeySpec, criteria imap.SearchCriteria, options *ESortOptions) (*ESearchData, error) {
	return c.sortExtended(ctx, true, keys, criteria, options)
}

func (c *Client) sortExtended(ctx context.Context, uid bool, keys []SortKeySpec, criteria imap.SearchCriteria, options *ESortOptions) (*ESearchData, error) {
	name := "SORT"
	if uid {
		name = "UID SORT"
	}
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: name + " requires a non-nil context"}
	}
	if len(keys) == 0 {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "SORT requires at least one sort key"}
	}
	o := ESortOptions{}
	if options != nil {
		o = *options
	}
	keywords, save, err := searchReturnKeywords(o.ReturnOptions)
	if err != nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: err.Error()}
	}
	if save {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "SORT RETURN (SAVE) is not supported by SortExtended"}
	}
	if criteria == nil {
		criteria = imap.SearchAll
	}
	if sortKeysWantRelevancy(keys) {
		if _, ok := criteria.(imap.SearchFuzzy); !ok {
			// RFC 6203: RELEVANCY requires a FUZZY search key in the same command.
			criteria = imap.SearchFuzzy{Criteria: criteria}
		}
	}
	if err := validateSearchCriteria(criteria); err != nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: err.Error()}
	}
	if err := validateSortKeys(keys, c, false); err != nil {
		return nil, err
	}
	if !c.Supports("SORT") {
		return nil, capabilityError(name, "SORT")
	}
	charset := o.Charset
	if charset == "" {
		charset = "UTF-8"
	}
	if !c.Supports("ESORT") {
		return c.sortExtendedEmulated(ctx, uid, keys, criteria, charset, keywords)
	}
	if c.searchPending() {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "an extended SORT cannot be pipelined with another pending SEARCH or SORT on the same connection"}
	}

	cmd := &ESearchCommand{data: &ESearchData{ESearchData: imap.ESearchData{UID: uid, Values: make(map[ESearchReturnKey]string)}}, uid: uid}
	cmd.Command = c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) {
		// RFC 5267 section 3: sort = ["UID" SP] "SORT" [sort-return-opts]
		// SP sort-criteria SP search-criteria, so RETURN comes first.
		enc.SP().Atom("RETURN").SP().List(len(keywords), func(i int) { enc.Atom(keywords[i]) })
		enc.SP().Special('(')
		for i, key := range keys {
			if i > 0 {
				enc.SP()
			}
			if key.Reverse {
				enc.Atom("REVERSE").SP()
			}
			enc.Atom(string(key.Key))
		}
		enc.Special(')').SP().Astring(charset).SP()
		writeSearchCriteria(enc, criteria)
	}, esearchCollector(cmd))
	return cmd.Wait(ctx)
}

// sortExtendedEmulated answers MIN, MAX, ALL and COUNT from an ordinary SORT,
// in sort order. The sorted numbers are written to ALL one range each, so
// the order survives exactly as an ESORT server would send it.
func (c *Client) sortExtendedEmulated(ctx context.Context, uid bool, keys []SortKeySpec, criteria imap.SearchCriteria, charset string, keywords []string) (*ESearchData, error) {
	wanted := make(map[ESearchReturnKey]bool, len(keywords))
	for _, keyword := range keywords {
		key := ESearchReturnKey(keyword)
		switch key {
		case ESearchReturnKeyMin, ESearchReturnKeyMax, ESearchReturnKeyAll, ESearchReturnKeyCount:
			wanted[key] = true
		default:
			return nil, capabilityError("SORT RETURN ("+keyword+")", "ESORT")
		}
	}
	if len(wanted) == 0 {
		// RFC 4731 section 3.1: an empty RETURN list is equivalent to (ALL).
		wanted[ESearchReturnKeyAll] = true
	}
	sorted, err := c.sort(ctx, uid, keys, criteria, &SortOptions{Charset: charset})
	if err != nil {
		return nil, err
	}
	numbers := make([]uint32, 0, len(sorted.SeqNums)+len(sorted.UIDs))
	for _, n := range sorted.SeqNums {
		numbers = append(numbers, uint32(n))
	}
	for _, n := range sorted.UIDs {
		numbers = append(numbers, uint32(n))
	}
	data := &ESearchData{ESearchData: imap.ESearchData{UID: uid, Values: make(map[ESearchReturnKey]string)}, Emulated: true}
	if wanted[ESearchReturnKeyCount] {
		data.Count, data.HasCount = uint32(len(numbers)), true
	}
	if len(numbers) > 0 && wanted[ESearchReturnKeyMin] {
		data.Min, data.HasMin = numbers[0], true
	}
	if len(numbers) > 0 && wanted[ESearchReturnKeyMax] {
		data.Max, data.HasMax = numbers[len(numbers)-1], true
	}
	if len(numbers) > 0 && wanted[ESearchReturnKeyAll] {
		data.HasAll = true
		if uid {
			for _, n := range numbers {
				data.AllUIDs = append(data.AllUIDs, imap.NumRange[imap.UID]{Start: imap.UID(n), Stop: imap.UID(n)})
			}
		} else {
			for _, n := range numbers {
				data.All = append(data.All, imap.NumRange[imap.SeqNum]{Start: imap.SeqNum(n), Stop: imap.SeqNum(n)})
			}
		}
	}
	return data, nil
}
