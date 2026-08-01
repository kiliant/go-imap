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
	// RFC 5267 section 4.3.
	SearchReturnUpdate SearchReturnOptionKeyword = "UPDATE"

	// SearchReturnESortAll is the ALL return option for ESORT. RFC 5267
	// section 3.1; identical spelling to [SearchReturnAll].
	SearchReturnESortAll = SearchReturnAll
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
// least one is required. A nil options pointer selects the defaults.
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
	cmd := c.beginCommand("CANCELUPDATE", stateSelected, func(enc *imapwire.Encoder) {
		for _, tag := range tags {
			enc.SP().Quoted(tag)
		}
	}, nil)
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
