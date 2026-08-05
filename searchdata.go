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
