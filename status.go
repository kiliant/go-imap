package imap

// StatusItem is one item requested by a STATUS command. RFC 3501 section 6.3.10,
// RFC 9051 section 6.3.11.
//
// StatusItem is a marker interface with an unexported method, for the same
// reason as [FetchItem]: the set grows with almost every extension — SIZE
// (RFC 8438), MAILBOXID (RFC 8474), DELETED and DELETED-STORAGE (RFC 9208),
// HIGHESTMODSEQ (RFC 7162), APPENDLIMIT (RFC 7889) — and each of those must be
// expressible as a new value or a new type rather than a change to an existing
// one.
//
// Every status item defined so far is a bare atom, so [StatusItemKeyword]
// covers them all. The interface exists so that an extension defining an item
// that takes arguments — as PREVIEW did for FETCH — can be added as a new
// struct type implementing this interface, instead of forcing the item type
// itself to change from a string to something else, which would break every
// caller at once.
type StatusItem interface {
	statusItem()
}

// StatusItemKeyword is a status item that is a bare name. Converting an
// arbitrary string yields a status item for an extension this library does not
// model yet; the value must be a valid IMAP atom.
type StatusItemKeyword string

func (StatusItemKeyword) statusItem() {}

// Status items defined by the base protocol. RFC 3501 section 6.3.10,
// RFC 9051 section 6.3.11.
const (
	// StatusItemMessages requests the number of messages in the mailbox.
	StatusItemMessages StatusItemKeyword = "MESSAGES"
	// StatusItemUIDNext requests the UID that will be assigned to the next
	// message appended to the mailbox.
	StatusItemUIDNext StatusItemKeyword = "UIDNEXT"
	// StatusItemUIDValidity requests the mailbox's UID validity value.
	StatusItemUIDValidity StatusItemKeyword = "UIDVALIDITY"
	// StatusItemUnseen requests the number of messages without \Seen.
	StatusItemUnseen StatusItemKeyword = "UNSEEN"
	// StatusItemRecent requests the number of messages with \Recent.
	// RFC 9051 removes it from IMAP4rev2.
	StatusItemRecent StatusItemKeyword = "RECENT"
)

// Status items added by extensions. Each is a value of the same open type,
// added without changing anything above it.
const (
	// StatusItemHighestModSeq requests the highest modification sequence in
	// the mailbox. CONDSTORE, RFC 7162 section 3.1.2.
	StatusItemHighestModSeq StatusItemKeyword = "HIGHESTMODSEQ"
	// StatusItemAppendLimit requests the largest message the server will
	// accept into this mailbox. APPENDLIMIT, RFC 7889 section 4.
	StatusItemAppendLimit StatusItemKeyword = "APPENDLIMIT"
	// StatusItemSize requests the total size in octets of the mailbox.
	// STATUS=SIZE, RFC 8438 section 3.
	StatusItemSize StatusItemKeyword = "SIZE"
	// StatusItemMailboxID requests the server-assigned, immutable
	// identifier of the mailbox. OBJECTID, RFC 8474 section 4.
	StatusItemMailboxID StatusItemKeyword = "MAILBOXID"
	// StatusItemDeleted requests the number of messages with \Deleted.
	// QUOTA, RFC 9208 section 3.3.
	StatusItemDeleted StatusItemKeyword = "DELETED"
	// StatusItemDeletedStorage requests the storage, in kibibytes, that
	// expunging the \Deleted messages would reclaim. QUOTA, RFC 9208
	// section 3.3.
	StatusItemDeletedStorage StatusItemKeyword = "DELETED-STORAGE"
)

// StatusData is the content of an untagged STATUS response. RFC 3501
// section 7.2.4, RFC 9051 section 7.3.3.
//
// Values carries every item, including extension items this library gives no
// convenience field. Numeric values are held as uint64; string-valued
// extensions, including NIL-shaped items such as APPENDLIMIT, are held as
// strings. Prefer [StatusData.Number] over type-asserting Values for numeric
// items.
//
// The convenience fields are a projection of Values, not a replacement for it: a
// producer that fills only the fields reports no items at all, so a producer
// must populate Values for every item it means to send.
//
// Construct with keyed fields only; fields may be added in a future release.
type StatusData struct {
	Mailbox       string
	NumMessages   uint32
	UIDNext       UID
	UIDValidity   uint32
	NumUnseen     uint32
	NumRecent     uint32
	HighestModSeq uint64
	Values        map[StatusItemKeyword]any
	_             struct{}
}

// Number returns the numeric STATUS value for item. The second result reports
// whether a uint64 is present for that item, which is distinct from a present
// zero. Non-numeric values (for example APPENDLIMIT NIL held as a string)
// return false.
func (data *StatusData) Number(item StatusItemKeyword) (uint64, bool) {
	if data == nil || data.Values == nil {
		return 0, false
	}
	value, ok := data.Values[item].(uint64)
	return value, ok
}
