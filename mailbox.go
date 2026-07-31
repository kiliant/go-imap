package imap

import "strings"

// MailboxAttr is an attribute reported for a mailbox in a LIST or LSUB
// response. RFC 3501 section 7.2.2, RFC 9051 section 7.3.1.
//
// Like [Flag] it is a string-backed named type and NOT an enumeration. Every
// extension that touches LIST adds attributes — RFC 3348 added the child
// attributes, RFC 5258 the subscription and existence attributes, RFC 6154 the
// special-use attributes — and the IANA "IMAP Mailbox Name Attributes" registry
// continues to grow. An attribute this library does not know is passed through
// verbatim.
//
// Compare with [MailboxAttr.Equal]; RFC 3501 defines these atoms as
// case-insensitive.
type MailboxAttr string

// Base mailbox attributes. RFC 3501 section 7.2.2, RFC 9051 section 7.3.1.
const (
	// MailboxAttrNoInferiors means the mailbox can never have children.
	MailboxAttrNoInferiors MailboxAttr = "\\Noinferiors"
	// MailboxAttrNoSelect means the mailbox cannot be selected.
	MailboxAttrNoSelect MailboxAttr = "\\Noselect"
	// MailboxAttrMarked means the server considers the mailbox interesting.
	MailboxAttrMarked MailboxAttr = "\\Marked"
	// MailboxAttrUnmarked means the server considers the mailbox
	// uninteresting.
	MailboxAttrUnmarked MailboxAttr = "\\Unmarked"
)

// Child attributes. RFC 3348, also folded into RFC 9051.
const (
	// MailboxAttrHasChildren means the mailbox has at least one child.
	MailboxAttrHasChildren MailboxAttr = "\\HasChildren"
	// MailboxAttrHasNoChildren means the mailbox has no children.
	MailboxAttrHasNoChildren MailboxAttr = "\\HasNoChildren"
)

// Extended LIST attributes. RFC 5258, also folded into RFC 9051.
const (
	// MailboxAttrNonExistent means the name does not refer to an existing
	// mailbox, although it may still be subscribed.
	MailboxAttrNonExistent MailboxAttr = "\\NonExistent"
	// MailboxAttrSubscribed means the mailbox is subscribed.
	MailboxAttrSubscribed MailboxAttr = "\\Subscribed"
	// MailboxAttrRemote means the mailbox lives on another server.
	MailboxAttrRemote MailboxAttr = "\\Remote"
)

// Special-use attributes. RFC 6154, plus RFC 8457 for \Important.
const (
	// MailboxAttrAll is a virtual mailbox holding every message.
	MailboxAttrAll MailboxAttr = "\\All"
	// MailboxAttrArchive holds archived messages.
	MailboxAttrArchive MailboxAttr = "\\Archive"
	// MailboxAttrDrafts holds draft messages.
	MailboxAttrDrafts MailboxAttr = "\\Drafts"
	// MailboxAttrFlagged is a virtual mailbox holding flagged messages.
	MailboxAttrFlagged MailboxAttr = "\\Flagged"
	// MailboxAttrJunk holds messages classified as junk.
	MailboxAttrJunk MailboxAttr = "\\Junk"
	// MailboxAttrSent holds messages the user has sent.
	MailboxAttrSent MailboxAttr = "\\Sent"
	// MailboxAttrTrash holds messages the user has deleted.
	MailboxAttrTrash MailboxAttr = "\\Trash"
	// MailboxAttrImportant is a virtual mailbox holding messages considered
	// important. RFC 8457.
	MailboxAttrImportant MailboxAttr = "\\Important"
)

// Equal reports whether a and other denote the same attribute, comparing
// case-insensitively.
func (a MailboxAttr) Equal(other MailboxAttr) bool {
	return strings.EqualFold(string(a), string(other))
}

// ContainsAttr reports whether attrs contains a, comparing with
// [MailboxAttr.Equal].
func ContainsAttr(attrs []MailboxAttr, a MailboxAttr) bool {
	for _, b := range attrs {
		if b.Equal(a) {
			return true
		}
	}
	return false
}
