package imapclient

import (
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ContextUpdate is shared response vocabulary; see [imap.ContextUpdate].
type ContextUpdate = imap.ContextUpdate

// searchContext is one RETURN (UPDATE) registration. uid and sorted are fixed
// before the command is issued; tag is assigned with the command tag under
// Client.mu, before the request can be answered. foreign is reader-goroutine
// state: the command's initial result correlated with another tag, so its Wait
// fails and the registration must not outlive tagged completion. The map
// containing registrations is protected by Client.mu; no event queue or
// retained result set is needed.
type searchContext struct {
	uid, sorted bool
	tag         string
	foreign     bool
}

// observeInitialTag is called by the owning command's collector, on the reader
// goroutine, with the correlator of an initial result. A nil registration is a
// command without UPDATE.
func (r *searchContext) observeInitialTag(tag string) {
	if r != nil && tag != "" && tag != r.tag {
		r.foreign = true
	}
}

func hasSearchReturnKeyword(keywords []string, want SearchReturnOptionKeyword) bool {
	for _, keyword := range keywords {
		if strings.EqualFold(keyword, string(want)) {
			return true
		}
	}
	return false
}

// finishContextCommandLocked runs before the tagged completion unblocks Wait.
func (c *Client) finishContextCommandLocked(cmd *Command, success bool) {
	// A refused request leaves no registration. Neither does one whose initial
	// result belongs to another tag: its Wait fails, so the caller has no
	// result to which later updates could apply.
	if !success || cmd.searchContext != nil && cmd.searchContext.foreign {
		delete(c.searchContexts, cmd.tag)
	}
	if !success {
		return
	}
	for _, tag := range cmd.cancelUpdates {
		delete(c.searchContexts, tag)
	}
	switch cmd.name {
	case "CLOSE", "UNSELECT", "UNAUTHENTICATE", "LOGOUT":
		c.searchContexts = nil
	}
}

// observeContextCondition removes the registration a NOUPDATE refusal names,
// before the condition reaches a collector or handler. A malformed argument
// names no registration; the condition is then delivered exactly as it was
// before CONTEXT delivery existed, rather than failing a connection that may
// never have asked for UPDATE.
func (c *Client) observeContextCondition(cond imapwire.RespCond) {
	if !strings.EqualFold(cond.Text.Code, "NOUPDATE") {
		return
	}
	tag, err := ParseNoUpdateArgs(cond.Text.Args)
	if err != nil {
		return
	}
	c.mu.Lock()
	delete(c.searchContexts, tag)
	c.mu.Unlock()
}

// routeContextUpdate runs before collectors. Known action items cannot be an
// initial result, even for an unknown or just-cancelled context. An ESEARCH for
// a live, completed context is also an event (including future return keys).
// Ordinary foreign-tag initial results still reach their collector and fail
// its existing correlator check; this is not search-pipelining support.
func (c *Client) routeContextUpdate(resp *untaggedResponse) (bool, error) {
	parsed := resp.data.esearch
	if parsed == nil {
		return false, nil
	}
	hasActions := false
	for _, item := range parsed.items {
		if item.key == imap.ESearchReturnKeyAddTo || item.key == imap.ESearchReturnKeyRemoveFrom {
			hasActions = true
			break
		}
	}
	tag := parsed.result.Tag
	c.mu.Lock()
	registration := c.searchContexts[tag]
	pending := c.pending[tag] != nil
	c.mu.Unlock()
	if !hasActions && (registration == nil || pending) {
		return false, nil
	}
	labels := make(map[string]bool)
	for _, label := range parsed.correlator {
		if labels[label] {
			return true, fmt.Errorf("duplicate CONTEXT correlator %q", label)
		}
		labels[label] = true
	}
	if tag == "" || !labels["TAG"] || labels["MAILBOX"] != labels["UIDVALIDITY"] {
		return true, fmt.Errorf("CONTEXT update requires a TAG and paired MULTISEARCH correlators")
	}
	uid := parsed.result.Data.UID
	if registration != nil {
		// As for the initial result, a UID context tolerates a missing UID
		// indicator; an explicit one on a sequence-number context fails.
		if uid && !registration.uid {
			return true, fmt.Errorf("CONTEXT update UID indicator does not match its searching command")
		}
		uid = registration.uid
	}
	update, err := decodeContextUpdate(parsed, uid)
	if err != nil {
		return true, err
	}
	c.trace(TraceServer, "* ESEARCH UPDATE")
	if registration == nil {
		return true, nil // cancelled, refused, abandoned, or unsolicited tag
	}
	if registration.sorted {
		for _, item := range update.Items {
			for _, change := range item.Changes {
				if change.Position == 0 {
					return true, fmt.Errorf("SORT context update position must be non-zero")
				}
			}
		}
	}
	if h := c.opts.UnilateralData; h != nil && h.ContextUpdate != nil {
		h.ContextUpdate(update)
	}
	return true, nil
}

// decodeContextUpdate interprets numbers in the given mode, which is the
// registration's for a live context and the response's own otherwise.
func decodeContextUpdate(parsed *esearchResponse, uid bool) (*ContextUpdate, error) {
	update := &ContextUpdate{Tag: parsed.result.Tag, UID: uid, Mailbox: parsed.result.Mailbox, UIDValidity: parsed.result.UIDValidity}
	for _, raw := range parsed.items {
		item := imap.ContextUpdateItem{Key: raw.key, Value: raw.value}
		if raw.key == imap.ESearchReturnKeyAddTo || raw.key == imap.ESearchReturnKeyRemoveFrom {
			// CaptureValue already bounded this input with the connection's
			// decoder limits. Do not impose a smaller default line limit when
			// interpreting a captured value from a custom-limit connection.
			dec := imapwire.NewDecoderString(raw.value+"\r\n", &imapwire.Options{MaxLineLength: len(raw.value) + 2})
			var err error
			item.Changes, err = imapcodec.ReadContextUpdateChanges(dec, update.UID)
			if err != nil {
				return nil, err
			}
			if !dec.ExpectCRLF() {
				return nil, dec.Err()
			}
		}
		update.Items = append(update.Items, item)
	}
	return update, nil
}
