package imapclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// SearchReturnOption is one entry of the RETURN list of an extended SEARCH.
// ESEARCH, RFC 4731 section 3.1; the generic form is search-return-opt in
// RFC 4466 section 2.6.
//
// It is a marker interface with an unexported method, so the set of options is
// open to this library and closed to external implementers: an extension that
// adds a parameterised option — CONTEXT (RFC 5267) adds UPDATE and PARTIAL —
// adds a type here and changes nothing that already exists.
type SearchReturnOption interface{ searchReturnOption() }

// SearchReturnOptionKeyword is a RETURN option that takes no parameters. It is
// a string-backed open type: an option this library does not model can be named
// by converting a string.
type SearchReturnOptionKeyword string

func (SearchReturnOptionKeyword) searchReturnOption() {}

// RETURN options for extended SEARCH.
const (
	// SearchReturnMin requests the lowest matching number. Omitted from the
	// response entirely when nothing matched. RFC 4731 section 3.1.
	SearchReturnMin SearchReturnOptionKeyword = "MIN"
	// SearchReturnMax requests the highest matching number. Omitted from the
	// response entirely when nothing matched. RFC 4731 section 3.1.
	SearchReturnMax SearchReturnOptionKeyword = "MAX"
	// SearchReturnAll requests every matching number as a sequence set.
	// Omitted from the response entirely when nothing matched. RFC 4731
	// section 3.1.
	SearchReturnAll SearchReturnOptionKeyword = "ALL"
	// SearchReturnCount requests the number of matches. Always present in the
	// response, including as zero. RFC 4731 section 3.1.
	SearchReturnCount SearchReturnOptionKeyword = "COUNT"
	// SearchReturnSave asks the server to store the result in the "$" marker
	// rather than, or as well as, returning it. SEARCHRES, RFC 5182
	// section 3. It requires the SEARCHRES capability and has no client-side
	// equivalent.
	SearchReturnSave SearchReturnOptionKeyword = "SAVE"
)

// ESearchReturnKey names one item of ESEARCH return data. It is an alias for
// [imap.ESearchReturnKey], which both protocol directions share.
type ESearchReturnKey = imap.ESearchReturnKey

// ESEARCH return data items this package models.
const (
	ESearchReturnKeyMin    = imap.ESearchReturnKeyMin
	ESearchReturnKeyMax    = imap.ESearchReturnKeyMax
	ESearchReturnKeyAll    = imap.ESearchReturnKeyAll
	ESearchReturnKeyCount  = imap.ESearchReturnKeyCount
	ESearchReturnKeyModSeq = imap.ESearchReturnKeyModSeq
)

// ESearchOptions configures an extended SEARCH. A nil pointer requests an
// ESEARCH response with no RETURN options, which RFC 4731 section 3.1 defines
// as equivalent to RETURN (ALL).
//
// Construct with keyed fields only; fields may be added in a future release.
type ESearchOptions struct {
	// Charset is the charset understood by the server for string criteria.
	// The client never transcodes values implicitly.
	Charset string

	// ReturnOptions is the RETURN list. An empty list still sends "RETURN ()",
	// which is what asks for an ESEARCH response rather than a SEARCH one.
	ReturnOptions []SearchReturnOption

	_ struct{}
}

// ESearchData is the result of an extended SEARCH: one ESEARCH response, or its
// client-side reconstruction.
//
// Each modelled item has a companion Has field, because RFC 4731 section 3.1
// distinguishes "absent" from "zero". MIN, MAX and ALL are omitted from the
// response entirely when nothing matched, while COUNT is always present and is
// then zero. Reading Min as 0 without checking HasMin therefore cannot tell an
// empty result from a match at message 0, which does not exist.
//
// Construct with keyed fields only; fields may be added in a future release.
type ESearchData struct {
	// Tag is the command tag the response correlated with, from the
	// search-correlator of RFC 4466 section 2.6. It is empty when the server
	// sent no correlator and when the data was reconstructed client-side.
	Tag string

	// UID reports whether every number in this response is a UID rather than
	// a sequence number. RFC 4731 section 3.1 requires an extended UID SEARCH
	// to set the UID indicator.
	UID bool

	// Min is the lowest matching number. Valid only when HasMin is set.
	Min    uint32
	HasMin bool

	// Max is the highest matching number. Valid only when HasMax is set.
	Max    uint32
	HasMax bool

	// Count is the number of matching messages. Valid only when HasCount is
	// set; a set HasCount with Count zero is a genuine empty result.
	Count    uint32
	HasCount bool

	// All holds every matching sequence number when UID is false. Valid only
	// when HasAll is set.
	All imap.SeqSet

	// AllUIDs holds every matching UID when UID is true. Valid only when
	// HasAll is set. The two address spaces are separate fields for the same
	// reason [Client.Search] and [Client.SearchUID] are separate methods:
	// conflating them silently operates on the wrong messages.
	AllUIDs imap.UIDSet

	HasAll bool

	// ModSeq is the modification sequence reported by the MODSEQ return item,
	// which a CONDSTORE server adds when the criteria mention MODSEQ.
	// RFC 4731 section 3.2. Valid only when HasModSeq is set.
	ModSeq    uint64
	HasModSeq bool

	// Values preserves every return item verbatim, keyed by the spelling the
	// server used, upper-cased. Items this package does not model are kept
	// here in raw wire form rather than dropped, because a server may return
	// data for an extension this library has never heard of and silently
	// losing it is worse than not understanding it. Modelled items appear here
	// too, so a caller can always read the unparsed text.
	Values map[ESearchReturnKey]string

	// Emulated reports that the server does not advertise ESEARCH and these
	// values were computed by this client from an ordinary SEARCH response.
	// See [Client.SearchExtended].
	Emulated bool

	_ struct{}
}

// ESearchCommand is an in-flight extended SEARCH.
type ESearchCommand struct {
	*Command

	data  *ESearchData
	saved *SavedSearchResult

	// emulated collects the numbers of a plain SEARCH response when the server
	// has no ESEARCH; wanted records which RETURN items to reconstruct.
	emulated *[]uint32
	wanted   map[ESearchReturnKey]bool
	uid      bool
}

// Wait waits for the extended SEARCH and returns its data.
func (cmd *ESearchCommand) Wait(ctx context.Context) (*ESearchData, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil extended search command")
	}
	if err := cmd.Command.Wait(ctx); err != nil {
		return nil, err
	}
	if cmd.emulated != nil {
		cmd.reconstruct()
	}
	// The correlator is checked here rather than in the collector because the
	// command's tag is not yet assigned when the collector closure is built,
	// and reading it from the reader goroutine would be a data race. Waiting
	// costs nothing: SearchExtended refuses to run beside another SEARCH, so a
	// foreign correlator is a server error either way.
	if cmd.data.Tag != "" && cmd.data.Tag != cmd.Tag() {
		return nil, &imap.Error{
			Type: imap.ErrorTypeProtocol,
			Tag:  cmd.Tag(),
			Text: fmt.Sprintf("ESEARCH response correlates with tag %q", cmd.data.Tag),
		}
	}
	return cmd.data, nil
}

// SavedResult returns a handle to the "$" marker this command asked the server
// to set, or nil when RETURN (SAVE) was not requested. Call it only after
// [ESearchCommand.Wait] has returned without error: RFC 5182 section 2.1 leaves
// "$" unchanged after a BAD and empties it after a NO, so a handle from a failed
// command would not refer to what the caller searched for.
func (cmd *ESearchCommand) SavedResult() *SavedSearchResult {
	if cmd == nil {
		return nil
	}
	return cmd.saved
}

// SearchExtended issues an extended SEARCH returning sequence numbers.
// ESEARCH, RFC 4731.
//
// # Fallback when ESEARCH is absent
//
// When the server advertises neither ESEARCH nor an enabled IMAP4rev2, this
// issues an ordinary SEARCH and computes MIN, MAX, ALL and COUNT from the full
// result client-side, marking the returned data [ESearchData.Emulated]. The
// values are identical; what is lost is the point of the extension. A server
// answering RETURN (MIN) can stop at the first match, whereas the emulation
// transfers every matching number over the wire and then discards all but one.
// On a large mailbox that is the difference between a few octets and a few
// hundred kilobytes.
//
// RETURN (SAVE) has no emulation: "$" is server-side state that no client-side
// computation can create. Requesting it without SEARCHRES returns an
// [imap.Error] wrapping [ErrCapabilityNotAdvertised] and writes nothing.
//
// The OLDER and YOUNGER criteria of WITHIN (RFC 5032) are likewise gated: a
// server that has not advertised WITHIN answers BAD, and this client refuses
// before the command reaches the wire. There is no faithful fallback — BEFORE
// and SINCE compare whole dates in the server's timezone, so rewriting "younger
// than 3600 seconds" as "since today" changes which messages match.
func (c *Client) SearchExtended(criteria imap.SearchCriteria, options *ESearchOptions) *ESearchCommand {
	return c.searchExtended(false, criteria, options)
}

// SearchExtendedUID issues an extended UID SEARCH returning UIDs. See
// [Client.SearchExtended] for the fallback behaviour.
func (c *Client) SearchExtendedUID(criteria imap.SearchCriteria, options *ESearchOptions) *ESearchCommand {
	return c.searchExtended(true, criteria, options)
}

func (c *Client) searchExtended(uid bool, criteria imap.SearchCriteria, options *ESearchOptions) *ESearchCommand {
	name := "SEARCH"
	if uid {
		name = "UID SEARCH"
	}
	cmd := &ESearchCommand{data: &ESearchData{UID: uid, Values: make(map[ESearchReturnKey]string)}, uid: uid}
	if criteria == nil {
		cmd.Command = rejectedCommand(c, name, "SEARCH requires criteria")
		return cmd
	}
	o := ESearchOptions{}
	if options != nil {
		o = *options
	}
	keywords, save, err := searchReturnKeywords(o.ReturnOptions)
	if err != nil {
		cmd.Command = rejectedCommand(c, name, err.Error())
		return cmd
	}
	if err := validateSearchCriteria(criteria); err != nil {
		cmd.Command = rejectedCommand(c, name, err.Error())
		return cmd
	}
	if searchNeedsCharset(criteria) && o.Charset == "" && !c.utf8AcceptEnabled() {
		cmd.Command = rejectedCommand(c, name, "non-ASCII SEARCH criteria require an explicit charset until UTF8=ACCEPT is enabled")
		return cmd
	}
	if criteriaUseWithin(criteria) && !c.supportsAny("WITHIN") {
		cmd.Command = failedCommand(name, capabilityError("the OLDER and YOUNGER search keys", "WITHIN"))
		return cmd
	}
	if save && !c.supportsAny("SEARCHRES") {
		cmd.Command = failedCommand(name, capabilityError("SEARCH RETURN (SAVE)", "SEARCHRES"))
		return cmd
	}
	if save {
		cmd.saved = c.newSavedSearchResult(uid)
	}
	if c.searchPending() {
		cmd.Command = rejectedCommand(c, name, "an extended SEARCH cannot be pipelined with another pending SEARCH on the same connection")
		return cmd
	}

	// RFC 9051 section 6.4.4 makes ESEARCH the base rev2 behaviour, so an
	// enabled rev2 session has it whether or not the capability was listed.
	if !c.supportsAny("ESEARCH") && !c.rev2Enabled() {
		c.searchExtendedEmulated(cmd, name, criteria, o, keywords)
		return cmd
	}

	cmd.Command = c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) {
		// RFC 4466 section 2.6: search = "SEARCH" [search-return-opts] SP
		// search-program, and the CHARSET belongs to search-program. RETURN
		// therefore precedes CHARSET, not the other way round.
		enc.SP().Atom("RETURN").SP().List(len(keywords), func(i int) { enc.Atom(keywords[i]) })
		if o.Charset != "" {
			enc.SP().Atom("CHARSET").SP().Astring(o.Charset)
		}
		enc.SP()
		writeSearchCriteria(enc, criteria)
	}, esearchCollector(cmd))
	return cmd
}

// searchExtendedEmulated issues a plain SEARCH and arranges for the requested
// RETURN items to be computed from its result.
func (c *Client) searchExtendedEmulated(cmd *ESearchCommand, name string, criteria imap.SearchCriteria, o ESearchOptions, keywords []string) {
	cmd.data.Emulated = true
	cmd.wanted = make(map[ESearchReturnKey]bool, len(keywords))
	for _, keyword := range keywords {
		cmd.wanted[ESearchReturnKey(keyword)] = true
	}
	if len(cmd.wanted) == 0 {
		// RFC 4731 section 3.1: an empty RETURN list is equivalent to (ALL).
		cmd.wanted[ESearchReturnKeyAll] = true
	}
	numbers := make([]uint32, 0)
	cmd.emulated = &numbers
	untaggedCount := 0
	cmd.Command = c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) {
		if o.Charset != "" {
			enc.SP().Atom("CHARSET").SP().Astring(o.Charset)
		}
		enc.SP()
		writeSearchCriteria(enc, criteria)
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.name != "SEARCH" || resp.hasNum || resp.cond != nil {
			return false, nil
		}
		if err := countUntaggedResponse(&untaggedCount, c.maxUntaggedResponses(), name); err != nil {
			return true, err
		}
		for resp.dec.SP() {
			var n uint32
			if !resp.dec.ExpectNumber(&n) {
				return true, resp.dec.Err()
			}
			numbers = append(numbers, n)
		}
		if !resp.dec.ExpectCRLF() {
			return true, resp.dec.Err()
		}
		return true, nil
	})
}

// reconstruct computes the requested RETURN items from a plain SEARCH result.
func (cmd *ESearchCommand) reconstruct() {
	numbers := *cmd.emulated
	data := cmd.data
	if cmd.wanted[ESearchReturnKeyCount] {
		// RFC 4731 section 3.1: COUNT is always present, including as zero.
		data.Count, data.HasCount = uint32(len(numbers)), true
		data.Values[ESearchReturnKeyCount] = fmt.Sprint(len(numbers))
	}
	if len(numbers) == 0 {
		// MIN, MAX and ALL are omitted entirely on an empty result, so the
		// emulation must omit them too rather than reporting zeroes.
		return
	}
	min, max := numbers[0], numbers[0]
	for _, n := range numbers {
		if n < min {
			min = n
		}
		if n > max {
			max = n
		}
	}
	if cmd.wanted[ESearchReturnKeyMin] {
		data.Min, data.HasMin = min, true
		data.Values[ESearchReturnKeyMin] = fmt.Sprint(min)
	}
	if cmd.wanted[ESearchReturnKeyMax] {
		data.Max, data.HasMax = max, true
		data.Values[ESearchReturnKeyMax] = fmt.Sprint(max)
	}
	if cmd.wanted[ESearchReturnKeyAll] {
		data.HasAll = true
		if cmd.uid {
			var set imap.UIDSet
			for _, n := range numbers {
				set.AddNum(imap.UID(n))
			}
			data.AllUIDs = set.Normalized()
			data.Values[ESearchReturnKeyAll] = data.AllUIDs.String()
		} else {
			var set imap.SeqSet
			for _, n := range numbers {
				set.AddNum(imap.SeqNum(n))
			}
			data.All = set.Normalized()
			data.Values[ESearchReturnKeyAll] = data.All.String()
		}
	}
}

// searchPending reports whether any SEARCH or UID SEARCH is still awaiting its
// tagged completion.
//
// It is the guard behind the "cannot be pipelined" rejection in
// [Client.SearchExtended]. RFC 5182 example 8 shows a server answering two
// pipelined extended searches in the opposite order, which is why the ESEARCH
// response carries a correlator at all. Demultiplexing on that correlator needs
// a collector chain that can hand a response to a command other than the one
// that parsed it, and this client's chain cannot: each collector consumes from
// the shared decoder, so a collector that reads a correlator and then declines
// the response leaves the next collector mid-line. Refusing to create the
// situation is the only interpretation that cannot silently deliver one
// command's matches to another.
func (c *Client) searchPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cmd := range c.pendingQ {
		switch cmd.name {
		case "SEARCH", "UID SEARCH", "ESEARCH", "UID ESEARCH":
			// ESEARCH / UID ESEARCH are the MULTISEARCH command names (RFC 7377).
			// Their collectors claim untagged ESEARCH the same way extended SEARCH
			// does, so they must participate in this mutual exclusion.
			return true
		}
	}
	return false
}

// esearchCollector claims the ESEARCH response belonging to cmd.
func esearchCollector(cmd *ESearchCommand) commandCollector {
	seen := false
	return func(resp *untaggedResponse) (bool, error) {
		if resp.name != "ESEARCH" || resp.hasNum || resp.cond != nil {
			return false, nil
		}
		dec := resp.dec
		data := ESearchData{UID: cmd.uid, Values: make(map[ESearchReturnKey]string)}
		tokenPending := false
		if dec.SP() {
			if dec.PeekSpecial('(') {
				// search-correlator = SP "(" "TAG" SP tag-string ")".
				if !dec.ExpectSpecial('(') {
					return true, dec.Err()
				}
				var label string
				if !dec.ExpectAtom(&label) || !dec.ExpectSP() {
					return true, dec.Err()
				}
				if !strings.EqualFold(label, "TAG") {
					return true, fmt.Errorf("unexpected ESEARCH correlator %q", label)
				}
				if !dec.ExpectString(&data.Tag) || !dec.ExpectSpecial(')') {
					return true, dec.Err()
				}
				// The correlator is compared against the command tag in
				// ESearchCommand.Wait; see the comment there.
			} else {
				tokenPending = true
			}
		}
		if err := readESearchItems(dec, &data, tokenPending); err != nil {
			return true, err
		}
		if !dec.ExpectCRLF() {
			return true, dec.Err()
		}
		// RFC 4731 section 3.1 allows exactly one ESEARCH response per
		// extended SEARCH. Amalgamating several is reserved for future
		// extensions that must define how, so accepting a second silently
		// would invent semantics.
		if seen {
			return true, fmt.Errorf("more than one ESEARCH response for one extended SEARCH")
		}
		seen = true
		if data.UID != cmd.uid {
			return true, fmt.Errorf("ESEARCH response UID indicator %t does not match the command", data.UID)
		}
		*cmd.data = data
		return true, nil
	}
}

// readESearchItems reads the optional UID indicator followed by the
// search-return-data items. tokenPending reports that the caller has already
// consumed the space introducing the first token.
func readESearchItems(dec *imapwire.Decoder, data *ESearchData, tokenPending bool) error {
	for {
		if !tokenPending && !dec.SP() {
			return nil
		}
		tokenPending = false
		var label string
		if !dec.ExpectAtom(&label) {
			return dec.Err()
		}
		key := ESearchReturnKey(strings.ToUpper(label))
		if key == "UID" {
			// The UID indicator is a bare atom with no value.
			data.UID = true
			continue
		}
		if !dec.ExpectSP() {
			return dec.Err()
		}
		var raw []byte
		if err := dec.CaptureValue(&raw); err != nil {
			return err
		}
		value := string(raw)
		data.Values[key] = value
		if err := applyESearchItem(data, key, value); err != nil {
			return err
		}
	}
}

// applyESearchItem fills the typed field for an item this package models. An
// item it does not model has already been preserved in ESearchData.Values, so
// falling through here is not data loss.
func applyESearchItem(data *ESearchData, key ESearchReturnKey, value string) error {
	switch key {
	case ESearchReturnKeyMin:
		n, err := responseCodeUint32(value)
		if err != nil {
			return fmt.Errorf("invalid ESEARCH MIN %q", value)
		}
		data.Min, data.HasMin = n, true
	case ESearchReturnKeyMax:
		n, err := responseCodeUint32(value)
		if err != nil {
			return fmt.Errorf("invalid ESEARCH MAX %q", value)
		}
		data.Max, data.HasMax = n, true
	case ESearchReturnKeyCount:
		n, err := responseCodeUint32(value)
		if err != nil {
			return fmt.Errorf("invalid ESEARCH COUNT %q", value)
		}
		data.Count, data.HasCount = n, true
	case ESearchReturnKeyModSeq:
		n, err := responseCodeUint64(value)
		if err != nil {
			return fmt.Errorf("invalid ESEARCH MODSEQ %q", value)
		}
		data.ModSeq, data.HasModSeq = n, true
	case ESearchReturnKeyAll:
		if data.UID {
			set, err := imap.ParseUIDSet(value)
			if err != nil {
				return fmt.Errorf("invalid ESEARCH ALL uid set %q: %w", value, err)
			}
			data.AllUIDs = set
		} else {
			set, err := imap.ParseSeqSet(value)
			if err != nil {
				return fmt.Errorf("invalid ESEARCH ALL sequence set %q: %w", value, err)
			}
			data.All = set
		}
		data.HasAll = true
	}
	return nil
}

// searchReturnKeywords renders the RETURN list and reports whether SAVE is
// among the options.
func searchReturnKeywords(options []SearchReturnOption) ([]string, bool, error) {
	keywords := make([]string, 0, len(options))
	save := false
	for _, option := range options {
		keyword, ok := option.(SearchReturnOptionKeyword)
		if !ok {
			return nil, false, fmt.Errorf("unsupported SEARCH RETURN option %T", option)
		}
		if !isListKeyword(string(keyword)) {
			return nil, false, fmt.Errorf("invalid SEARCH RETURN option %q", string(keyword))
		}
		upper := strings.ToUpper(string(keyword))
		if upper == string(SearchReturnSave) {
			save = true
		}
		keywords = append(keywords, upper)
	}
	return keywords, save, nil
}

// criteriaUseWithin reports whether criteria contain an OLDER or YOUNGER key.
// WITHIN, RFC 5032 section 3.
func criteriaUseWithin(criterion imap.SearchCriteria) bool {
	switch c := criterion.(type) {
	case imap.SearchAnd:
		for _, x := range c {
			if criteriaUseWithin(x) {
				return true
			}
		}
	case imap.SearchOr:
		return criteriaUseWithin(c.Left) || criteriaUseWithin(c.Right)
	case imap.SearchNot:
		return criteriaUseWithin(c.Criteria)
	case imap.SearchFuzzy:
		return criteriaUseWithin(c.Criteria)
	case imap.SearchWithin:
		return true
	}
	return false
}
