package imapclient

import (
	"context"
	"fmt"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// PartialRange selects a slice of SEARCH/FETCH results by 1-based index into
// the result set. It is an alias for [imap.PartialRange], which both protocol
// directions share.
type PartialRange = imap.PartialRange

func validatePartialRange(r PartialRange) error {
	first := r.FirstStart != 0 || r.FirstEnd != 0
	last := r.LastStart != 0 || r.LastEnd != 0
	switch {
	case first == last:
		return fmt.Errorf("PARTIAL range must set either First* or Last*, not both or neither")
	case first && (r.FirstStart == 0 || r.FirstEnd == 0):
		return fmt.Errorf("PARTIAL FirstStart and FirstEnd must both be non-zero")
	case last && (r.LastStart == 0 || r.LastEnd == 0):
		return fmt.Errorf("PARTIAL LastStart and LastEnd must both be non-zero")
	}
	return nil
}

// SearchReturnPartial is the PARTIAL SEARCH return option. PARTIAL, RFC 9394.
//
// Prefer [Client.SearchPartial] / [Client.SearchPartialUID]: T08's
// SearchExtended encoder currently accepts only keyword RETURN options, so
// placing this value in [ESearchOptions.ReturnOptions] is rejected until that
// helper learns to encode parameterised options. The type exists so the set
// of SearchReturnOption values stays open.
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchReturnPartial struct {
	Range PartialRange
	_     struct{}
}

func (SearchReturnPartial) searchReturnOption() {}

// ESearchReturnKeyPartial is the PARTIAL return-data item. RFC 9394 section 3.1.
const ESearchReturnKeyPartial = imap.ESearchReturnKeyPartial

// PartialSearchData is the PARTIAL item of an ESEARCH response. It is an alias
// for [imap.PartialSearchData], which both protocol directions share.
type PartialSearchData = imap.PartialSearchData

// PartialSearchOptions configures a PARTIAL SEARCH. A nil pointer is invalid
// for [Client.SearchPartial] / [Client.SearchPartialUID] — the range is
// mandatory. Companion RETURN options (MIN, MAX, COUNT, SAVE, RELEVANCY, …)
// go in ReturnOptions; ALL is rejected (RFC 9394 forbids ALL with PARTIAL).
// Do not place [SearchReturnPartial] in ReturnOptions — Range is encoded as
// the PARTIAL return option.
//
// Construct with keyed fields only; fields may be added in a future release.
type PartialSearchOptions struct {
	// Range selects which page of matches to return. Mandatory.
	Range PartialRange

	// Charset is the charset understood by the server for string criteria.
	Charset string

	// ReturnOptions requests companion RETURN items alongside PARTIAL
	// (MIN/MAX/COUNT/SAVE/RELEVANCY/…). ALL is forbidden with PARTIAL.
	ReturnOptions []SearchReturnOption

	_ struct{}
}

// SearchPartial issues SEARCH RETURN (PARTIAL …) and returns the page.
// PARTIAL, RFC 9394 section 3.1.
//
// Requires the PARTIAL capability (or CONTEXT=SEARCH for the older form).
// There is no faithful client-side fallback that preserves server-side result
// ordering and the NIL-beyond-end semantics without transferring the full set.
//
// A nil options pointer is invalid — Range is mandatory. The returned
// [SavedSearchResult] is non-nil only when SearchReturnSave was among
// [PartialSearchOptions.ReturnOptions] and the command completed; see
// [ESearchCommand.SavedResult].
func (c *Client) SearchPartial(ctx context.Context, criteria imap.SearchCriteria, options *PartialSearchOptions) (*ESearchData, *PartialSearchData, *SavedSearchResult, error) {
	return c.searchPartial(ctx, false, criteria, options)
}

// SearchPartialUID issues UID SEARCH RETURN (PARTIAL …). See [Client.SearchPartial].
func (c *Client) SearchPartialUID(ctx context.Context, criteria imap.SearchCriteria, options *PartialSearchOptions) (*ESearchData, *PartialSearchData, *SavedSearchResult, error) {
	return c.searchPartial(ctx, true, criteria, options)
}

func (c *Client) searchPartial(ctx context.Context, uid bool, criteria imap.SearchCriteria, options *PartialSearchOptions) (*ESearchData, *PartialSearchData, *SavedSearchResult, error) {
	name := "SEARCH"
	if uid {
		name = "UID SEARCH"
	}
	if ctx == nil {
		return nil, nil, nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: name + " requires a non-nil context"}
	}
	if options == nil {
		return nil, nil, nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: name + " PARTIAL requires options"}
	}
	if err := validatePartialRange(options.Range); err != nil {
		return nil, nil, nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: err.Error()}
	}
	if criteria == nil {
		return nil, nil, nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "SEARCH requires criteria"}
	}
	if err := validateSearchCriteria(criteria); err != nil {
		return nil, nil, nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: err.Error()}
	}
	if !c.Supports("PARTIAL") && !c.Supports("CONTEXT=SEARCH") {
		return nil, nil, nil, capabilityError("SEARCH RETURN (PARTIAL)", "PARTIAL")
	}
	if searchNeedsCharset(criteria) && options.Charset == "" && !c.utf8AcceptEnabled() {
		return nil, nil, nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "non-ASCII SEARCH criteria require an explicit charset until UTF8=ACCEPT is enabled"}
	}
	returnOpts, save, err := partialSearchReturnKeywords(options.ReturnOptions)
	if err != nil {
		return nil, nil, nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: err.Error()}
	}
	if save && !c.supportsAny("SEARCHRES") {
		return nil, nil, nil, capabilityError("SEARCH RETURN (SAVE)", "SEARCHRES")
	}
	if c.searchPending() {
		return nil, nil, nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "an extended SEARCH cannot be pipelined with another pending SEARCH on the same connection"}
	}

	rng := options.Range
	charset := options.Charset
	cmd := &ESearchCommand{data: &ESearchData{ESearchData: imap.ESearchData{UID: uid, Values: make(map[ESearchReturnKey]string)}}, uid: uid}
	if save {
		cmd.saved = c.newSavedSearchResult(uid)
	}
	cmd.Command = c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Atom("RETURN").SP().Special('(').Atom("PARTIAL").SP()
		writePartialRange(enc, rng)
		for _, tok := range returnOpts {
			enc.SP().Atom(tok)
		}
		enc.Special(')')
		if charset != "" {
			enc.SP().Atom("CHARSET").SP().Astring(charset)
		}
		enc.SP()
		writeSearchCriteria(enc, criteria)
	}, esearchCollector(cmd))
	data, err := cmd.Wait(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	partial, err := data.Partial()
	if err != nil {
		return data, nil, nil, err
	}
	return data, partial, cmd.SavedResult(), nil
}

// partialSearchReturnKeywords renders companion RETURN options for PARTIAL
// SEARCH. ALL is forbidden (RFC 9394); PARTIAL itself is carried by Range.
func partialSearchReturnKeywords(options []SearchReturnOption) ([]string, bool, error) {
	keywords, save, err := searchReturnKeywords(options)
	if err != nil {
		return nil, false, err
	}
	for _, kw := range keywords {
		if kw == string(SearchReturnAll) {
			return nil, false, fmt.Errorf("SEARCH RETURN (ALL) is forbidden with PARTIAL (RFC 9394)")
		}
	}
	return keywords, save, nil
}

func writePartialRange(enc *imapwire.Encoder, rng PartialRange) {
	if rng.LastStart != 0 {
		enc.Atom(fmt.Sprintf("-%d", rng.LastStart)).Special(':').Atom(fmt.Sprintf("-%d", rng.LastEnd))
		return
	}
	enc.Number(rng.FirstStart).Special(':').Number(rng.FirstEnd)
}

// PartialFetchOptions configures the PARTIAL FETCH modifier. A nil pointer is
// invalid for [Client.FetchPartial] — the range is mandatory.
//
// Construct with keyed fields only; fields may be added in a future release.
type PartialFetchOptions struct {
	Range PartialRange
	_     struct{}
}

// FetchPartial issues FETCH with the PARTIAL modifier. PARTIAL, RFC 9394
// section 3.3. Sequence-number FETCH with PARTIAL is allowed by the RFC's
// fetch-modifier production; prefer [Client.FetchUIDPartial] when paging a
// UID set of unknown size.
func (c *Client) FetchPartial(set imap.SeqSet, options *PartialFetchOptions, items ...imap.FetchItem) *FetchCommand {
	return c.fetchPartial("FETCH", set.String(), func(n imap.SeqNum) bool { return set.Contains(n) }, options, items)
}

// FetchUIDPartial issues UID FETCH with the PARTIAL modifier. RFC 9394
// section 3.3.
func (c *Client) FetchUIDPartial(set imap.UIDSet, options *PartialFetchOptions, items ...imap.FetchItem) *FetchCommand {
	return c.fetchPartial("UID FETCH", set.String(), func(imap.SeqNum) bool { return true }, options, items)
}

func (c *Client) fetchPartial(name, set string, matches func(imap.SeqNum) bool, options *PartialFetchOptions, items []imap.FetchItem) *FetchCommand {
	fc := &FetchCommand{responses: make(chan *imap.FetchMessageData), stop: make(chan struct{})}
	fail := func(err error) *FetchCommand {
		fc.Command = failedCommand(name, err)
		close(fc.stop)
		return fc
	}
	if options == nil {
		return fail(&imap.Error{Type: imap.ErrorTypeProtocol, Text: "FETCH PARTIAL requires options"})
	}
	if err := validatePartialRange(options.Range); err != nil {
		return fail(&imap.Error{Type: imap.ErrorTypeProtocol, Text: err.Error()})
	}
	if set == "" || len(items) == 0 {
		return fail(&imap.Error{Type: imap.ErrorTypeProtocol, Text: "FETCH requires a non-empty set and at least one item"})
	}
	if err := validateFetchItems(items); err != nil {
		return fail(&imap.Error{Type: imap.ErrorTypeProtocol, Text: err.Error()})
	}
	if !c.Supports("PARTIAL") {
		return fail(capabilityError("FETCH PARTIAL", "PARTIAL"))
	}
	rng := options.Range
	fc.Command = c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) {
		enc.SP()
		writeNumSet(enc, set)
		enc.SP().List(len(items), func(i int) { writeFetchItem(enc, items[i]) })
		enc.SP().Special('(').Atom("PARTIAL").SP()
		writePartialRange(enc, rng)
		enc.Special(')')
	}, func(resp *untaggedResponse) (bool, error) {
		if !resp.hasNum || resp.name != "FETCH" || !matches(imap.SeqNum(resp.number)) {
			return false, nil
		}
		return true, readFetchResponse(resp, fc.deliver)
	})
	go func() {
		<-fc.Command.done
		close(fc.stop)
	}()
	return fc
}
