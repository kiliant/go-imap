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

// searchContext is immutable after command registration. The map containing
// it is protected by Client.mu; no event queue or retained result set is needed.
type searchContext struct {
	uid, sorted bool
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
	if !success {
		delete(c.searchContexts, cmd.tag)
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

func (c *Client) observeContextCondition(cond imapwire.RespCond) error {
	if !strings.EqualFold(cond.Text.Code, "NOUPDATE") {
		return nil
	}
	tag, err := ParseNoUpdateArgs(cond.Text.Args)
	if err != nil {
		return err
	}
	c.mu.Lock()
	delete(c.searchContexts, tag)
	c.mu.Unlock()
	return nil
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
	if registration != nil && uid != registration.uid {
		return true, fmt.Errorf("CONTEXT update UID indicator does not match its searching command")
	}
	update, err := decodeContextUpdate(parsed)
	if err != nil {
		return true, err
	}
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
	c.trace(TraceServer, "* ESEARCH UPDATE")
	return true, nil
}

func decodeContextUpdate(parsed *esearchResponse) (*ContextUpdate, error) {
	update := &ContextUpdate{Tag: parsed.result.Tag, UID: parsed.result.Data.UID, Mailbox: parsed.result.Mailbox, UIDValidity: parsed.result.UIDValidity}
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
