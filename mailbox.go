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

// ListData describes one mailbox, as reported by an untagged LIST or LSUB
// response. RFC 3501 section 7.2.2, RFC 9051 section 7.3.1.
//
// A zero Delimiter means NIL: this mailbox has no hierarchy delimiter. Consumers
// must use this server-provided value rather than assuming '/', '.', or '\\',
// and a producer must report the delimiter its namespace actually uses.
//
// Construct with keyed fields only; fields may be added in a future release.
type ListData struct {
	Attrs     []MailboxAttr
	Delimiter rune
	Mailbox   string
	_         struct{}
}

// MailboxStatus is the state of a mailbox as reported while selecting it: the
// untagged responses and response codes of a SELECT or EXAMINE. RFC 3501
// section 6.3.1, RFC 9051 section 6.3.2.
//
// Every field here is mailbox state that both protocol directions can express.
// A client-side observation derived by comparing this against a cache — "the
// UIDVALIDITY differs from the one I last saw" — is deliberately not here: no
// server can produce it, and a field one side can never fill is evidence the
// shared type has the wrong boundary. The client carries such observations on
// its own type.
//
// Construct with keyed fields only; fields may be added in a future release.
type MailboxStatus struct {
	// Mailbox is the name of the selected mailbox.
	Mailbox string

	// Flags are the flags defined in the mailbox, and PermanentFlags those a
	// client may change persistently. A nil PermanentFlags means no
	// PERMANENTFLAGS response code was reported, which is not the same as an
	// empty one.
	Flags          []Flag
	PermanentFlags []Flag

	// NumMessages is the EXISTS count and NumRecent the RECENT count.
	// RFC 9051 removes RECENT from IMAP4rev2.
	NumMessages uint32
	NumRecent   uint32

	// UIDNext is the UID predicted for the next message appended, or zero if
	// it was not reported. UIDValidity is the mailbox's UID validity value.
	UIDNext     UID
	UIDValidity uint32

	// Unseen is the sequence number of the first unseen message from the
	// UNSEEN response code, or zero if none was reported. It is a sequence
	// number, not a count; the STATUS item of the same name is a count.
	Unseen uint32

	// HighestModSeq is the highest modification sequence in the mailbox.
	// CONDSTORE, RFC 7162 section 3.1.2.2. Zero means it was not reported,
	// which a NOMODSEQ mailbox must signal separately.
	HighestModSeq uint64

	// ReadOnly reports that the mailbox was opened read-only: EXAMINE, or
	// SELECT answered with the READ-ONLY response code.
	ReadOnly bool

	_ struct{}
}

// NamespaceDescriptor describes one namespace prefix and its hierarchy
// delimiter. A zero Delimiter means NIL. RFC 2342.
//
// Construct with keyed fields only; fields may be added in a future release.
type NamespaceDescriptor struct {
	Prefix    string
	Delimiter rune
	_         struct{}
}

// NamespaceData is the content of a NAMESPACE response: the personal, other
// users' and shared namespaces. RFC 2342.
//
// A nil group is NIL on the wire — the server has no namespace of that class —
// which is distinct from a present but empty list.
//
// Construct with keyed fields only; fields may be added in a future release.
type NamespaceData struct {
	Personal   []NamespaceDescriptor
	OtherUsers []NamespaceDescriptor
	Shared     []NamespaceDescriptor
	_          struct{}
}
