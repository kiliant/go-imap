package imapclient

import (
	"context"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// JMAPAccessData is the result of GETJMAPACCESS. It is an alias for
// [imap.JMAPAccessData], which both protocol directions share.
type JMAPAccessData = imap.JMAPAccessData

// GetJMAPAccessOptions configures GETJMAPACCESS. A nil pointer selects the
// defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type GetJMAPAccessOptions struct {
	_ struct{}
}

// GetJMAPAccess returns the JMAP session URL for the same mailstore.
// GETJMAPACCESS, RFC 9698.
//
// It requires the JMAPACCESS capability. The server must also advertise
// OBJECTID; this client does not enforce that pairing client-side because a
// misconfigured server still answers GETJMAPACCESS usefully, and the
// capability check alone is what prevents sending an unknown command.
func (c *Client) GetJMAPAccess(ctx context.Context, options *GetJMAPAccessOptions) (*JMAPAccessData, error) {
	_ = options
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETJMAPACCESS requires a non-nil context"}
	}
	if !c.Supports("JMAPACCESS") {
		return nil, capabilityError("GETJMAPACCESS", "JMAPACCESS")
	}
	data := &JMAPAccessData{}
	var got bool
	cmd := c.beginCommand("GETJMAPACCESS", stateAuthenticated|stateSelected, nil, func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil || resp.name != "JMAPACCESS" {
			return false, nil
		}
		parsed, err := readJMAPAccessResponse(resp.dec)
		if err != nil {
			return true, err
		}
		*data = *parsed
		got = true
		return true, nil
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	if !got {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETJMAPACCESS completed without a JMAPACCESS response"}
	}
	return data, nil
}

func readJMAPAccessResponse(dec *imapwire.Decoder) (*JMAPAccessData, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	var url string
	if !dec.Quoted(&url) && !dec.ExpectAstring(&url) {
		return nil, dec.Err()
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return &JMAPAccessData{SessionURL: url}, nil
}
