package imapclient

import (
	"context"
	"strings"

	"github.com/kiliant/go-imap"
)

// UnauthenticateOptions configures UNAUTHENTICATE. A nil pointer selects the
// defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type UnauthenticateOptions struct {
	_ struct{}
}

// Unauthenticate returns the session to the not-authenticated state without
// closing the connection. UNAUTHENTICATE, RFC 8437.
//
// On success the ENABLE set is cleared and any selected mailbox is closed.
// Capabilities are reset and then rediscovered with CAPABILITY, matching the
// post-login refresh LOGIN/AUTHENTICATE perform: a tagged OK [CAPABILITY …]
// on the UNAUTHENTICATE response is preserved when present, otherwise a
// follow-up CAPABILITY command fills the set for the not-authenticated state.
func (c *Client) Unauthenticate(ctx context.Context, options *UnauthenticateOptions) error {
	_ = options
	if ctx == nil {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "UNAUTHENTICATE requires a non-nil context"}
	}
	if !c.Supports("UNAUTHENTICATE") {
		return capabilityError("UNAUTHENTICATE", "UNAUTHENTICATE")
	}
	cmd := c.beginCommandWithCompletion("UNAUTHENTICATE", stateAuthenticated|stateSelected, nil, nil, func(success bool, code, args string) {
		if !success {
			return
		}
		c.mu.Lock()
		if !c.closed {
			c.state = StateNotAuthenticated
			c.caps = make(map[string]struct{})
			c.enabled = make(map[string]struct{})
			c.selectedMailbox = ""
			c.enc.SetUTF8Accept(false)
			c.dec.SetUTF8Accept(false)
			// completeTagged applies [CAPABILITY …] before onComplete; re-apply
			// those values after the wipe so a server that announces the
			// post-reset set on the tagged OK is not ignored.
			if strings.EqualFold(code, "CAPABILITY") {
				for _, cap := range strings.Fields(args) {
					c.caps[strings.ToUpper(cap)] = struct{}{}
				}
			}
		}
		c.mu.Unlock()
	})
	if err := cmd.Wait(ctx); err != nil {
		return err
	}
	if len(c.Capabilities()) != 0 {
		return nil
	}
	return c.requestCapability(ctx)
}
