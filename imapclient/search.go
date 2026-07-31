package imapclient

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// SearchOptions configures SEARCH. ReturnOptions is reserved for ESEARCH and
// lets that extension add typed options without introducing a second method.
// Construct with keyed fields only.
type SearchOptions struct {
	// Charset is the charset understood by the server for string criteria. The
	// client never transcodes values implicitly.
	Charset string
	// ReturnOptions is sent in the ESEARCH RETURN position when that extension
	// is enabled. Base SEARCH rejects non-empty options.
	ReturnOptions []string
	_             struct{}
}

// SearchCommand is an in-flight SEARCH or UID SEARCH command.
type SearchCommand struct {
	*Command
	numbers []uint32
}

// All waits for command completion and returns matching sequence numbers. For
// UID SEARCH, the returned numbers are UIDs; use AllUID for type safety.
func (cmd *SearchCommand) All(ctx context.Context) ([]imap.SeqNum, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil search command")
	}
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	result := make([]imap.SeqNum, len(cmd.numbers))
	for i, n := range cmd.numbers {
		result[i] = imap.SeqNum(n)
	}
	return result, nil
}

// AllUID waits for a UID SEARCH and returns UIDs. Calling it on a sequence
// SEARCH is rejected to avoid silently relabelling the address space.
func (cmd *SearchCommand) AllUID(ctx context.Context) ([]imap.UID, error) {
	if cmd == nil || cmd.Command == nil || !strings.HasPrefix(cmd.name, "UID ") {
		return nil, fmt.Errorf("imapclient: not a UID SEARCH command")
	}
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	result := make([]imap.UID, len(cmd.numbers))
	for i, n := range cmd.numbers {
		result[i] = imap.UID(n)
	}
	return result, nil
}

// Search issues SEARCH and returns sequence numbers through SearchCommand.All.
// A nil options selects the base SEARCH command with default settings.
func (c *Client) Search(criteria imap.SearchCriteria, options *SearchOptions) *SearchCommand {
	return c.search("SEARCH", criteria, options)
}

// SearchUID issues UID SEARCH and returns UIDs through SearchCommand.AllUID. A
// nil options selects the base UID SEARCH command with default settings.
func (c *Client) SearchUID(criteria imap.SearchCriteria, options *SearchOptions) *SearchCommand {
	return c.search("UID SEARCH", criteria, options)
}

func (c *Client) search(name string, criteria imap.SearchCriteria, options *SearchOptions) *SearchCommand {
	sc := &SearchCommand{}
	if criteria == nil {
		sc.Command = rejectedCommand(c, name, "SEARCH requires criteria")
		return sc
	}
	o := SearchOptions{}
	if options != nil {
		o = *options
	}
	if len(o.ReturnOptions) != 0 {
		sc.Command = rejectedCommand(c, name, "SEARCH RETURN options require ESEARCH")
		return sc
	}
	if err := validateSearchCriteria(criteria); err != nil {
		sc.Command = rejectedCommand(c, name, err.Error())
		return sc
	}
	if searchNeedsCharset(criteria) && o.Charset == "" && !c.utf8AcceptEnabled() {
		sc.Command = rejectedCommand(c, name, "non-ASCII SEARCH criteria require an explicit charset until UTF8=ACCEPT is enabled")
		return sc
	}
	untaggedCount := 0
	sc.Command = c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) {
		if o.Charset != "" {
			enc.SP().Atom("CHARSET").SP().Astring(o.Charset)
		}
		enc.SP()
		writeSearchCriteria(enc, criteria)
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.name != "SEARCH" || resp.hasNum {
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
			sc.numbers = append(sc.numbers, n)
		}
		if !resp.dec.ExpectCRLF() {
			return true, resp.dec.Err()
		}
		return true, nil
	})
	return sc
}

// writeSearchCriteria writes criterion in the top-level position of the SEARCH
// command, where the grammar accepts a bare sequence of space-separated keys.
func writeSearchCriteria(enc *imapwire.Encoder, criterion imap.SearchCriteria) {
	if and, ok := criterion.(imap.SearchAnd); ok {
		if len(and) == 0 {
			// Documented as matching every message, which is what ALL says.
			// An empty key sequence is not in the grammar at all.
			enc.Atom("ALL")
			return
		}
		for i, x := range and {
			if i > 0 {
				enc.SP()
			}
			writeSearchKey(enc, x)
		}
		return
	}
	writeSearchKey(enc, criterion)
}

// writeSearchKey writes exactly one search-key. A conjunction of several keys
// is only a single key when it is parenthesised: without the parentheses the
// server would attach the remaining keys to the enclosing OR or NOT instead,
// silently answering a different question. RFC 3501 section 9, search-key.
func writeSearchKey(enc *imapwire.Encoder, criterion imap.SearchCriteria) {
	if and, ok := criterion.(imap.SearchAnd); ok && len(and) != 1 {
		if len(and) == 0 {
			enc.Atom("ALL")
			return
		}
		enc.Special('(')
		for i, x := range and {
			if i > 0 {
				enc.SP()
			}
			writeSearchKey(enc, x)
		}
		enc.Special(')')
		return
	}
	switch c := criterion.(type) {
	case imap.SearchAnd:
		writeSearchKey(enc, c[0])
	case imap.SearchOr:
		enc.Atom("OR").SP()
		writeSearchKey(enc, c.Left)
		enc.SP()
		writeSearchKey(enc, c.Right)
	case imap.SearchNot:
		enc.Atom("NOT").SP()
		writeSearchKey(enc, c.Criteria)
	case imap.SearchKeyword:
		enc.Atom(string(c))
	case imap.SearchFlagKeyword:
		if c.Not {
			enc.Atom("UNKEYWORD")
		} else {
			enc.Atom("KEYWORD")
		}
		enc.SP().Flag(string(c.Flag))
	case imap.SearchHeaderField:
		enc.Atom("HEADER").SP().Astring(c.Field).SP().String(c.Value)
	case imap.SearchString:
		enc.Atom(string(c.Key)).SP().String(c.Value)
	case imap.SearchDate:
		enc.Atom(string(c.Key)).SP().Date(c.Date)
	case imap.SearchSize:
		enc.Atom(string(c.Key)).SP().Number64(c.Size)
	case imap.SearchSeqNum:
		enc.Atom(c.Set.String())
	case imap.SearchUID:
		enc.Atom("UID").SP().Atom(c.Set.String())
	case imap.SearchSavedResult:
		enc.Atom("$")
	case imap.SearchWithin:
		enc.Atom(string(c.Key)).SP().Number64(c.Seconds)
	case imap.SearchObjectID:
		enc.Atom(string(c.Key)).SP().Astring(c.Value)
	case imap.SearchModSeq:
		enc.Atom("MODSEQ")
		if c.EntryName != "" {
			// RFC 7162 section 3.1.5: the entry name and entry type are a pair;
			// neither is legal on its own. validateSearchCriteria rejects a
			// half-specified one before anything reaches the wire.
			enc.SP().Astring(c.EntryName).SP().Atom(string(c.EntryType))
		}
		enc.SP().Number64(int64(c.ModSeq))
	case imap.SearchFuzzy:
		enc.Atom("FUZZY").SP()
		writeSearchKey(enc, c.Criteria)
	}
}

// validateSearchCriteria rejects criteria the encoder cannot render, before a
// command tag is allocated. SearchCriteria is an open interface, so a value
// this package does not know about has to be reported rather than silently
// dropped or turned into a malformed command line.
func validateSearchCriteria(criterion imap.SearchCriteria) error {
	switch c := criterion.(type) {
	case imap.SearchAnd:
		for _, x := range c {
			if err := validateSearchCriteria(x); err != nil {
				return err
			}
		}
	case imap.SearchOr:
		if c.Left == nil || c.Right == nil {
			return fmt.Errorf("SEARCH OR requires both operands")
		}
		if err := validateSearchCriteria(c.Left); err != nil {
			return err
		}
		return validateSearchCriteria(c.Right)
	case imap.SearchNot:
		if c.Criteria == nil {
			return fmt.Errorf("SEARCH NOT requires an operand")
		}
		return validateSearchCriteria(c.Criteria)
	case imap.SearchFuzzy:
		if c.Criteria == nil {
			return fmt.Errorf("SEARCH FUZZY requires an operand")
		}
		return validateSearchCriteria(c.Criteria)
	case imap.SearchModSeq:
		if (c.EntryName == "") != (c.EntryType == "") {
			return fmt.Errorf("SEARCH MODSEQ requires an entry name and entry type together")
		}
	case imap.SearchKeyword, imap.SearchFlagKeyword, imap.SearchHeaderField,
		imap.SearchString, imap.SearchDate, imap.SearchSize, imap.SearchSeqNum,
		imap.SearchUID, imap.SearchSavedResult, imap.SearchWithin, imap.SearchObjectID:
	default:
		return fmt.Errorf("unsupported SEARCH criteria type %T", criterion)
	}
	return nil
}

// utf8AcceptEnabled reports whether UTF-8 may appear unencoded in command
// arguments. RFC 9755 makes that a property of a successful ENABLE, not of the
// advertised capability.
func (c *Client) utf8AcceptEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.enabled["UTF8=ACCEPT"]
	return ok
}

func searchNeedsCharset(criterion imap.SearchCriteria) bool {
	switch c := criterion.(type) {
	case imap.SearchAnd:
		for _, x := range c {
			if searchNeedsCharset(x) {
				return true
			}
		}
	case imap.SearchOr:
		return searchNeedsCharset(c.Left) || searchNeedsCharset(c.Right)
	case imap.SearchNot:
		return searchNeedsCharset(c.Criteria)
	case imap.SearchString:
		return !utf8.ValidString(c.Value) || strings.ContainsFunc(c.Value, func(r rune) bool { return r > 127 })
	case imap.SearchHeaderField:
		return !utf8.ValidString(c.Value) || strings.ContainsFunc(c.Value, func(r rune) bool { return r > 127 })
	}
	return false
}
