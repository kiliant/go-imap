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
	if searchNeedsCharset(criteria) && o.Charset == "" && !c.Capabilities()["UTF8=ACCEPT"] {
		sc.Command = rejectedCommand(c, name, "non-ASCII SEARCH criteria require an explicit charset")
		return sc
	}
	needsLiteral := searchNeedsLiteral(criteria)
	var continued chan struct{}
	var clear func()
	var caps map[string]bool
	if needsLiteral {
		c.literalMu.Lock()
		defer c.literalMu.Unlock()
		continued = make(chan struct{})
		clear = c.setContinuation(func(string) error { continued <- struct{}{}; return nil })
		caps = c.Capabilities()
	}
	untaggedCount := 0
	sc.Command = c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) {
		if needsLiteral {
			defer clear()
			defer enc.SetWaitContinuation(nil)
			enc.SetLiteralPlus(caps["LITERAL+"])
			enc.SetLiteralMinus(caps["LITERAL-"])
			enc.SetWaitContinuation(func() error { <-continued; return nil })
		}
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
	if clear != nil && sc.Command.tag == "" {
		clear()
	}
	return sc
}

func writeSearchCriteria(enc *imapwire.Encoder, criterion imap.SearchCriteria) {
	switch c := criterion.(type) {
	case imap.SearchAnd:
		for i, x := range c {
			if i > 0 {
				enc.SP()
			}
			writeSearchCriteria(enc, x)
		}
	case imap.SearchOr:
		enc.Atom("OR").SP()
		writeSearchCriteria(enc, c.Left)
		enc.SP()
		writeSearchCriteria(enc, c.Right)
	case imap.SearchNot:
		enc.Atom("NOT").SP()
		writeSearchCriteria(enc, c.Criteria)
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
		enc.Atom("MODSEQ").SP().Number64(int64(c.ModSeq))
		if c.EntryName != "" {
			enc.SP().Astring(c.EntryName).SP().Atom(string(c.EntryType))
		}
	case imap.SearchFuzzy:
		enc.Atom("FUZZY").SP()
		writeSearchCriteria(enc, c.Criteria)
	default:
		enc.Atom("")
	}
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

func searchNeedsLiteral(criterion imap.SearchCriteria) bool {
	switch c := criterion.(type) {
	case imap.SearchAnd:
		for _, x := range c {
			if searchNeedsLiteral(x) {
				return true
			}
		}
	case imap.SearchOr:
		return searchNeedsLiteral(c.Left) || searchNeedsLiteral(c.Right)
	case imap.SearchNot:
		return searchNeedsLiteral(c.Criteria)
	case imap.SearchString:
		return stringNeedsLiteral(c.Value)
	case imap.SearchHeaderField:
		return stringNeedsLiteral(c.Field) || stringNeedsLiteral(c.Value)
	case imap.SearchObjectID:
		return stringNeedsLiteral(c.Value)
	case imap.SearchModSeq:
		return stringNeedsLiteral(c.EntryName)
	}
	return false
}

func stringNeedsLiteral(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r == 0 || r == '\r' || r == '\n' || r > 127 }) >= 0
}
