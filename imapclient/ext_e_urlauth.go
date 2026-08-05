package imapclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// URLAuthMechanism names a URLAUTH authorization mechanism such as INTERNAL.
// It is string-backed and open-ended: RFC 4467 registers INTERNAL and later
// documents may add more.
type URLAuthMechanism string

const (
	// URLAuthInternal is the INTERNAL mechanism of RFC 4467.
	URLAuthInternal URLAuthMechanism = "INTERNAL"
)

// GenURLAuthRequest is one rump-URL / mechanism pair for GENURLAUTH.
//
// Construct with keyed fields only; fields may be added in a future release.
type GenURLAuthRequest struct {
	// RumpURL is the IMAP URL ending in ";URLAUTH=<access>" without the
	// mechanism and token (RFC 4467 section 5).
	RumpURL string

	// Mechanism selects the authorization algorithm. Empty defaults to
	// INTERNAL.
	Mechanism URLAuthMechanism

	_ struct{}
}

// GenURLAuthOptions configures GENURLAUTH. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type GenURLAuthOptions struct {
	_ struct{}
}

// GenURLAuthData is the result of GENURLAUTH. It is an alias for
// [imap.GenURLAuthData], which both protocol directions share.
type GenURLAuthData = imap.GenURLAuthData

// URLFetchItem is one URL/body pair of a URLFETCH response. It is an alias
// for [imap.URLFetchItem], which both protocol directions share.
type URLFetchItem = imap.URLFetchItem

// URLFetchOptions configures URLFETCH. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type URLFetchOptions struct {
	_ struct{}
}

// ResetKeyOptions configures RESETKEY. A nil pointer resets every mailbox key.
//
// Construct with keyed fields only; fields may be added in a future release.
type ResetKeyOptions struct {
	// Mailbox, when non-empty, limits the reset to that mailbox.
	Mailbox string
	// Mechanisms restricts which URLAUTH mechanisms are reset. Empty means
	// every mechanism for the selected mailbox(es).
	Mechanisms []URLAuthMechanism
	_          struct{}
}

// GenURLAuth generates URLAUTH-authorized IMAP URLs. GENURLAUTH, RFC 4467.
//
// Requires URLAUTH. URLAUTH=BINARY (RFC 5524) is advertised separately and
// does not change this command's wire form. A nil options pointer selects the
// defaults.
func (c *Client) GenURLAuth(ctx context.Context, requests []GenURLAuthRequest, options *GenURLAuthOptions) (*GenURLAuthData, error) {
	_ = options
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GENURLAUTH requires a non-nil context"}
	}
	if len(requests) == 0 {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GENURLAUTH requires at least one request"}
	}
	if !c.Supports("URLAUTH") && !c.Supports("URLAUTH=BINARY") {
		return nil, capabilityError("GENURLAUTH", "URLAUTH")
	}
	for i, req := range requests {
		if strings.TrimSpace(req.RumpURL) == "" {
			return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("GENURLAUTH request %d: empty rump URL", i)}
		}
	}
	data := &GenURLAuthData{}
	limit := c.maxUntaggedResponses()
	count := 0
	cmd := c.beginCommand("GENURLAUTH", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		for _, req := range requests {
			mech := req.Mechanism
			if mech == "" {
				mech = URLAuthInternal
			}
			enc.SP().Astring(req.RumpURL).SP().Atom(string(mech))
		}
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil || resp.name != "GENURLAUTH" {
			return false, nil
		}
		if err := countUntaggedResponse(&count, limit, "GENURLAUTH"); err != nil {
			return true, err
		}
		urls, err := readGenURLAuthResponse(resp.dec)
		if err != nil {
			return true, err
		}
		data.URLs = append(data.URLs, urls...)
		return true, nil
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	if len(data.URLs) == 0 {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GENURLAUTH completed without a GENURLAUTH response"}
	}
	return data, nil
}

// URLFetch retrieves message data addressed by URLAUTH-authorized URLs.
// URLFETCH, RFC 4467. URL-PARTIAL (RFC 5550) is expressed inside the URL via
// ";PARTIAL="; [Client.SupportsURLPartial] reports whether the server accepts
// that field. A nil options pointer selects the defaults.
func (c *Client) URLFetch(ctx context.Context, urls []string, options *URLFetchOptions) ([]URLFetchItem, error) {
	_ = options
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "URLFETCH requires a non-nil context"}
	}
	if len(urls) == 0 {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "URLFETCH requires at least one URL"}
	}
	if !c.Supports("URLAUTH") && !c.Supports("URLAUTH=BINARY") {
		return nil, capabilityError("URLFETCH", "URLAUTH")
	}
	items := make([]URLFetchItem, 0, len(urls))
	limit := c.maxUntaggedResponses()
	count := 0
	cmd := c.beginCommand("URLFETCH", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		for _, url := range urls {
			enc.SP().Astring(url)
		}
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil || resp.name != "URLFETCH" {
			return false, nil
		}
		if err := countUntaggedResponse(&count, limit, "URLFETCH"); err != nil {
			return true, err
		}
		parsed, err := readURLFetchResponse(resp.dec)
		if err != nil {
			return true, err
		}
		items = append(items, parsed...)
		return true, nil
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "URLFETCH completed without a URLFETCH response"}
	}
	return items, nil
}

// SupportsURLPartial reports URL-PARTIAL. RFC 5550.
func (c *Client) SupportsURLPartial() bool { return c.Supports("URL-PARTIAL") }

// ResetKey resets URLAUTH mailbox access keys. RESETKEY, RFC 4467.
func (c *Client) ResetKey(ctx context.Context, options *ResetKeyOptions) error {
	if ctx == nil {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "RESETKEY requires a non-nil context"}
	}
	if !c.Supports("URLAUTH") && !c.Supports("URLAUTH=BINARY") {
		return capabilityError("RESETKEY", "URLAUTH")
	}
	if options != nil && options.Mailbox == "" && len(options.Mechanisms) != 0 {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "RESETKEY mechanisms require a mailbox"}
	}
	cmd := c.beginCommand("RESETKEY", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		if options == nil || options.Mailbox == "" {
			return
		}
		enc.SP().Mailbox(options.Mailbox)
		for _, mech := range options.Mechanisms {
			enc.SP().Atom(string(mech))
		}
	}, nil)
	return cmd.Wait(ctx)
}

func readGenURLAuthResponse(dec *imapwire.Decoder) ([]string, error) {
	var urls []string
	for dec.SP() {
		var url string
		if !dec.Quoted(&url) && !dec.ExpectAstring(&url) {
			return nil, dec.Err()
		}
		urls = append(urls, url)
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return urls, nil
}

func readURLFetchResponse(dec *imapwire.Decoder) ([]URLFetchItem, error) {
	var items []URLFetchItem
	for {
		if !dec.SP() {
			break
		}
		var url string
		if !dec.Quoted(&url) && !dec.ExpectAstring(&url) {
			return nil, dec.Err()
		}
		if !dec.ExpectSP() {
			return nil, dec.Err()
		}
		var body string
		var isNil bool
		if !dec.ExpectNString(&body, &isNil) {
			return nil, dec.Err()
		}
		item := URLFetchItem{URL: url}
		if !isNil {
			v := body
			item.Body = &v
		}
		items = append(items, item)
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return items, nil
}
