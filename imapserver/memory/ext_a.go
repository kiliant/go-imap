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
	// UIDPLUS: this backend returns UIDs in AppendData and CopyData, so the
	// APPENDUID and COPYUID codes are real, and its Expunge honours the UID-set
	// filter UID EXPUNGE supplies.
	"UIDPLUS":            true,
	"CHILDREN":           true,
	"SPECIAL-USE":        true,
	"CREATE-SPECIAL-USE": true,
	"WITHIN":             true,
	// Group B. See ext_b.go, and the note there about what QRESYNC cannot
	// survive in an in-memory backend.
	"CONDSTORE": true,
	"QRESYNC":   true,
	// Group B attribute extensions. Each is a FETCH or STATUS item this
	// backend genuinely produces; see ext_b.go.
	"OBJECTID":    true,
	"SAVEDATE":    true,
	"STATUS=SIZE": true,
	"APPENDLIMIT": true,
	"PREVIEW":     true,
	"REPLACE":     true,
	// Group D. QUOTA, ACL, METADATA, NAMESPACE and UNAUTHENTICATE witness
	// themselves by implementing their optional interfaces; only JMAPACCESS
	// is a spoken claim, and this backend serves no JMAP endpoint.
	// Group C. SORT and THREAD also need the spoken witness because the
	// interface alone does not say which keys and algorithms are real.
	// SORT=DISPLAY is included: the display keys are genuinely evaluated
	// against the envelope, not silently treated as their non-display forms.
	"SORT":         true,
	"SORT=DISPLAY": true,
	// ORDEREDSUBJECT only. REFERENCES needs a Message-ID graph this backend
	// does not retain, and answering it with ORDEREDSUBJECT results would
	// silently mis-thread the client's view — so the token is withheld and
	// Thread refuses the algorithm. RFC 5256 has no bare "THREAD" capability.
	"THREAD=ORDEREDSUBJECT": true,
	"BINARY":                true,
	"MULTISEARCH":           true,
	// SEARCH=FUZZY (RFC 6203). The shared evaluator answers FUZZY with exact
	// matches, which RFC 6203 section 2 permits — the server chooses the
	// algorithm, and an exact match is a fuzzy match with the confidence turned
	// up. The token was missing while the behaviour was present, so the key
	// parsed, evaluated and returned results from a server that never offered
	// it. Advertising it is what makes the key legitimately usable.
	"SEARCH=FUZZY": true,
	"UTF8=APPEND":  true,
	// Group E. LANGUAGE and URLAUTH witness themselves through their optional
	// interfaces; these are the tokens that need a spoken claim.
	"I18NLEVEL=1": true,
	"URL-PARTIAL": true,
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
