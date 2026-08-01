package imapclient

import (
	"context"
	"slices"
	"strings"
)

// CapabilityOptions configures [Client.Capability]. A nil pointer selects the
// defaults.
//
// It carries no fields today; capability names are an open-ended set and this
// keeps a future refresh policy or filter addable without a signature change.
//
// Construct with keyed fields only; fields may be added in a future release.
type CapabilityOptions struct {
	_ struct{}
}

// Capability asks the server for its current capability set. Servers commonly
// change this set after STARTTLS and authentication, so callers normally do
// not need this method: DialStartTLS and the authentication methods refresh it
// automatically. The newly received set replaces the previous CAPABILITY
// command result.
//
// A nil options pointer selects the defaults.
func (c *Client) Capability(ctx context.Context, options *CapabilityOptions) error {
	return c.requestCapability(ctx)
}

// Capabilities returns a snapshot of the capability names learned from the
// greeting or a CAPABILITY response. Names are upper-cased. The returned map
// is owned by the caller. It contains advertised wire tokens only; use
// [Client.Supports] for a feature gate that also understands IMAP4rev2.
func (c *Client) Capabilities() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[string]bool, len(c.caps))
	for capability := range c.caps {
		result[capability] = true
	}
	return result
}

// CapabilityValues returns the values advertised for a parameterised
// capability name. For example, CapabilityValues("AUTH") can return PLAIN and
// SCRAM-SHA-256, and CapabilityValues("APPENDLIMIT") can return a server's
// limit. Names and values are returned upper-cased because IMAP capabilities
// are case-insensitive. The returned slice is owned by the caller.
func (c *Client) CapabilityValues(name string) []string {
	prefix := strings.ToUpper(strings.TrimSuffix(name, "=")) + "="
	c.mu.Lock()
	defer c.mu.Unlock()
	values := make([]string, 0)
	for capability := range c.caps {
		if strings.HasPrefix(capability, prefix) {
			values = append(values, strings.TrimPrefix(capability, prefix))
		}
	}
	slices.Sort(values)
	return values
}

// EnabledCapabilities returns a snapshot of the capabilities the server
// actually enabled in response to ENABLE. It can be a subset of the requested
// capabilities. Names are upper-cased and the returned map is owned by the
// caller.
func (c *Client) EnabledCapabilities() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make(map[string]bool, len(c.enabled))
	for capability := range c.enabled {
		result[capability] = true
	}
	return result
}

// Supports reports whether a capability is available for use in this session.
// It includes capabilities explicitly advertised by the server and the
// capabilities made mandatory by RFC 9051 once IMAP4rev2 has been enabled.
// Capabilities returns only the server's advertised wire tokens; use Supports
// for feature gates.
func (c *Client) Supports(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.supportsLocked(name)
}

// supportsLocked is [Client.Supports] for callers already holding mu.
func (c *Client) supportsLocked(name string) bool {
	name = strings.ToUpper(name)
	if _, ok := c.caps[name]; ok {
		return true
	}
	if _, rev2 := c.enabled["IMAP4REV2"]; !rev2 {
		return false
	}
	return rev2MandatoryCapabilities[name]
}

func (c *Client) hasCapability(name string) bool { return c.Supports(name) }

var rev2MandatoryCapabilities = map[string]bool{
	"BINARY":        true,
	"ENABLE":        true,
	"IDLE":          true,
	"IMAP4REV2":     true,
	"LIST-EXTENDED": true,
	"MOVE":          true,
	"NAMESPACE":     true,
	"SASL-IR":       true,
	"SPECIAL-USE":   true,
	"UIDPLUS":       true,
}
