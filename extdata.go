package imap

// This file holds the response and command data of extensions whose vocabulary
// is a single small type or two — ID, INPROGRESS, JMAPACCESS, LANGUAGE,
// MESSAGELIMIT, LOGIN-REFERRALS/MAILBOX-REFERRALS and URLAUTH. Extensions with
// a larger vocabulary get their own file.

// IDField is one name/value pair exchanged by the ID command. ID, RFC 2971.
//
// Name is required. Value is nil when the wire value is NIL, which is distinct
// from an empty string. Field names are open-ended: RFC 2971 section 3.3 defines
// conventional names (name, version, os, …) and either peer may send others
// without an API change.
//
// Construct with keyed fields only; fields may be added in a future release.
type IDField struct {
	Name  string
	Value *string
	_     struct{}
}

// InProgressData is one INPROGRESS progress notification: the arguments of an
// INPROGRESS response code together with the text of the untagged OK carrying
// it. RFC 9585.
//
// Construct with keyed fields only; fields may be added in a future release.
type InProgressData struct {
	// Tag is the tag of the command being reported on, or "" for NIL (or when
	// the detail list is omitted entirely).
	Tag string

	// Progress is the number of items processed so far. Valid only when
	// HasProgress is set. RFC 9585 requires a non-negative value.
	Progress    uint32
	HasProgress bool

	// Goal is the expected total. Valid only when HasGoal is set. When set it
	// must be strictly greater than Progress; RFC 9585 makes a violation a
	// notification to disregard rather than a protocol error.
	Goal    uint32
	HasGoal bool

	// Text is the human-readable text accompanying the untagged OK, when it is
	// known. It is empty when only the response-code arguments are present.
	Text string

	_ struct{}
}

// JMAPAccessData is the content of a JMAPACCESS response code: the JMAP session
// resource that serves the same mailstore as this IMAP account. JMAPACCESS,
// RFC 9698.
//
// Construct with keyed fields only; fields may be added in a future release.
type JMAPAccessData struct {
	SessionURL string
	_          struct{}
}

// LanguageData is the content of one untagged LANGUAGE response. RFC 5255
// section 3.3.
//
// When a language is being selected, Tags has a single entry. When the supported
// languages are being enumerated, Tags lists them all.
//
// Construct with keyed fields only; fields may be added in a future release.
type LanguageData struct {
	Tags []string
	_    struct{}
}

// ComparatorData is the content of one untagged COMPARATOR response. RFC 5255
// section 4.8.
//
// Construct with keyed fields only; fields may be added in a future release.
type ComparatorData struct {
	// Active is the comparator now in use.
	Active string

	// Matching lists the further comparators that matched the request, when
	// they are reported.
	Matching []string

	_ struct{}
}

// MessageLimitPartial is the content of a MESSAGELIMIT response code:
// "MESSAGELIMIT <limit> [<uid>]". MESSAGELIMIT, RFC 9738 section 3.1.
//
// Construct with keyed fields only; fields may be added in a future release.
type MessageLimitPartial struct {
	// Limit is the advertised ceiling that was hit.
	Limit uint32

	// LowestUID is the lowest processed UID, when one is supplied.
	// HasLowestUID distinguishes "omitted" from UID 0, which is illegal.
	LowestUID    UID
	HasLowestUID bool

	_ struct{}
}

// ReferralData is the content of a REFERRAL response code. LOGIN-REFERRALS
// (RFC 2221) and MAILBOX-REFERRALS (RFC 2193) both use the same code; the IMAP
// URL says where to reconnect, or which mailbox is elsewhere.
//
// Construct with keyed fields only; fields may be added in a future release.
type ReferralData struct {
	// URL is the IMAP URL from the response code, verbatim. It is kept in raw
	// form so that URL parameters this library does not model survive in both
	// directions; parse it with net/url or an IMAP-URL helper.
	URL string
	_   struct{}
}

// GenURLAuthData is the content of an untagged GENURLAUTH response. URLAUTH,
// RFC 4467 section 3.1.
//
// Construct with keyed fields only; fields may be added in a future release.
type GenURLAuthData struct {
	// URLs are the URLAUTH-authorized URLs, in response order.
	URLs []string
	_    struct{}
}

// URLFetchItem is one URL/body pair of an untagged URLFETCH response. URLAUTH,
// RFC 4467 section 3.2.
//
// Body is nil for NIL, meaning the URL could not be fetched. An empty string is
// a present, empty body and is a different thing.
//
// Construct with keyed fields only; fields may be added in a future release.
type URLFetchItem struct {
	URL  string
	Body *string
	_    struct{}
}
