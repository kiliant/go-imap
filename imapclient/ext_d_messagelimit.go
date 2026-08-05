package imapclient

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
)

// MessageLimitData reports a server-imposed per-command message count limit.
// MESSAGELIMIT / SAVELIMIT, RFC 9738.
//
// Construct with keyed fields only; fields may be added in a future release.
type MessageLimitData struct {
	// Limit is the maximum number of messages a single FETCH/SEARCH/STORE/
	// COPY/MOVE/APPEND/UID EXPUNGE (MESSAGELIMIT) or COPY/APPEND (SAVELIMIT)
	// may process. It is always positive when the capability is well-formed.
	Limit uint32

	// SaveOnly is true when the limit came from SAVELIMIT= rather than
	// MESSAGELIMIT=: only COPY/APPEND (and UID variants) are constrained.
	SaveOnly bool

	_ struct{}
}

// MessageLimitPartial is the parsed form of a MESSAGELIMIT response code. It
// is an alias for [imap.MessageLimitPartial], which both protocol directions
// share.
type MessageLimitPartial = imap.MessageLimitPartial

// MessageLimit returns the MESSAGELIMIT=N capability value, or falls back to
// SAVELIMIT=N when only the narrower form is advertised. RFC 9738.
//
// When neither is advertised it returns an [imap.Error] wrapping
// [ErrCapabilityNotAdvertised]. There is no STATUS equivalent: the limit is
// only published as a capability parameter.
func (c *Client) MessageLimit() (*MessageLimitData, error) {
	if values := c.CapabilityValues("MESSAGELIMIT"); len(values) > 0 {
		limit, err := parsePositiveUint32(values[0], "MESSAGELIMIT")
		if err != nil {
			return nil, err
		}
		return &MessageLimitData{Limit: limit}, nil
	}
	if values := c.CapabilityValues("SAVELIMIT"); len(values) > 0 {
		limit, err := parsePositiveUint32(values[0], "SAVELIMIT")
		if err != nil {
			return nil, err
		}
		return &MessageLimitData{Limit: limit, SaveOnly: true}, nil
	}
	return nil, capabilityError("MESSAGELIMIT/SAVELIMIT", "MESSAGELIMIT")
}

// SaveLimit returns the SAVELIMIT=N capability value. RFC 9738.
//
// Prefer [Client.MessageLimit] when either form is acceptable: a server that
// advertises MESSAGELIMIT already covers COPY/APPEND. This method exists so a
// caller that only cares about save operations can ignore a broader limit.
func (c *Client) SaveLimit() (*MessageLimitData, error) {
	if values := c.CapabilityValues("SAVELIMIT"); len(values) > 0 {
		limit, err := parsePositiveUint32(values[0], "SAVELIMIT")
		if err != nil {
			return nil, err
		}
		return &MessageLimitData{Limit: limit, SaveOnly: true}, nil
	}
	if values := c.CapabilityValues("MESSAGELIMIT"); len(values) > 0 {
		limit, err := parsePositiveUint32(values[0], "MESSAGELIMIT")
		if err != nil {
			return nil, err
		}
		return &MessageLimitData{Limit: limit}, nil
	}
	return nil, capabilityError("SAVELIMIT", "SAVELIMIT")
}

// ParseMessageLimitArgs parses a MESSAGELIMIT response-code argument list.
// RFC 9738 section 5: message-limit [SP uniqueid].
func ParseMessageLimitArgs(args string) (*MessageLimitPartial, error) {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 || len(fields) > 2 {
		return nil, fmt.Errorf("invalid MESSAGELIMIT response code %q", args)
	}
	limit, err := parsePositiveUint32(fields[0], "MESSAGELIMIT")
	if err != nil {
		return nil, err
	}
	out := &MessageLimitPartial{Limit: limit}
	if len(fields) == 2 {
		uid, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil || uid == 0 {
			return nil, fmt.Errorf("invalid MESSAGELIMIT UID %q", fields[1])
		}
		out.LowestUID = imap.UID(uid)
		out.HasLowestUID = true
	}
	return out, nil
}

func parsePositiveUint32(raw, label string) (uint32, error) {
	n, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || n == 0 {
		return 0, &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("invalid %s capability value %q", label, raw)}
	}
	return uint32(n), nil
}
