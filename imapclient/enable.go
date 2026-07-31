package imapclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap/internal/imapwire"
)

// EnableCommand is an in-flight ENABLE command. ENABLE is only valid after
// authentication and before selecting a mailbox.
type EnableCommand struct {
	*Command
	enabled []string
}

// Wait waits for ENABLE and returns the capabilities the server actually
// enabled. A server is allowed to enable only a subset of the requested
// capabilities. The returned slice is owned by the caller.
func (cmd *EnableCommand) Wait(ctx context.Context) ([]string, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil enable command")
	}
	if err := cmd.Command.Wait(ctx); err != nil {
		return nil, err
	}
	return append([]string(nil), cmd.enabled...), nil
}

// Enable requests the supplied RFC 5161 capabilities. It accepts extension
// names as open strings so later RFCs do not require an API change. The common
// values are IMAP4rev2, CONDSTORE, QRESYNC, and UTF8=ACCEPT.
//
// The command is rejected locally unless the session is authenticated and no
// mailbox is selected. Use [EnableCommand.Wait] to learn the subset the server
// actually enabled.
func (c *Client) Enable(capabilities ...string) *EnableCommand {
	result := &EnableCommand{}
	if len(capabilities) == 0 {
		result.Command = rejectedCommand(c, "ENABLE", "ENABLE requires at least one capability")
		return result
	}
	requested := make([]string, 0, len(capabilities))
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		capability = strings.ToUpper(capability)
		if capability == "" {
			result.Command = rejectedCommand(c, "ENABLE", "ENABLE capability must not be empty")
			return result
		}
		if _, ok := seen[capability]; ok {
			continue
		}
		seen[capability] = struct{}{}
		requested = append(requested, capability)
	}
	var cmd *Command
	cmd = c.beginCommandWithCompletion("ENABLE", stateAuthenticated, func(enc *imapwire.Encoder) {
		for _, capability := range requested {
			enc.SP().Atom(capability)
		}
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.name != "ENABLED" || resp.hasNum || resp.cond != nil {
			return false, nil
		}
		for resp.dec.SP() {
			var capability string
			if !resp.dec.ExpectAtom(&capability) {
				return true, resp.dec.Err()
			}
			result.enabled = append(result.enabled, strings.ToUpper(capability))
		}
		if !resp.dec.ExpectCRLF() {
			return true, resp.dec.Err()
		}
		return true, nil
	}, func(success bool) {
		if !success {
			return
		}
		c.mu.Lock()
		for _, capability := range result.enabled {
			c.enabled[capability] = struct{}{}
			if capability == "UTF8=ACCEPT" {
				c.enc.SetUTF8Accept(true)
				c.dec.SetUTF8Accept(true)
			}
		}
		c.mu.Unlock()
	})
	result.Command = cmd
	return result
}

func (c *Client) hasEnabled(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.enabled[strings.ToUpper(name)]
	return ok
}

func (c *Client) rev2Enabled() bool { return c.hasEnabled("IMAP4REV2") }
