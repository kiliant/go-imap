package imap

// This file holds the vocabulary of the extended search family — ESEARCH
// (RFC 4731), PARTIAL (RFC 9394), SORT (RFC 5256) and THREAD (RFC 5256) — as
// opposed to search.go, which holds the search *criteria* tree.

// ESearchReturnKey names one item of ESEARCH return data. ESEARCH, RFC 4731
// section 3.1.
//
// It is a string-backed open type, and it is deliberately distinct from the
// keyword type naming a requested RETURN option: the request and response
// vocabularies are not the same set. MODSEQ (RFC 4731 section 3.2) is returned
// but never requested.
type ESearchReturnKey string

// ESEARCH return data items this library models.
const (
	ESearchReturnKeyMin       ESearchReturnKey = "MIN"
	ESearchReturnKeyMax       ESearchReturnKey = "MAX"
	ESearchReturnKeyAll       ESearchReturnKey = "ALL"
	ESearchReturnKeyCount     ESearchReturnKey = "COUNT"
	ESearchReturnKeyModSeq    ESearchReturnKey = "MODSEQ"
	ESearchReturnKeyPartial   ESearchReturnKey = "PARTIAL"
	ESearchReturnKeyRelevancy ESearchReturnKey = "RELEVANCY"
)

// PartialRange selects a slice of SEARCH or FETCH results by 1-based index into
// the result set, not into mailbox sequence numbers. PARTIAL, RFC 9394.
//
// Exactly one of the two forms is used. FirstStart:FirstEnd selects from the
// oldest matching result (wire form N:M). LastStart:LastEnd selects from the
// newest (wire form -N:-M, so LastStart is the magnitude of the first index and
// 1 is the newest). A zero end with a non-zero start is invalid — both ends are
// mandatory.
//
// Construct with keyed fields only; fields may be added in a future release.
type PartialRange struct {
	// FirstStart and FirstEnd select results counted from the oldest match.
	// Both must be non-zero together.
	FirstStart uint32
	FirstEnd   uint32

	// LastStart and LastEnd select results counted from the newest match.
	// Both must be non-zero together.
	LastStart uint32
	LastEnd   uint32

	_ struct{}
}

// PartialSearchData is the PARTIAL item of an ESEARCH response. PARTIAL,
// RFC 9394 section 3.1.
//
// Construct with keyed fields only; fields may be added in a future release.
type PartialSearchData struct {
	// Range is the requested range, echoed in the response.
	Range PartialRange

	// All holds matching sequence numbers when the command was not UID.
	All SeqSet
	// AllUIDs holds matching UIDs when the command was UID SEARCH.
	AllUIDs UIDSet

	// HasResults is false when NIL was reported for the range: no results fall
	// in that window. It distinguishes an empty page from a missing item.
	HasResults bool

	_ struct{}
}

// SortKey is one sort key of a SORT command. SORT, RFC 5256 section 3;
// SORT=DISPLAY, RFC 5957.
//
// It is a string-backed open type: DISPLAYFROM and DISPLAYTO (RFC 5957) are
// constants alongside the RFC 5256 keys, and a key this library does not model
// can be named by converting a string.
type SortKey string

// Sort keys. RFC 5256 section 3 and RFC 5957.
const (
	SortKeyArrival SortKey = "ARRIVAL"
	SortKeyCc      SortKey = "CC"
	SortKeyDate    SortKey = "DATE"
	SortKeyFrom    SortKey = "FROM"
	SortKeySize    SortKey = "SIZE"
	SortKeySubject SortKey = "SUBJECT"
	SortKeyTo      SortKey = "TO"

	// SortKeyDisplayFrom sorts by the displayed From name. SORT=DISPLAY,
	// RFC 5957.
	SortKeyDisplayFrom SortKey = "DISPLAYFROM"
	// SortKeyDisplayTo sorts by the displayed To name. SORT=DISPLAY,
	// RFC 5957.
	SortKeyDisplayTo SortKey = "DISPLAYTO"

	// SortKeyRelevancy sorts by FUZZY search relevancy, highest first.
	// SEARCH=FUZZY, RFC 6203 section 6. It requires a FUZZY search key in the
	// same command.
	SortKeyRelevancy SortKey = "RELEVANCY"
)

// SortKeySpec is one entry of the SORT key list, optionally reversed.
// RFC 5256 section 3.
//
// Construct with keyed fields only; fields may be added in a future release.
type SortKeySpec struct {
	Key     SortKey
	Reverse bool
	_       struct{}
}

// ThreadAlgorithm names a THREAD algorithm. THREAD, RFC 5256 section 4.
//
// It is a string-backed open type: ORDEREDSUBJECT and REFERENCES are constants,
// and a future algorithm registers as THREAD=<name> without changing this API.
type ThreadAlgorithm string

// Thread algorithms. RFC 5256 section 4.
const (
	ThreadOrderedSubject ThreadAlgorithm = "ORDEREDSUBJECT"
	ThreadReferences     ThreadAlgorithm = "REFERENCES"
)

// ThreadNode is one node of a THREAD response tree. Children are replies in the
// algorithm's order. THREAD, RFC 5256 section 4.
//
// Num is a sequence number for THREAD and a UID for UID THREAD; which one is
// carried by [ThreadData.UID]. Num is zero only for the anonymous container of
// the ((a)(b)) grouping, which has no message of its own.
//
// Construct with keyed fields only; fields may be added in a future release.
type ThreadNode struct {
	Num      uint32
	Children []ThreadNode
	_        struct{}
}

// ThreadData is the content of an untagged THREAD response. RFC 5256 section 4.
//
// Construct with keyed fields only; fields may be added in a future release.
type ThreadData struct {
	// Roots is the forest of thread trees. Empty when nothing matched.
	Roots []ThreadNode

	// UID reports whether every Num is a UID rather than a sequence number.
	UID bool

	_ struct{}
}

// ESearchData is the content of an untagged ESEARCH response, or a
// reconstruction of one. ESEARCH, RFC 4731 section 3.1.
//
// Each modelled item has a companion Has field, because RFC 4731 section 3.1
// distinguishes "absent" from "zero". MIN, MAX and ALL are omitted from the
// response entirely when nothing matched, while COUNT is always present and is
// then zero. Reading Min as 0 without checking HasMin therefore cannot tell an
// empty result from a match at message 0, which does not exist.
//
// Construct with keyed fields only; fields may be added in a future release.
type ESearchData struct {
	// Tag is the tag of the command this response correlates with, from the
	// search-correlator of RFC 4466 section 2.6. It is empty when no
	// correlator is carried, and when the data was reconstructed rather than
	// decoded.
	Tag string

	// UID reports whether every number in this response is a UID rather than
	// a sequence number. RFC 4731 section 3.1 requires an extended UID SEARCH
	// to set the UID indicator.
	UID bool

	// Min is the lowest matching number. Valid only when HasMin is set.
	Min    uint32
	HasMin bool

	// Max is the highest matching number. Valid only when HasMax is set.
	Max    uint32
	HasMax bool

	// Count is the number of matching messages. Valid only when HasCount is
	// set; a set HasCount with Count zero is a genuine empty result.
	Count    uint32
	HasCount bool

	// All holds every matching sequence number when UID is false. Valid only
	// when HasAll is set.
	All SeqSet

	// AllUIDs holds every matching UID when UID is true. Valid only when
	// HasAll is set. The two address spaces are separate fields for the same
	// reason sequence numbers and UIDs are separate types: conflating them
	// silently operates on the wrong messages.
	AllUIDs UIDSet

	HasAll bool

	// ModSeq is the modification sequence of the MODSEQ return item, which
	// CONDSTORE adds when the criteria mention MODSEQ. RFC 4731 section 3.2.
	// Valid only when HasModSeq is set.
	ModSeq    uint64
	HasModSeq bool

	// Values preserves every return item verbatim, keyed by the spelling used
	// on the wire, upper-cased. Items this library does not model are kept
	// here in raw form rather than dropped, because losing data for an
	// extension it has never heard of is worse than not understanding it.
	// Modelled items appear here too, so the unparsed text is always readable.
	Values map[ESearchReturnKey]string

	// Emulated reports that the value was computed from an ordinary SEARCH
	// response rather than decoded from an ESEARCH one, because the peer does
	// not advertise ESEARCH.
	//
	// It is a client-side observation: it describes how a decoder obtained the
	// value, and a producer has nothing to say with it. A server-produced
	// value leaves it false. It is kept on the shared type rather than split
	// off, because unlike [MailboxStatus.UIDValidityChanged] it appears in no
	// server-facing contract, and splitting it would divide the exported
	// helpers that read this type between two spellings for no gain.
	Emulated bool

	_ struct{}
}

// MultiSearchResult is one ESEARCH response of a multimailbox search: the
// per-mailbox result, tagged with the mailbox it came from. MULTISEARCH,
// RFC 7377 section 2.1.
//
// Construct with keyed fields only; fields may be added in a future release.
type MultiSearchResult struct {
	// Tag is the command tag correlator.
	Tag string

	// Mailbox is the mailbox this response refers to.
	Mailbox string

	// UIDValidity is the UIDVALIDITY of Mailbox.
	UIDValidity uint32

	// Data holds the return items. Data.UID is always true: RFC 7377
	// section 2.1 requires multimailbox responses to use UIDs.
	Data ESearchData

	_ struct{}
}

// MultiSearchData collects every per-mailbox ESEARCH response of a multimailbox
// search. MULTISEARCH, RFC 7377.
//
// Construct with keyed fields only; fields may be added in a future release.
type MultiSearchData struct {
	Results []MultiSearchResult
	_       struct{}
}

// SortData is the content of an untagged SORT response: the matching messages
// in sort order. SORT, RFC 5256 section 4.
//
// The order is the payload, so this is a slice rather than a [SeqSet] or
// [UIDSet], which are unordered sets and would destroy it.
//
// Construct with keyed fields only; fields may be added in a future release.
type SortData struct {
	// SeqNums carries the order for SORT; empty for UID SORT.
	SeqNums []SeqNum
	// UIDs carries the order for UID SORT; empty for sequence SORT.
	UIDs []UID

	// Emulated reports that the order was computed locally rather than by the
	// peer, because the peer does not advertise SORT. Like
	// [ESearchData.Emulated] it is a client-side observation, and a
	// server-produced value leaves it false.
	Emulated bool

	_ struct{}
}

// IDData is the peer identification of an untagged ID response. ID, RFC 2971.
//
// Fields is nil for ID NIL. A non-nil, possibly empty, slice is a parameter
// list, and the two are different on the wire.
//
// Construct with keyed fields only; fields may be added in a future release.
type IDData struct {
	// Received reports that an untagged ID response was seen at all, which
	// RFC 2971 section 3.1 makes optional. It is a client-side observation
	// about the exchange rather than about the data; a producer decides
	// whether to send the response instead, and leaves this false.
	Received bool

	// Fields is the parameter list.
	Fields []IDField

	_ struct{}
}
