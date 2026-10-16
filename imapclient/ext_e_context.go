package imapclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// CONTEXT / ESORT return options (RFC 5267). They are bare keywords on the
// SEARCH/SORT RETURN list; use them through [ESearchOptions.ReturnOptions]
// (or the SORT helpers once T10 lands). PARTIAL takes arguments and needs a
// structured option owned by T08 — escalate before inventing a second SEARCH
// API here.

const (
	// SearchReturnContext asks the server to retain the result as a CONTEXT.
	// CONTEXT=SEARCH / CONTEXT=SORT, RFC 5267 section 4.2.
	SearchReturnContext SearchReturnOptionKeyword = "CONTEXT"

	// SearchReturnUpdate asks for incremental ESEARCH updates to a CONTEXT.
	// RFC 5267 section 4.3. Requires CONTEXT=SEARCH for SearchExtended,
	// SearchPartial and MultiSearch, or CONTEXT=SORT for SortExtended. Updates arrive through
	// [UnilateralDataHandler.ContextUpdate], independently of command results.
	SearchReturnUpdate SearchReturnOptionKeyword = "UPDATE"

	// SearchReturnESortAll is the ALL return option for ESORT. RFC 5267
	// section 3.1; identical spelling to [SearchReturnAll].
	SearchReturnESortAll = SearchReturnAll
)

// ESearchReturnKeyAddTo and ESearchReturnKeyRemoveFrom are the CONTEXT update
// items, RFC 5267 sections 4.3.3 and 4.3.4. See [ContextUpdate].
const (
	ESearchReturnKeyAddTo      = imap.ESearchReturnKeyAddTo
	ESearchReturnKeyRemoveFrom = imap.ESearchReturnKeyRemoveFrom
)

// CancelUpdateOptions configures CANCELUPDATE. A nil pointer selects the
// defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type CancelUpdateOptions struct {
	_ struct{}
}

// CancelUpdate cancels CONTEXT UPDATE notifications. CANCELUPDATE, RFC 5267
// section 4.3.5. Requires CONTEXT=SEARCH or CONTEXT=SORT.
//
// tags are the command tags whose UPDATE contexts should be cancelled; at
// least one is required. Use the searching command's Tag or the result's
// [ESearchData.ContextTag]. A nil options pointer selects the defaults.
//
// # Registration lifecycle
//
// A searching command with RETURN (UPDATE) registers its tag before the
// request is written. [UnilateralDataHandler.ContextUpdate] receives that
// tag's updates until the registration ends:
//
//   - CancelUpdate: updates may still arrive before CANCELUPDATE's tagged OK
//     and are not delivered after it. A failed cancellation leaves the
//     registrations active.
//   - Refusal: an [imap.CodeNoUpdate] response code ends the registration it
//     names ([ParseNoUpdateArgs] extracts the tag), in an untagged NO before
//     [UnilateralDataHandler.StatusResponse] sees it. A tagged NO or BAD
//     ends the command's own.
//   - A failed initial result: when Wait fails because the ESEARCH response
//     correlates with another tag, the registration ends at tagged
//     completion. The server may still hold the context; cancelling the
//     command's tag releases it. When SortExtended returns that error, the
//     tag is the error's [imap.Error] Tag field.
//   - Selection: SELECT and EXAMINE end every registration when issued, even
//     if they then fail. A successful CLOSE, UNSELECT or UNAUTHENTICATE ends
//     them at its tagged OK.
//   - The session: BYE, LOGOUT and disconnect end every registration.
//
// A response carrying ADDTO or REMOVEFROM for a tag with no live registration
// is discarded. An update for a live registration that fails validation, such
// as a zero position in a SORT context, fails the connection.
func (c *Client) CancelUpdate(ctx context.Context, tags []string, options *CancelUpdateOptions) error {
	_ = options
	if ctx == nil {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "CANCELUPDATE requires a non-nil context"}
	}
	if len(tags) == 0 {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "CANCELUPDATE requires at least one tag"}
	}
	if !c.Supports("CONTEXT=SEARCH") && !c.Supports("CONTEXT=SORT") {
		return capabilityError("CANCELUPDATE", "CONTEXT=SEARCH")
	}
	for i, tag := range tags {
		if strings.TrimSpace(tag) == "" {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("CANCELUPDATE tag %d must not be empty", i)}
		}
	}
	tags = append([]string(nil), tags...)
	cmd := c.issue("CANCELUPDATE", commandOptions{allowed: stateSelected, cancelUpdates: tags, write: func(enc *imapwire.Encoder) {
		for _, tag := range tags {
			enc.SP().Quoted(tag)
		}
	}})
	return cmd.Wait(ctx)
}

// SupportsESort reports the ESORT capability. RFC 5267.
func (c *Client) SupportsESort() bool { return c.Supports("ESORT") }

// SupportsContextSearch reports CONTEXT=SEARCH. RFC 5267.
func (c *Client) SupportsContextSearch() bool { return c.Supports("CONTEXT=SEARCH") }

// SupportsContextSort reports CONTEXT=SORT. RFC 5267.
func (c *Client) SupportsContextSort() bool { return c.Supports("CONTEXT=SORT") }

// ParseNoUpdateArgs extracts the quoted tag from a NOUPDATE response code.
// RFC 5267 section 4.3.1: `NOUPDATE SP quoted`.
func ParseNoUpdateArgs(args string) (tag string, err error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return "", fmt.Errorf("NOUPDATE response code requires a tag")
	}
	dec := imapwire.NewDecoderString(args+"\r\n", nil)
	var quoted string
	if dec.Quoted(&quoted) {
		return quoted, nil
	}
	// Unquoted atom fallback for lenient servers.
	var atom string
	if dec.ExpectAtom(&atom) {
		return atom, nil
	}
	return "", fmt.Errorf("invalid NOUPDATE arguments %q", args)
}
