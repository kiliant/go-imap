package memory

import (
	"slices"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// Group A capability support: CHILDREN (RFC 3348), SPECIAL-USE and
// CREATE-SPECIAL-USE (RFC 6154), and WITHIN (RFC 5032).
//
// ESEARCH, SEARCHRES, LIST-EXTENDED and LIST-STATUS are answered entirely by the
// framework from data this backend already returns, so they are not witnessed
// here — a backend does not opt into them and cannot fail to support them.

// supportedCapabilities are the tokens this backend witnesses for group A.
//
// WITHIN is included because OLDER and YOUNGER reach the search evaluator as
// ordinary criteria and internal dates are stored per message, so the keys are
// genuinely evaluated rather than ignored. Advertising a search key a backend
// silently drops would make every result quietly wrong.
var supportedCapabilities = map[string]bool{
	"CHILDREN":           true,
	"SPECIAL-USE":        true,
	"CREATE-SPECIAL-USE": true,
	"WITHIN":             true,
}

// SupportsCapability implements [imapserver.CapabilitySupport]. It is declared
// on Backend rather than on session because none of these vary by user here.
func (b *Backend) SupportsCapability(name string) bool {
	return supportedCapabilities[strings.ToUpper(name)]
}

// mailboxAttrsLocked reports every attribute this backend knows about a
// mailbox. The framework filters the result down to what the client's LIST
// return options asked for, so reporting more here is not the same as leaking
// it onto the wire.
//
// The caller must hold the account lock.
func (s *session) mailboxAttrsLocked(m *mailbox) []imap.MailboxAttr {
	attrs := slices.Clone(m.specialUse)
	if m.subscribed {
		attrs = append(attrs, imap.MailboxAttrSubscribed)
	}
	if s.hasChildrenLocked(m.name) {
		attrs = append(attrs, imap.MailboxAttrHasChildren)
	} else {
		attrs = append(attrs, imap.MailboxAttrHasNoChildren)
	}
	return attrs
}

// hasChildrenLocked reports whether any mailbox is a descendant of name under
// this backend's fixed "/" hierarchy delimiter.
//
// The caller must hold the account lock.
func (s *session) hasChildrenLocked(name string) bool {
	prefix := name + "/"
	for _, other := range s.account.mailboxes {
		if strings.HasPrefix(other.name, prefix) {
			return true
		}
	}
	return false
}

// specialUseInUseLocked reports whether another mailbox already claims attr.
// RFC 6154 section 3 leaves this to the server; this backend allows one mailbox
// per use attribute, which is the behaviour clients expect when they look up
// "the" Drafts mailbox.
//
// The caller must hold the account lock.
func (s *session) specialUseInUseLocked(attr imap.MailboxAttr) bool {
	for _, m := range s.account.mailboxes {
		if slices.ContainsFunc(m.specialUse, attr.Equal) {
			return true
		}
	}
	return false
}

// applyCreateSpecialUseLocked validates and records the USE parameter supplied
// with CREATE.
//
// The caller must hold the account lock.
func (s *session) applyCreateSpecialUseLocked(m *mailbox, options *imapserver.CreateOptions) error {
	if options == nil || len(options.SpecialUse) == 0 {
		return nil
	}
	for _, attr := range options.SpecialUse {
		if s.specialUseInUseLocked(attr) {
			return &imap.Error{
				Type: imap.ErrorTypeNo,
				Code: imap.CodeUseAttr,
				Text: "another mailbox already has that use attribute",
			}
		}
	}
	m.specialUse = slices.Clone(options.SpecialUse)
	return nil
}

var _ imapserver.CapabilitySupport = (*Backend)(nil)
