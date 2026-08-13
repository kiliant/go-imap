package imap

import "time"

// SearchCriteria is one search key of a SEARCH command. RFC 3501 section 6.4.4,
// RFC 9051 section 6.4.4.
//
// SearchCriteria is a marker interface with an unexported method, so the set of
// search keys is open to this library and closed to external implementers. Each
// extension that adds a key — WITHIN (RFC 5032), CONDSTORE (RFC 7162), OBJECTID
// (RFC 8474), SAVEDATE (RFC 8514), SEARCH=FUZZY (RFC 6203), SEARCHRES
// (RFC 5182) — adds a new value or a new type here, and changes nothing that
// already exists.
//
// Keys compose. IMAP conjoins space-separated keys, which [SearchAnd]
// represents; [SearchOr] and [SearchNot] nest arbitrarily:
//
//	// (FROM "ann" OR (SUBJECT "urgent") (NOT SEEN)) SINCE 1-Jan-2026
//	crit := imap.SearchAnd{
//		imap.SearchString{Key: imap.SearchKeyFrom, Value: "ann"},
//		imap.SearchOr{
//			Left:  imap.SearchString{Key: imap.SearchKeySubject, Value: "urgent"},
//			Right: imap.SearchNot{Criteria: imap.SearchSeen},
//		},
//		imap.SearchDate{Key: imap.SearchDateKeySince, Date: cutoff},
//	}
//
// Some implementations carry slices, so a SearchCriteria is not necessarily
// comparable and must not be used as a map key.
//
// # Consumers
//
// The set being closed to external implementers does not make it closed to
// growth: a later release of this library may add a key, so code that switches
// over a SearchCriteria it did not build must expect a type it has never seen.
//
// Treat an unrecognised criterion as an error, never as "does not match". The
// two are indistinguishable to the client — an empty result reads as a correct
// search that found nothing — so a silent default turns a library upgrade into
// wrong answers with no symptom. Failing loudly is the whole point; the specific
// error matters less. The evaluator behind imapserver/memory returns a sentinel
// its callers can recognise rather than reporting no match.
//
// Backends are the main consumers, and they are given a narrower guarantee than
// this: the framework promises no SearchSeqNum and no [SearchFilter] ever
// reaches them. See imapserver.SearchQuery.Criteria and
// docs/API-STABILITY.md section 10.
type SearchCriteria interface {
	searchCriteria()
}

// SearchAnd is the conjunction of its elements: every criterion must match.
// RFC 3501 section 6.4.4 makes conjunction implicit in a list of search keys.
//
// An empty SearchAnd matches every message, like [SearchAll].
type SearchAnd []SearchCriteria

func (SearchAnd) searchCriteria() {}

// SearchOr is the disjunction of two criteria: the OR key of RFC 3501
// section 6.4.4.
//
// IMAP's OR is strictly binary. Nest to express more alternatives:
//
//	imap.SearchOr{Left: a, Right: imap.SearchOr{Left: b, Right: c}}
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchOr struct {
	Left  SearchCriteria
	Right SearchCriteria

	_ struct{}
}

func (SearchOr) searchCriteria() {}

// SearchNot negates a criterion: the NOT key of RFC 3501 section 6.4.4.
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchNot struct {
	Criteria SearchCriteria

	_ struct{}
}

func (SearchNot) searchCriteria() {}

// SearchKeyword is a search key that takes no argument, such as SEEN or
// UNANSWERED.
//
// It is a string-backed open type: an extension defining another argument-less
// key is a new constant here, and a caller can name a key this library does not
// model by converting a string.
type SearchKeyword string

func (SearchKeyword) searchCriteria() {}

// Argument-less search keys. RFC 3501 section 6.4.4.
const (
	// SearchAll matches every message in the mailbox.
	SearchAll SearchKeyword = "ALL"
	// SearchAnswered matches messages with \Answered.
	SearchAnswered SearchKeyword = "ANSWERED"
	// SearchDeleted matches messages with \Deleted.
	SearchDeleted SearchKeyword = "DELETED"
	// SearchDraft matches messages with \Draft.
	SearchDraft SearchKeyword = "DRAFT"
	// SearchFlagged matches messages with \Flagged.
	SearchFlagged SearchKeyword = "FLAGGED"
	// SearchNew matches messages with \Recent but not \Seen. Removed by
	// RFC 9051 along with \Recent.
	SearchNew SearchKeyword = "NEW"
	// SearchOld matches messages without \Recent. Removed by RFC 9051.
	SearchOld SearchKeyword = "OLD"
	// SearchRecent matches messages with \Recent. Removed by RFC 9051.
	SearchRecent SearchKeyword = "RECENT"
	// SearchSeen matches messages with \Seen.
	SearchSeen SearchKeyword = "SEEN"
	// SearchUnanswered matches messages without \Answered.
	SearchUnanswered SearchKeyword = "UNANSWERED"
	// SearchUndeleted matches messages without \Deleted.
	SearchUndeleted SearchKeyword = "UNDELETED"
	// SearchUndraft matches messages without \Draft.
	SearchUndraft SearchKeyword = "UNDRAFT"
	// SearchUnflagged matches messages without \Flagged.
	SearchUnflagged SearchKeyword = "UNFLAGGED"
	// SearchUnseen matches messages without \Seen.
	SearchUnseen SearchKeyword = "UNSEEN"

	// SearchSaveDateSupported matches every message in a mailbox that
	// records save dates, and no message otherwise. SAVEDATE, RFC 8514
	// section 2.
	SearchSaveDateSupported SearchKeyword = "SAVEDATESUPPORTED"
)

// SearchFlagKeyword matches on a keyword flag: the KEYWORD and UNKEYWORD keys
// of RFC 3501 section 6.4.4.
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchFlagKeyword struct {
	// Flag is the keyword to match.
	Flag Flag
	// Not selects UNKEYWORD, matching messages that do not have the flag.
	Not bool

	_ struct{}
}

func (SearchFlagKeyword) searchCriteria() {}

// SearchHeaderField matches messages whose named header field contains Value as
// a substring, case-insensitively: the HEADER key of RFC 3501 section 6.4.4. An
// empty Value matches any message that has the field at all.
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchHeaderField struct {
	// Field is the header field name, without the colon.
	Field string
	// Value is the substring to look for.
	Value string

	_ struct{}
}

func (SearchHeaderField) searchCriteria() {}

// SearchStringKey names a search key that takes a single string argument and
// matches it as a substring. It is a string-backed open type.
type SearchStringKey string

// String-argument search keys. RFC 3501 section 6.4.4.
const (
	// SearchKeyBcc searches the Bcc header.
	SearchKeyBcc SearchStringKey = "BCC"
	// SearchKeyBody searches the message body.
	SearchKeyBody SearchStringKey = "BODY"
	// SearchKeyCc searches the Cc header.
	SearchKeyCc SearchStringKey = "CC"
	// SearchKeyFrom searches the From header.
	SearchKeyFrom SearchStringKey = "FROM"
	// SearchKeySubject searches the Subject header.
	SearchKeySubject SearchStringKey = "SUBJECT"
	// SearchKeyText searches the whole message, header and body.
	SearchKeyText SearchStringKey = "TEXT"
	// SearchKeyTo searches the To header.
	SearchKeyTo SearchStringKey = "TO"
)

// SearchString matches a substring against part of a message.
//
// Non-ASCII values require the server to have been told a charset; see the
// CHARSET argument of RFC 3501 section 6.4.4 and the UTF8=ACCEPT capability of
// RFC 9755. This library reports what the server supports rather than
// transcoding silently.
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchString struct {
	// Key selects what is searched. See [SearchStringKey].
	Key SearchStringKey
	// Value is the substring to look for, matched case-insensitively.
	Value string

	_ struct{}
}

func (SearchString) searchCriteria() {}

// SearchDateKey names a search key that takes a date argument. It is a
// string-backed open type: SAVEDATE (RFC 8514) added three constants below
// without any change to this type or to [SearchDate].
type SearchDateKey string

// Date search keys. The first six are RFC 3501 section 6.4.4; the internal-date
// keys compare against the server's arrival time and the SENT keys against the
// message's Date header. Comparison is by date only, ignoring time and zone.
const (
	// SearchDateKeyBefore matches messages whose internal date is earlier
	// than the given date.
	SearchDateKeyBefore SearchDateKey = "BEFORE"
	// SearchDateKeyOn matches messages whose internal date is the given
	// date.
	SearchDateKeyOn SearchDateKey = "ON"
	// SearchDateKeySince matches messages whose internal date is the given
	// date or later.
	SearchDateKeySince SearchDateKey = "SINCE"
	// SearchDateKeySentBefore matches on the Date header.
	SearchDateKeySentBefore SearchDateKey = "SENTBEFORE"
	// SearchDateKeySentOn matches on the Date header.
	SearchDateKeySentOn SearchDateKey = "SENTON"
	// SearchDateKeySentSince matches on the Date header.
	SearchDateKeySentSince SearchDateKey = "SENTSINCE"

	// SearchDateKeySavedBefore matches on the message's save date.
	// SAVEDATE, RFC 8514 section 2.
	SearchDateKeySavedBefore SearchDateKey = "SAVEDBEFORE"
	// SearchDateKeySavedOn matches on the message's save date. RFC 8514.
	SearchDateKeySavedOn SearchDateKey = "SAVEDON"
	// SearchDateKeySavedSince matches on the message's save date.
	// RFC 8514.
	SearchDateKeySavedSince SearchDateKey = "SAVEDSINCE"
)

// SearchDate matches a message date against a given date.
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchDate struct {
	// Key selects which date is compared and how. See [SearchDateKey].
	Key SearchDateKey
	// Date is the date to compare against. Only its calendar date in its
	// own location is significant.
	Date time.Time

	_ struct{}
}

func (SearchDate) searchCriteria() {}

// SearchSizeKey names a search key that takes a size argument. It is a
// string-backed open type.
type SearchSizeKey string

// Size search keys. RFC 3501 section 6.4.4.
const (
	// SearchSizeKeyLarger matches messages larger than the given size.
	SearchSizeKeyLarger SearchSizeKey = "LARGER"
	// SearchSizeKeySmaller matches messages smaller than the given size.
	SearchSizeKeySmaller SearchSizeKey = "SMALLER"
)

// SearchSize matches on the size of a message in octets.
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchSize struct {
	// Key selects the comparison. See [SearchSizeKey].
	Key SearchSizeKey
	// Size is the number of octets to compare against.
	Size int64

	_ struct{}
}

func (SearchSize) searchCriteria() {}

// SearchSeqNum matches messages by sequence number: the bare sequence-set
// search key of RFC 3501 section 6.4.4.
//
// It is a distinct type from [SearchUID] because addressing messages by
// sequence number where UIDs were meant, or the reverse, silently operates on
// the wrong messages. The type system prevents it.
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchSeqNum struct {
	Set SeqSet

	_ struct{}
}

func (SearchSeqNum) searchCriteria() {}

// SearchUID matches messages by unique identifier: the UID search key of
// RFC 3501 section 6.4.4. See [SearchSeqNum].
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchUID struct {
	Set UIDSet

	_ struct{}
}

func (SearchUID) searchCriteria() {}

// SearchSavedResult refers to the result of the previous SEARCH in this
// session, the "$" marker. SEARCHRES, RFC 5182 section 2.
//
// It is a search key here; used in place of a sequence set in FETCH, STORE,
// COPY or MOVE it is not a set value but a command option, so the commands that
// accept it expose it through their options struct rather than through
// [SeqSet]. That keeps [SeqSet] a plain set of numbers.
type SearchSavedResult struct{}

func (SearchSavedResult) searchCriteria() {}

// SearchWithinKey names a search key that takes an interval in seconds. It is a
// string-backed open type.
type SearchWithinKey string

// Interval search keys. WITHIN, RFC 5032 section 3.
const (
	// SearchWithinKeyOlder matches messages whose internal date is more
	// than the given number of seconds in the past.
	SearchWithinKeyOlder SearchWithinKey = "OLDER"
	// SearchWithinKeyYounger matches messages whose internal date is within
	// the given number of seconds.
	SearchWithinKeyYounger SearchWithinKey = "YOUNGER"
)

// SearchWithin matches on the age of a message. WITHIN, RFC 5032 section 3.
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchWithin struct {
	// Key selects the comparison. See [SearchWithinKey].
	Key SearchWithinKey
	// Seconds is the interval, relative to the moment the server evaluates
	// the search.
	Seconds int64

	_ struct{}
}

func (SearchWithin) searchCriteria() {}

// SearchObjectIDKey names a search key that takes an object identifier. It is a
// string-backed open type.
type SearchObjectIDKey string

// Object identifier search keys. OBJECTID, RFC 8474 section 6.
const (
	// SearchObjectIDKeyEmail matches the message with the given EMAILID.
	SearchObjectIDKeyEmail SearchObjectIDKey = "EMAILID"
	// SearchObjectIDKeyThread matches the messages with the given THREADID.
	SearchObjectIDKeyThread SearchObjectIDKey = "THREADID"
)

// SearchObjectID matches on a server-assigned object identifier. OBJECTID,
// RFC 8474 section 6.
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchObjectID struct {
	// Key selects which identifier is matched. See [SearchObjectIDKey].
	Key SearchObjectIDKey
	// Value is the identifier, as the server reported it.
	Value string

	_ struct{}
}

func (SearchObjectID) searchCriteria() {}

// SearchModSeqMetadata restricts a [SearchModSeq] to changes made to a
// particular kind of metadata item. CONDSTORE, RFC 7162 section 3.1.5.
//
// It is a string-backed open type.
type SearchModSeqMetadata string

// Metadata item types for [SearchModSeq]. CONDSTORE, RFC 7162 section 3.1.5.
const (
	// SearchModSeqMetadataPrivate restricts the match to per-user metadata.
	SearchModSeqMetadataPrivate SearchModSeqMetadata = "priv"
	// SearchModSeqMetadataShared restricts the match to shared metadata.
	SearchModSeqMetadataShared SearchModSeqMetadata = "shared"
	// SearchModSeqMetadataAll matches either.
	SearchModSeqMetadataAll SearchModSeqMetadata = "all"
)

// SearchModSeq matches messages whose modification sequence is at least
// ModSeq. CONDSTORE, RFC 7162 section 3.1.5.
//
// This is an extension key. It is a new type sitting beside the base keys, not
// a change to any of them, which is the property the open [SearchCriteria] set
// exists to guarantee.
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchModSeq struct {
	// ModSeq is the modification sequence to compare against. Zero matches
	// every message, per the mod-sequence-valzer production.
	ModSeq uint64

	// EntryName optionally restricts the match to changes to one metadata
	// entry, for example "/flags/\\draft". It must be set together with
	// EntryType, or both left zero.
	EntryName string

	// EntryType optionally restricts the match to one kind of metadata.
	EntryType SearchModSeqMetadata

	_ struct{}
}

func (SearchModSeq) searchCriteria() {}

// SearchFuzzy asks the server to match the wrapped criterion approximately
// rather than exactly. SEARCH=FUZZY, RFC 6203 section 6.
//
// Construct with keyed fields only; fields may be added in a future release.
type SearchFuzzy struct {
	Criteria SearchCriteria

	_ struct{}
}

func (SearchFuzzy) searchCriteria() {}

// SearchFilter names a server-side saved search filter, which stands in for the
// criteria the server stores under that name. FILTERS, RFC 5466 section 3.
//
// It is a named string type rather than a struct because a filter reference is
// exactly a name: the criteria it expands to live on the server, and a client
// never sees them. A server that does not know the name answers with the
// UNDEFINED-FILTER response code rather than an empty result, so a client can
// tell "no such filter" from "nothing matched".
type SearchFilter string

func (SearchFilter) searchCriteria() {}
