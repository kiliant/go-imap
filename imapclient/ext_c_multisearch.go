package imapclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// MultiSearchSource selects which mailboxes an ESEARCH command searches.
// MULTISEARCH, RFC 7377 section 2.2.
//
// It is a marker interface with an unexported method so the set of source
// options stays open to this library: a future RFC that adds a mailbox
// filter adds a type here and changes nothing that already exists.
type MultiSearchSource interface{ multiSearchSource() }

// MultiSearchSelected searches only the currently selected mailbox.
// It is the default when Sources is empty.
type MultiSearchSelected struct{ _ struct{} }

func (MultiSearchSelected) multiSearchSource() {}

// MultiSearchPersonal searches all mailboxes in the personal namespace.
type MultiSearchPersonal struct{ _ struct{} }

func (MultiSearchPersonal) multiSearchSource() {}

// MultiSearchSubscribed searches all subscribed mailboxes.
type MultiSearchSubscribed struct{ _ struct{} }

func (MultiSearchSubscribed) multiSearchSource() {}

// MultiSearchInboxes searches all mailboxes named "INBOX" or that have the
// \Inbox special-use attribute. RFC 7377 via RFC 5465.
type MultiSearchInboxes struct{ _ struct{} }

func (MultiSearchInboxes) multiSearchSource() {}

// MultiSearchMailboxes searches the named mailboxes. Names are not wildcards.
//
// Construct with keyed fields only; fields may be added in a future release.
type MultiSearchMailboxes struct {
	Names []string
	_     struct{}
}

func (MultiSearchMailboxes) multiSearchSource() {}

// MultiSearchSubtree searches mailbox and every selectable descendant.
// OneLevel limits the walk to one hierarchy level (subtree-one).
//
// Construct with keyed fields only; fields may be added in a future release.
type MultiSearchSubtree struct {
	Mailbox  string
	OneLevel bool
	_        struct{}
}

func (MultiSearchSubtree) multiSearchSource() {}

// MultiSearchOptions configures the MULTISEARCH ESEARCH command. A nil pointer
// selects the defaults: selected-mailbox source only, no CHARSET, and an empty
// RETURN list (treated as ALL per RFC 7377 / RFC 4731).
//
// Construct with keyed fields only; fields may be added in a future release.
type MultiSearchOptions struct {
	// Sources selects mailboxes. Empty defaults to selected-only.
	Sources []MultiSearchSource

	// Charset is the charset for string criteria. Empty omits CHARSET.
	Charset string

	// ReturnOptions is the RETURN list. Empty still sends RETURN (), which
	// RFC 7377 / RFC 4731 treat as ALL. SAVE is only legal with selected-only.
	ReturnOptions []SearchReturnOption

	_ struct{}
}

// MultiSearchResult is one ESEARCH response from a multimailbox search. It is
// an alias for [imap.MultiSearchResult], which both protocol directions share.
type MultiSearchResult = imap.MultiSearchResult

// MultiSearchData collects every per-mailbox ESEARCH response. It is an alias
// for [imap.MultiSearchData], which both protocol directions share.
type MultiSearchData = imap.MultiSearchData

// MultiSearchCommand is an in-flight multimailbox ESEARCH.
type MultiSearchCommand struct {
	*Command
	data  *MultiSearchData
	saved *SavedSearchResult
}

// Wait waits for the multimailbox ESEARCH and returns every per-mailbox result.
func (cmd *MultiSearchCommand) Wait(ctx context.Context) (*MultiSearchData, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil multisearch command")
	}
	if err := cmd.Command.Wait(ctx); err != nil {
		return nil, err
	}
	tag := cmd.Tag()
	for i := range cmd.data.Results {
		r := &cmd.data.Results[i]
		if r.Tag != "" && r.Tag != tag {
			return nil, &imap.Error{
				Type: imap.ErrorTypeProtocol,
				Tag:  tag,
				Text: fmt.Sprintf("ESEARCH response correlates with tag %q", r.Tag),
			}
		}
	}
	return cmd.data, nil
}

// SavedResult returns the "$" handle when RETURN (SAVE) was requested, or nil
// otherwise. Call only after a successful [MultiSearchCommand.Wait]; see
// [ESearchCommand.SavedResult].
func (cmd *MultiSearchCommand) SavedResult() *SavedSearchResult {
	if cmd == nil {
		return nil
	}
	return cmd.saved
}

// MultiSearch issues the ESEARCH command across one or more mailboxes.
// MULTISEARCH, RFC 7377.
//
// Unlike [Client.SearchExtended], this uses the ESEARCH command name (not
// SEARCH RETURN) and may return several ESEARCH responses, one per mailbox
// that had matches. An empty result list with a tagged OK means no mailbox
// matched. Use [MultiSearchCommand.Wait] with a context for completion.
//
// There is no atomic client-side fallback: SELECT+SEARCH per mailbox would
// disrupt the selected state that this command is designed to preserve.
func (c *Client) MultiSearch(criteria imap.SearchCriteria, options *MultiSearchOptions) *MultiSearchCommand {
	return c.multiSearch(false, criteria, options)
}

// MultiSearchUID issues UID ESEARCH. See [Client.MultiSearch].
func (c *Client) MultiSearchUID(criteria imap.SearchCriteria, options *MultiSearchOptions) *MultiSearchCommand {
	return c.multiSearch(true, criteria, options)
}

func (c *Client) multiSearch(uid bool, criteria imap.SearchCriteria, options *MultiSearchOptions) *MultiSearchCommand {
	name := "ESEARCH"
	if uid {
		name = "UID ESEARCH"
	}
	result := &MultiSearchCommand{data: &MultiSearchData{}}
	if !c.Supports("MULTISEARCH") {
		result.Command = failedCommand(name, capabilityError(name, "MULTISEARCH"))
		return result
	}
	if criteria == nil {
		result.Command = rejectedCommand(c, name, "ESEARCH requires criteria")
		return result
	}
	if err := validateSearchCriteria(criteria); err != nil {
		result.Command = rejectedCommand(c, name, err.Error())
		return result
	}
	o := MultiSearchOptions{}
	if options != nil {
		o = *options
	}
	if err := validateMultiSearchSources(o.Sources); err != nil {
		result.Command = rejectedCommand(c, name, err.Error())
		return result
	}
	if searchNeedsCharset(criteria) && o.Charset == "" && !c.utf8AcceptEnabled() {
		result.Command = rejectedCommand(c, name, "non-ASCII SEARCH criteria require an explicit charset until UTF8=ACCEPT is enabled")
		return result
	}
	keywords, save, err := searchReturnKeywords(o.ReturnOptions)
	if err != nil {
		result.Command = rejectedCommand(c, name, err.Error())
		return result
	}
	selectedOnly := multiSearchSelectedOnly(o.Sources)
	if save {
		if !selectedOnly {
			result.Command = rejectedCommand(c, name, "SEARCH RETURN (SAVE) is only valid with the selected mailbox (RFC 7377 section 2.2)")
			return result
		}
		if !c.supportsAny("SEARCHRES") {
			result.Command = failedCommand(name, capabilityError("SEARCH RETURN (SAVE)", "SEARCHRES"))
			return result
		}
		// MULTISEARCH SAVE is only legal for selected-only searches, which
		// always return UIDs (RFC 7377 section 2.1).
		result.saved = c.newSavedSearchResult(true)
	}
	needSelected := selectedOnly || len(o.Sources) == 0
	state := stateAuthenticated | stateSelected
	if needSelected {
		state = stateSelected
	}
	if c.searchPending() {
		result.Command = rejectedCommand(c, name, "an extended SEARCH cannot be pipelined with another pending SEARCH on the same connection")
		return result
	}

	sources := o.Sources
	untagged := 0
	result.Command = c.beginCommand(name, state, func(enc *imapwire.Encoder) {
		if len(sources) > 0 {
			enc.SP().Atom("IN").SP().Special('(')
			for i, src := range sources {
				if i > 0 {
					enc.SP()
				}
				writeMultiSearchSource(enc, src)
			}
			enc.Special(')')
		}
		enc.SP().Atom("RETURN").SP().List(len(keywords), func(i int) { enc.Atom(keywords[i]) })
		if o.Charset != "" {
			enc.SP().Atom("CHARSET").SP().Astring(o.Charset)
		}
		enc.SP()
		writeSearchCriteria(enc, criteria)
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.name != "ESEARCH" || resp.hasNum || resp.cond != nil {
			return false, nil
		}
		if err := countUntaggedResponse(&untagged, c.maxUntaggedResponses(), name); err != nil {
			return true, err
		}
		one, err := readMultiSearchResponse(resp.dec)
		if err != nil {
			return true, err
		}
		one.Data.UID = true // RFC 7377 section 2.1.
		result.data.Results = append(result.data.Results, one)
		return true, nil
	})
	return result
}

func validateMultiSearchSources(sources []MultiSearchSource) error {
	for i, src := range sources {
		if src == nil {
			return fmt.Errorf("MULTISEARCH source %d is nil", i)
		}
		switch s := src.(type) {
		case MultiSearchMailboxes:
			if len(s.Names) == 0 {
				return fmt.Errorf("MULTISEARCH mailboxes source requires at least one name")
			}
		case *MultiSearchMailboxes:
			if s == nil || len(s.Names) == 0 {
				return fmt.Errorf("MULTISEARCH mailboxes source requires at least one name")
			}
		case MultiSearchSubtree:
			if s.Mailbox == "" {
				return fmt.Errorf("MULTISEARCH subtree source requires a mailbox")
			}
		case *MultiSearchSubtree:
			if s == nil || s.Mailbox == "" {
				return fmt.Errorf("MULTISEARCH subtree source requires a mailbox")
			}
		case MultiSearchSelected, *MultiSearchSelected,
			MultiSearchPersonal, *MultiSearchPersonal,
			MultiSearchSubscribed, *MultiSearchSubscribed,
			MultiSearchInboxes, *MultiSearchInboxes:
		default:
			return fmt.Errorf("unsupported MULTISEARCH source %T", src)
		}
	}
	return nil
}

func multiSearchSelectedOnly(sources []MultiSearchSource) bool {
	if len(sources) == 0 {
		return true
	}
	if len(sources) != 1 {
		return false
	}
	switch sources[0].(type) {
	case MultiSearchSelected, *MultiSearchSelected:
		return true
	default:
		return false
	}
}

func writeMultiSearchSource(enc *imapwire.Encoder, src MultiSearchSource) {
	switch s := src.(type) {
	case MultiSearchSelected:
		enc.Atom("selected")
	case *MultiSearchSelected:
		enc.Atom("selected")
	case MultiSearchPersonal:
		enc.Atom("personal")
	case *MultiSearchPersonal:
		enc.Atom("personal")
	case MultiSearchSubscribed:
		enc.Atom("subscribed")
	case *MultiSearchSubscribed:
		enc.Atom("subscribed")
	case MultiSearchInboxes:
		enc.Atom("inboxes")
	case *MultiSearchInboxes:
		enc.Atom("inboxes")
	case MultiSearchMailboxes:
		enc.Atom("mailboxes")
		for _, name := range s.Names {
			enc.SP().Mailbox(name)
		}
	case *MultiSearchMailboxes:
		enc.Atom("mailboxes")
		for _, name := range s.Names {
			enc.SP().Mailbox(name)
		}
	case MultiSearchSubtree:
		if s.OneLevel {
			enc.Atom("subtree-one")
		} else {
			enc.Atom("subtree")
		}
		enc.SP().Mailbox(s.Mailbox)
	case *MultiSearchSubtree:
		if s.OneLevel {
			enc.Atom("subtree-one")
		} else {
			enc.Atom("subtree")
		}
		enc.SP().Mailbox(s.Mailbox)
	default:
		enc.Atom("selected") // unreachable after validation; keeps encoder moving
	}
}

func readMultiSearchResponse(dec *imapwire.Decoder) (MultiSearchResult, error) {
	var out MultiSearchResult
	out.Data.Values = make(map[ESearchReturnKey]string)
	tokenPending := false
	if dec.SP() {
		if dec.PeekSpecial('(') {
			if !dec.ExpectSpecial('(') {
				return out, dec.Err()
			}
			for {
				var label string
				if !dec.ExpectAtom(&label) || !dec.ExpectSP() {
					return out, dec.Err()
				}
				switch strings.ToUpper(label) {
				case "TAG":
					if !dec.ExpectString(&out.Tag) {
						return out, dec.Err()
					}
				case "MAILBOX":
					if !dec.ExpectAstring(&out.Mailbox) {
						return out, dec.Err()
					}
				case "UIDVALIDITY":
					if !dec.ExpectNumber(&out.UIDValidity) {
						return out, dec.Err()
					}
				default:
					return out, fmt.Errorf("unexpected ESEARCH correlator %q", label)
				}
				if dec.Special(')') {
					break
				}
				if !dec.ExpectSP() {
					return out, dec.Err()
				}
			}
		} else {
			tokenPending = true
		}
	}
	if err := readESearchItems(dec, &out.Data, tokenPending); err != nil {
		return out, err
	}
	if !dec.ExpectCRLF() {
		return out, dec.Err()
	}
	return out, nil
}
