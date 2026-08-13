package imapserver

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ESEARCH (RFC 4731) and SEARCHRES (RFC 5182).
//
// Both are pure response-shape extensions over the SEARCH result the backend
// already returns, so neither declares a backend witness: any backend that can
// answer SEARCH can answer these. MIN, MAX and COUNT are derived from the full
// UID result rather than asked of the backend, which keeps SelectedMailbox.Search
// unchanged.
//
// Neither declares RequiresFramework either. That field gates a capability on an
// optional framework component that may not be compiled in; these are
// unconditionally part of the package, and claiming otherwise would put a second
// switch in front of a capability that has none.

// SEARCH return options. RFC 4731 section 3.1, RFC 5182 section 2.1.
const (
	searchReturnMin   = "MIN"
	searchReturnMax   = "MAX"
	searchReturnAll   = "ALL"
	searchReturnCount = "COUNT"
	searchReturnSave  = "SAVE"
	// searchReturnPartial is PARTIAL (RFC 9394), implemented in ext_c_partial.go.
	searchReturnPartial = "PARTIAL"
)

// searchResultMarker is the SEARCHRES reference to the last saved result.
const searchResultMarker = "$"

func init() {
	registerCapabilities(
		capabilityDescriptor{Name: "ESEARCH", States: stateMaskAuthenticated | stateMaskSelected},
		capabilityDescriptor{Name: "SEARCHRES", States: stateMaskAuthenticated | stateMaskSelected},
	)
}

// parseSearchReturnOptions reads the optional "RETURN (...)" clause that
// precedes the search criteria. An empty list is not the same as an absent one:
// RFC 4731 section 3.1 makes "RETURN ()" mean ALL, while an absent clause
// selects the original RFC 3501 response shape.
func parseSearchReturnOptions(decoder *imapwire.Decoder, args *searchArgs) error {
	if !decoder.PeekAtomEqual("RETURN") {
		return nil
	}
	var keyword string
	if !decoder.ExpectAtom(&keyword) || !decoder.ExpectSP() {
		return decoder.Err()
	}
	args.extended = true
	if err := decoder.ExpectList(func() error {
		var option string
		if !decoder.ExpectAtom(&option) {
			return decoder.Err()
		}
		option = strings.ToUpper(option)
		args.returnOptions = append(args.returnOptions, option)
		// Most return options are bare atoms. PARTIAL carries a range, which is
		// read by the group C file that owns the extension.
		if option == searchReturnPartial {
			return parseSearchPartialRange(decoder, args)
		}
		return nil
	}); err != nil {
		return err
	}
	if !decoder.ExpectSP() {
		return decoder.Err()
	}
	return nil
}

// validateSearchReturnOptions rejects options this server does not implement
// before the backend is asked to do any work.
func validateSearchReturnOptions(c *conn, args *searchArgs) error {
	if !args.extended {
		return nil
	}
	advertised := advertisedCapabilities(c)
	for _, option := range args.returnOptions {
		switch option {
		case searchReturnMin, searchReturnMax, searchReturnAll, searchReturnCount:
			if !advertised["ESEARCH"] {
				return fmt.Errorf("SEARCH return option %s requires ESEARCH", option)
			}
		case searchReturnSave:
			if !advertised["SEARCHRES"] {
				return fmt.Errorf("SEARCH return option SAVE requires SEARCHRES")
			}
		case searchReturnPartial:
			if !advertised["PARTIAL"] {
				return fmt.Errorf("SEARCH return option PARTIAL requires PARTIAL")
			}
		default:
			return fmt.Errorf("unsupported SEARCH return option %q", option)
		}
	}
	return nil
}

// writeSearchResponse renders one SEARCH result in whichever of the two shapes
// the command asked for, and applies SEARCHRES's SAVE as a side effect.
//
// uids are the matching UIDs, already sorted, deduplicated and filtered to
// messages still present in the selection. numbers are the same messages in the
// number space the response must use — UIDs for UID SEARCH, sequence numbers
// otherwise.
func writeSearchResponse(c *conn, command *queuedCommand, args *searchArgs, uids []imap.UID, numbers []uint32) error {
	if args.extended {
		saveSearchResult(c, args, uids)
		return writeESearchResponse(c, command, args, numbers)
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("SEARCH")
	for _, number := range numbers {
		c.encoder.SP().Number(number)
	}
	c.encoder.CRLF()
	return c.encoder.Flush()
}

func writeESearchResponse(c *conn, command *queuedCommand, args *searchArgs, numbers []uint32) error {
	requested := searchReturnSet(args)
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("ESEARCH").SP().
		Special('(').Atom("TAG").SP().Quoted(command.tag).Special(')')
	if commandUsesUIDs(command) {
		c.encoder.SP().Atom("UID")
	}
	// MIN, MAX and ALL describe messages, so they are omitted entirely on an
	// empty result. COUNT describes the result itself and is always reported.
	if len(numbers) > 0 {
		if requested[searchReturnMin] {
			c.encoder.SP().Atom(searchReturnMin).SP().Number(numbers[0])
		}
		if requested[searchReturnMax] {
			c.encoder.SP().Atom(searchReturnMax).SP().Number(numbers[len(numbers)-1])
		}
		if requested[searchReturnAll] {
			c.encoder.SP().Atom(searchReturnAll).SP().Atom(numberSetString(numbers))
		}
	}
	if requested[searchReturnCount] {
		c.encoder.SP().Atom(searchReturnCount).SP().Number(uint32(len(numbers)))
	}
	if requested[searchReturnPartial] {
		writeSearchPartial(c, args, numbers)
	}
	c.encoder.CRLF()
	return c.encoder.Flush()
}

// searchReturnSet resolves which data items an ESEARCH response carries.
// RFC 4731 section 3.1: an empty RETURN list means ALL, and so does a list that
// asks only for SAVE, since SAVE alone produces no response data.
func searchReturnSet(args *searchArgs) map[string]bool {
	requested := make(map[string]bool, len(args.returnOptions))
	for _, option := range args.returnOptions {
		requested[option] = true
	}
	if !requested[searchReturnMin] && !requested[searchReturnMax] &&
		!requested[searchReturnAll] && !requested[searchReturnCount] &&
		!requested[searchReturnPartial] {
		requested[searchReturnAll] = true
	}
	return requested
}

// saveSearchResult applies RETURN (SAVE). RFC 5182 section 2.1 defines the
// saved set by which other options accompany SAVE: with MIN and/or MAX alone the
// saved set is those one or two messages, and in every other case it is the
// whole result.
func saveSearchResult(c *conn, args *searchArgs, uids []imap.UID) {
	if !slices.Contains(args.returnOptions, searchReturnSave) {
		return
	}
	requested := searchReturnSet(args)
	var saved []imap.UID
	switch {
	case len(uids) == 0:
	case (requested[searchReturnMin] || requested[searchReturnMax]) &&
		!requested[searchReturnAll] && !requested[searchReturnCount]:
		if requested[searchReturnMin] {
			saved = append(saved, uids[0])
		}
		if requested[searchReturnMax] && uids[len(uids)-1] != uids[0] {
			saved = append(saved, uids[len(uids)-1])
		}
	default:
		saved = uids
	}
	c.state.selected.savedSearch = imap.UIDSetNum(saved...)
}

// expectMessageSet matches a sequence set or the SEARCHRES "$" marker.
//
// "$" is not part of the sequence-set grammar the wire decoder implements, and
// extending that grammar would let "$" through in contexts RFC 5182 does not
// define it for. Recognising it here instead keeps it to the commands that
// accept a message set.
func expectMessageSet(decoder *imapwire.Decoder, dst *string) bool {
	if decoder.Special('$') {
		*dst = searchResultMarker
		return true
	}
	return decoder.ExpectSequenceSet(dst)
}

// resolveSavedSearch answers the "$" message-set reference of RFC 5182.
//
// An unset saved result is an empty set, not an error: RFC 5182 section 2.1
// specifies that a command referencing "$" before anything was saved behaves as
// though it matched no messages.
func resolveSavedSearch(selected *selectedState) (imap.UIDSet, []imap.UID) {
	if selected == nil {
		return nil, nil
	}
	var ordered []imap.UID
	for _, uid := range selected.uids {
		if uidSetContains(selected.savedSearch, uid, 0) {
			ordered = append(ordered, uid)
		}
	}
	return imap.UIDSetNum(ordered...), ordered
}

// numberSetString renders an ascending number list as a compact IMAP sequence
// set, collapsing runs into ranges.
func numberSetString(numbers []uint32) string {
	if len(numbers) == 0 {
		return ""
	}
	var builder strings.Builder
	for i := 0; i < len(numbers); {
		end := i
		for end+1 < len(numbers) && numbers[end+1] == numbers[end]+1 {
			end++
		}
		if builder.Len() > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(strconv.FormatUint(uint64(numbers[i]), 10))
		if end > i {
			builder.WriteByte(':')
			builder.WriteString(strconv.FormatUint(uint64(numbers[end]), 10))
		}
		i = end + 1
	}
	return builder.String()
}

// advertisedCapabilities is the capability set currently visible to this
// session, as a set for membership tests.
func advertisedCapabilities(c *conn) map[string]bool {
	advertised := make(map[string]bool)
	for _, capability := range deriveCapabilities(&c.state, c.server) {
		advertised[capability] = true
	}
	return advertised
}
