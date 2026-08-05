package imap

import (
	"io"
	"time"
)

// FetchItem is one item requested by a FETCH command: a message data item name
// or a body section specification. RFC 3501 section 6.4.5, RFC 9051
// section 6.4.5.
//
// FetchItem is a marker interface with an unexported method. That makes the set
// open to this library and closed to external implementers, which is what lets
// a new extension add a fetch item — MODSEQ (RFC 7162), EMAILID and THREADID
// (RFC 8474), SAVEDATE (RFC 8514), PREVIEW (RFC 8970), BINARY (RFC 3516) — as a
// new type, without altering any existing type or breaking any existing caller.
// A closed enumeration, or an options struct of bool fields, could not do that
// and cannot express BODY[HEADER.FIELDS (From To)] at all.
//
// The implementations are [FetchItemKeyword] for items that are a bare name,
// and one struct type per item that carries arguments. Note that those structs
// contain slices, so a FetchItem is not necessarily comparable: it must not be
// used as a map key, and code keying returned data by requested item must key
// on something else, such as the encoded item name.
//
// Callers can name an item this library does not model by converting a string
// to [FetchItemKeyword].
type FetchItem interface {
	fetchItem()
}

// FetchItemKeyword is a fetch item that is a bare name with no arguments, such
// as FLAGS or UID.
//
// Converting an arbitrary string yields a fetch item for an extension this
// library does not model yet; the value must be a valid IMAP atom, and the
// encoder rejects it if it is not. This is the caller-side escape hatch that
// keeps the library useful against a server implementing an RFC newer than the
// library.
type FetchItemKeyword string

func (FetchItemKeyword) fetchItem() {}

// Fetch items defined by the base protocol. RFC 3501 section 6.4.5, RFC 9051
// section 6.4.5.
const (
	// FetchItemUID requests the message's unique identifier.
	FetchItemUID FetchItemKeyword = "UID"
	// FetchItemFlags requests the message's flags.
	FetchItemFlags FetchItemKeyword = "FLAGS"
	// FetchItemInternalDate requests the server's internal date for the
	// message.
	FetchItemInternalDate FetchItemKeyword = "INTERNALDATE"
	// FetchItemRFC822Size requests the message's size in octets.
	FetchItemRFC822Size FetchItemKeyword = "RFC822.SIZE"
	// FetchItemEnvelope requests the message envelope. See [Envelope].
	FetchItemEnvelope FetchItemKeyword = "ENVELOPE"
)

// Legacy fetch items retained by RFC 3501 and removed by RFC 9051 section 6.4.5
// in favour of the equivalent body sections. They are provided so that a client
// speaking to an old server can use them; prefer the body-section forms.
const (
	// FetchItemRFC822 is equivalent to BODY[].
	FetchItemRFC822 FetchItemKeyword = "RFC822"
	// FetchItemRFC822Header is equivalent to BODY.PEEK[HEADER].
	FetchItemRFC822Header FetchItemKeyword = "RFC822.HEADER"
	// FetchItemRFC822Text is equivalent to BODY[TEXT].
	FetchItemRFC822Text FetchItemKeyword = "RFC822.TEXT"
)

// Fetch items added by extensions. Each is a value of the same open type, added
// without changing anything above it.
const (
	// FetchItemModSeq requests the message's modification sequence.
	// CONDSTORE, RFC 7162 section 3.1.
	FetchItemModSeq FetchItemKeyword = "MODSEQ"
	// FetchItemEmailID requests the server-assigned, immutable identifier
	// of the message's content. OBJECTID, RFC 8474 section 5.
	FetchItemEmailID FetchItemKeyword = "EMAILID"
	// FetchItemThreadID requests the server-assigned identifier of the
	// thread the message belongs to. OBJECTID, RFC 8474 section 5.
	FetchItemThreadID FetchItemKeyword = "THREADID"
	// FetchItemSaveDate requests the date the message was saved into the
	// mailbox. SAVEDATE, RFC 8514 section 2.
	FetchItemSaveDate FetchItemKeyword = "SAVEDATE"
)

// FetchItemBodyStructure requests the MIME structure of the message.
// RFC 3501 section 6.4.5.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchItemBodyStructure struct {
	// Extended selects BODYSTRUCTURE, which includes the extension data of
	// [BodyStructureSinglePartExt] and [BodyStructureMultiPartExt], over
	// BODY, which does not.
	Extended bool

	_ struct{}
}

func (*FetchItemBodyStructure) fetchItem() {}

// PartSpecifier names the portion of a message or body part that a
// [FetchItemBodySection] refers to. RFC 3501 section 6.4.5, production
// section-msgtext.
//
// It is a string-backed named type rather than an enumeration so that a future
// specifier is a new constant.
type PartSpecifier string

// Part specifiers. RFC 3501 section 6.4.5.
const (
	// PartSpecifierNone selects the entire part, header and body.
	PartSpecifierNone PartSpecifier = ""
	// PartSpecifierHeader selects the RFC 5322 header of the part.
	PartSpecifierHeader PartSpecifier = "HEADER"
	// PartSpecifierMIME selects the MIME header of a nested part. It is
	// only valid with a non-empty part number.
	PartSpecifierMIME PartSpecifier = "MIME"
	// PartSpecifierText selects the body of the part, without its header.
	PartSpecifierText PartSpecifier = "TEXT"
)

// SectionPartial restricts a body section to a byte range, the <p.n> suffix of
// RFC 3501 section 6.4.5.
//
// Construct with keyed fields only; fields may be added in a future release.
type SectionPartial struct {
	// Offset is the zero-based octet offset of the first octet wanted.
	Offset int64
	// Size is the maximum number of octets wanted. A server may return
	// fewer, and returns none if Offset is past the end of the section.
	Size int64

	_ struct{}
}

// FetchItemBodySection requests a body section: BODY[...] or BODY.PEEK[...],
// optionally restricted to a byte range. RFC 3501 section 6.4.5.
//
// The zero value requests BODY[], the whole message, and sets the \Seen flag
// because Peek is false — see [FetchItemBodySection.Peek].
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchItemBodySection struct {
	// Part is the body part number, the sequence of 1-based indices that
	// addresses a nested part. Empty selects the message itself.
	Part []int

	// Specifier selects header, text or the whole part. See
	// [PartSpecifier].
	Specifier PartSpecifier

	// HeaderFields, when non-empty, requests HEADER.FIELDS: only the named
	// header fields are returned. It requires Specifier to be
	// [PartSpecifierHeader], and is mutually exclusive with
	// HeaderFieldsNot.
	HeaderFields []string

	// HeaderFieldsNot, when non-empty, requests HEADER.FIELDS.NOT: every
	// header field except those named is returned. It requires Specifier to
	// be [PartSpecifierHeader], and is mutually exclusive with
	// HeaderFields.
	HeaderFieldsNot []string

	// Partial restricts the result to a byte range, or nil for the whole
	// section.
	Partial *SectionPartial

	// Peek selects BODY.PEEK[...] over BODY[...].
	//
	// This is a side effect, not a formatting detail: fetching BODY[...]
	// sets the \Seen flag on the message, and BODY.PEEK[...] does not. A
	// client that reads a message without intending to mark it read must
	// set Peek.
	Peek bool

	_ struct{}
}

func (*FetchItemBodySection) fetchItem() {}

// FetchItemBinarySection requests a body section with its content-transfer
// encoding already removed by the server: BINARY[...] or BINARY.PEEK[...].
// BINARY, RFC 3516 section 4.2.
//
// This is an extension item. It exists as its own type, added alongside the
// items above rather than in place of them, which is the property the open
// [FetchItem] set is designed to have.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchItemBinarySection struct {
	// Part is the body part number; empty selects the whole message.
	Part []int

	// Partial restricts the result to a byte range of the *decoded* data,
	// or nil for the whole section.
	Partial *SectionPartial

	// Peek selects BINARY.PEEK[...], which does not set \Seen. See
	// [FetchItemBodySection.Peek].
	Peek bool

	_ struct{}
}

func (*FetchItemBinarySection) fetchItem() {}

// FetchItemBinarySectionSize requests the size in octets of a body section
// after its content-transfer encoding has been removed: BINARY.SIZE[...].
// BINARY, RFC 3516 section 4.2.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchItemBinarySectionSize struct {
	// Part is the body part number; empty selects the whole message.
	Part []int

	_ struct{}
}

func (*FetchItemBinarySectionSize) fetchItem() {}

// FetchItemPreview requests a short textual preview of the message. PREVIEW,
// RFC 8970 section 5.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchItemPreview struct {
	// Lazy asks the server to return a preview only if one is already
	// available, rather than generating it. RFC 8970 section 5, the LAZY
	// modifier.
	Lazy bool

	_ struct{}
}

func (*FetchItemPreview) fetchItem() {}

// FetchDataKey is the item name of one value in a FETCH response. It includes
// any section specifier and partial origin, for example "FLAGS", "BODY[1]" or
// "BODY[TEXT]<1024>".
//
// It is string-backed and open-ended because servers may return response items
// introduced by extensions this version of the library does not know. Such a
// key is preserved verbatim rather than folded into an "unknown" bucket; see
// [FetchDataRaw]. Item names are case-insensitive on the wire, but preserving
// their spelling prevents data loss and makes diagnostics faithful.
type FetchDataKey string

// FetchData is one typed value in a FETCH response. RFC 3501 section 7.4.2,
// RFC 9051 section 7.5.2.
//
// Like [FetchItem], this marker interface is open to this library and closed to
// external implementers. A future RFC adds a FetchData implementation without
// changing [FetchMessageData] or any existing value type. Values from an
// extension this library does not yet model are represented by [*FetchDataRaw]
// under their original key.
type FetchData interface {
	fetchData()
}

// FetchMessageData is the data reported for one message by one or more FETCH
// responses. RFC 3501 section 7.4.2, RFC 9051 section 7.5.2.
//
// Items is keyed rather than a fixed struct because FETCH response data grows
// with every extension. The slice preserves repeated item names instead of
// silently overwriting one; values for each key are in wire order. A response
// item the library does not recognise is retained as [*FetchDataRaw].
//
// A server may split one message across multiple untagged FETCH responses. The
// client merges those responses by appending values to Items, so callers must
// not assume one response or one value per key.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchMessageData struct {
	// SeqNum is the message's sequence number at the time of the response.
	SeqNum SeqNum

	// Items maps the item spelling sent by the server to all values sent for
	// that item, in order. The map and its slices are owned by this value.
	Items map[FetchDataKey][]FetchData

	_ struct{}
}

// FetchDataUID is a message unique identifier returned for the UID item.
type FetchDataUID UID

func (FetchDataUID) fetchData() {}

// FetchDataFlags is the complete flag list returned for the FLAGS item. The
// slice is owned by the [FetchMessageData] containing it.
type FetchDataFlags []Flag

func (FetchDataFlags) fetchData() {}

// FetchDataInternalDate is the date and time returned for INTERNALDATE.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchDataInternalDate struct {
	Time time.Time

	_ struct{}
}

func (*FetchDataInternalDate) fetchData() {}

// FetchDataRFC822Size is the message size in octets returned for RFC822.SIZE.
type FetchDataRFC822Size int64

func (FetchDataRFC822Size) fetchData() {}

// FetchDataEnvelope is the parsed value returned for ENVELOPE.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchDataEnvelope struct {
	Envelope *Envelope

	_ struct{}
}

func (*FetchDataEnvelope) fetchData() {}

// FetchDataBodyStructure is the parsed value returned for BODY or
// BODYSTRUCTURE. The key in [FetchMessageData.Items] distinguishes the two.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchDataBodyStructure struct {
	BodyStructure BodyStructure

	_ struct{}
}

func (*FetchDataBodyStructure) fetchData() {}

// FetchDataLiteral is the streaming value of a legacy RFC822, RFC822.HEADER or
// RFC822.TEXT response item. Prefer [FetchDataBodySection] for BODY sections.
//
// Literal reads exactly the item value. It must be consumed or explicitly
// closed when it also implements [io.Closer] before the client can decode the
// next response; abandoning it invalidates the connection rather than risking
// protocol desynchronisation.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchDataLiteral struct {
	Literal io.Reader

	_ struct{}
}

func (*FetchDataLiteral) fetchData() {}

// FetchDataBodySection is the streaming value of a BODY[...] response item.
// Its key in [FetchMessageData.Items] preserves the item spelling sent by the
// server, while these fields provide its parsed structure.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchDataBodySection struct {
	// Part is the body part number. Empty selects the message itself.
	Part []int

	// Specifier selects header, text, MIME header or the whole part.
	Specifier PartSpecifier

	// HeaderFields and HeaderFieldsNot preserve the field-list form of the
	// returned section, when present.
	HeaderFields    []string
	HeaderFieldsNot []string

	// Origin is the zero-based origin from a response partial, or zero when
	// no origin was present. HasOrigin distinguishes BODY[] from BODY[]<0>.
	Origin    int64
	HasOrigin bool

	// Literal reads exactly the section bytes. It has the same drain/close
	// requirement as [FetchDataLiteral.Literal].
	Literal io.Reader

	_ struct{}
}

func (*FetchDataBodySection) fetchData() {}

// FetchDataBinarySection is the streaming value of a BINARY[...] response
// item. BINARY, RFC 3516 section 4.3.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchDataBinarySection struct {
	// Part is the body part number. Empty selects the whole message.
	Part []int

	// Origin and HasOrigin preserve an optional response partial; see
	// [FetchDataBodySection.Origin].
	Origin    int64
	HasOrigin bool

	// Literal reads the decoded section bytes and must be drained or closed.
	Literal io.Reader

	_ struct{}
}

func (*FetchDataBinarySection) fetchData() {}

// FetchDataBinarySectionSize is the decoded size in octets returned for a
// BINARY.SIZE[...] item. BINARY, RFC 3516 section 4.3.
type FetchDataBinarySectionSize int64

func (FetchDataBinarySectionSize) fetchData() {}

// FetchDataModSeq is the modification sequence returned for MODSEQ. CONDSTORE,
// RFC 7162 section 3.1.
type FetchDataModSeq uint64

func (FetchDataModSeq) fetchData() {}

// FetchDataObjectID is an EMAILID or THREADID value. The item key distinguishes
// the two. OBJECTID, RFC 8474 section 5.
type FetchDataObjectID string

func (FetchDataObjectID) fetchData() {}

// FetchDataSaveDate is the value returned for SAVEDATE. Date is nil when the
// server returned NIL. SAVEDATE, RFC 8514 section 2.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchDataSaveDate struct {
	Date *time.Time

	_ struct{}
}

func (*FetchDataSaveDate) fetchData() {}

// FetchDataPreview is the value returned for PREVIEW. Text is nil when the
// server returned NIL, which is distinct from an empty preview. PREVIEW, RFC
// 8970 section 5.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchDataPreview struct {
	Text *string

	_ struct{}
}

func (*FetchDataPreview) fetchData() {}

// FetchDataRaw preserves a FETCH response item value this version of the
// library does not recognise. Its map key is the item name exactly as the
// server sent it; Reader yields the exact wire bytes of the corresponding
// value, excluding the item name and its separating whitespace.
//
// An unknown extension cannot force an unbounded allocation: a value too large
// to hold in memory is consumed from the connection but not retained, and
// Reader is then empty. An empty Reader therefore does not distinguish an empty
// value from one that was too large to keep.
//
// Reader is owned by this value. Treat it as valid only until the command ends:
// drain it, or close it when it also implements [io.Closer], before the client
// decodes the next response, and copy the bytes if they must outlive the
// command. The current implementation buffers, so a reader often survives
// longer than that — do not rely on it. These obligations are what keep the
// library free to stream this value in a future release without breaking
// callers.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchDataRaw struct {
	Reader io.Reader

	_ struct{}
}

func (*FetchDataRaw) fetchData() {}
