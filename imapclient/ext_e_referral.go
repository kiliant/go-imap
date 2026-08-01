package imapclient

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
)

// ReferralData is a parsed REFERRAL response code. LOGIN-REFERRALS (RFC 2221)
// and MAILBOX-REFERRALS (RFC 2193) both use the same code; the IMAP URL tells
// the client where to reconnect or which mailbox is elsewhere.
//
// Construct with keyed fields only; fields may be added in a future release.
type ReferralData struct {
	// URL is the IMAP URL from the response code, verbatim. Callers parse it
	// with net/url or an IMAP-URL helper; this library preserves the raw form
	// so unknown URL parameters survive.
	URL string
	_   struct{}
}

// ParseReferralArgs extracts the IMAP URL from a REFERRAL response code's
// arguments. RFC 2221 / RFC 2193.
//
// The wire form is a single imapurl with no surrounding quotes in the common
// case (`[REFERRAL IMAP://user@host/]`). Some servers quote it; both spellings
// are accepted. An empty argument is rejected — a referral without a URL is
// useless and would hide the failure mode the response code exists to report.
func ParseReferralArgs(args string) (*ReferralData, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return nil, fmt.Errorf("REFERRAL response code requires an IMAP URL")
	}
	if len(args) >= 2 && args[0] == '"' && args[len(args)-1] == '"' {
		// Strip a single layer of IMAP quoting without interpreting escapes
		// beyond \" and \\ — enough for URLs, which rarely need more.
		var b strings.Builder
		escaped := false
		for i := 1; i < len(args)-1; i++ {
			c := args[i]
			if escaped {
				b.WriteByte(c)
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			b.WriteByte(c)
		}
		args = b.String()
	}
	if args == "" {
		return nil, fmt.Errorf("REFERRAL response code requires an IMAP URL")
	}
	return &ReferralData{URL: args}, nil
}

// ReferralFromError returns the referral encoded in err, if any. It matches
// [imap.CodeReferral] on an [*imap.Error] and parses [Error.CodeArgs].
func ReferralFromError(err error) (*ReferralData, bool) {
	var ierr *imap.Error
	if !errors.As(err, &ierr) || ierr.Code != imap.CodeReferral {
		return nil, false
	}
	data, parseErr := ParseReferralArgs(ierr.CodeArgs)
	if parseErr != nil {
		return nil, false
	}
	return data, true
}

// SupportsLoginReferrals reports LOGIN-REFERRALS. RFC 2221.
func (c *Client) SupportsLoginReferrals() bool { return c.Supports("LOGIN-REFERRALS") }

// SupportsMailboxReferrals reports MAILBOX-REFERRALS. RFC 2193.
func (c *Client) SupportsMailboxReferrals() bool { return c.Supports("MAILBOX-REFERRALS") }
