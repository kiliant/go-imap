package imapclient

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// SearchOptions configures SEARCH. A nil pointer selects the defaults.
// Extended SEARCH RETURN options live on [ESearchOptions] and reach the wire
// through [Client.SearchExtended] / [Client.SearchExtendedUID].
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchOptions struct {
	// Charset is the charset understood by the server for string criteria. The
	// client never transcodes values implicitly.
	Charset string
	_       struct{}
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
	imapcodec.WriteSearchCriteria(enc, criterion)
}

// validateSearchCriteria rejects criteria the encoder cannot render, before a
// command tag is allocated. SearchCriteria is an open interface, so a value
// this package does not know about has to be reported rather than silently
// dropped or turned into a malformed command line.
func validateSearchCriteria(criterion imap.SearchCriteria) error {
	return imapcodec.ValidateSearchCriteria(criterion)
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

// searchNeedsCharset reports whether any string in the criteria is non-ASCII, so
// the command must declare a CHARSET.
//
// Containers come from imapcodec.SearchCriteriaChildren rather than a local
// switch. The local switch omitted imap.SearchFuzzy, and every Client.*Fuzzy
// entry point wraps the caller's criteria in exactly that type before this runs,
// so a fuzzy search over non-ASCII text silently skipped the CHARSET guard and
// went onto the wire undeclared.
func searchNeedsCharset(criterion imap.SearchCriteria) bool {
	return imapcodec.SearchCriteriaMentions(criterion, func(node imap.SearchCriteria) bool {
		switch c := node.(type) {
		case imap.SearchString:
			return valueNeedsCharset(c.Value)
		case imap.SearchHeaderField:
			return valueNeedsCharset(c.Value)
		}
		return false
	})
}

func valueNeedsCharset(value string) bool {
	return !utf8.ValidString(value) || strings.ContainsFunc(value, func(r rune) bool { return r > 127 })
}
