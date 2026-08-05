package imap

// VanishedData is one VANISHED response: a set of UIDs that are no longer in
// the mailbox. QRESYNC, RFC 7162 section 3.2.10.
//
// A VANISHED response is emphatically not an EXPUNGE response, and the two must
// not be treated interchangeably. Conflating them corrupts a client's cache:
//
//   - VANISHED identifies messages by UID and never renumbers anything.
//     EXPUNGE identifies one message by sequence number, and every later message
//     is renumbered as a result.
//   - VANISHED reports a whole set in one response. EXPUNGE reports exactly one
//     message, and several must have the renumbering applied between them.
//
// Once QRESYNC has been enabled, a server sends VANISHED instead of EXPUNGE for
// every mailbox that is not NOMODSEQ, for the rest of the connection.
//
// Construct with keyed fields only; fields may be added in a future release.
type VanishedData struct {
	// UIDs are the UIDs that vanished. The set is owned by this value, and
	// RFC 7162 section 7 forbids "*" in it.
	UIDs UIDSet

	// Earlier reports whether the response carried the (EARLIER) tag.
	//
	// Earlier is true for expunges that happened before this session knew of
	// the messages — typically while the client was disconnected — and is the
	// whole point of QRESYNC. Such messages were never announced to the
	// session by an EXISTS, so the client must not decrement its message count
	// for them.
	//
	// Earlier is false for messages expunged during the session. Those were
	// visible, so each UID listed decrements the message count by one.
	// RFC 7162 section 3.2.10 forbids naming a previously expunged or
	// never-announced message in this form.
	Earlier bool

	_ struct{}
}

// SeqMatchData is the optional fourth QRESYNC select parameter: a sample of
// sequence numbers paired with the UIDs the client believes they have. QRESYNC,
// RFC 7162 section 3.2.5.2.
//
// The server compares each pair with the mailbox's current state. Where a pair
// matches, the client demonstrably knows about every expunge up to and including
// that message, so the server can leave that range out of the VANISHED response
// even when it no longer remembers when those messages were expunged. On a large
// mailbox this can turn a VANISHED response listing tens of thousands of UIDs
// into nothing at all.
//
// Both sets must be in ascending order, must contain the same number of
// elements, and must not contain "*".
//
// Construct with keyed fields only; fields may be added in a future release.
type SeqMatchData struct {
	// SeqNums is the sample of message sequence numbers, ascending.
	SeqNums SeqSet
	// UIDs are the UIDs believed to correspond to SeqNums, ascending and of
	// the same length.
	UIDs UIDSet
	_    struct{}
}

// SyncStoreData is the content of a MODIFIED response code: the messages a
// conditional STORE did not modify. CONDSTORE, RFC 7162 section 3.1.3.
//
// Exactly one of the two sets is ever populated, matching the address space of
// the command that produced it: STORE reports sequence numbers and UID STORE
// reports UIDs. Keeping them apart is deliberate — a UID silently read as a
// sequence number points at the wrong message.
//
// Construct with keyed fields only; fields may be added in a future release.
type SyncStoreData struct {
	// ModifiedSeqNums are the sequence numbers from a MODIFIED response code
	// on a STORE. Empty when every message passed the UNCHANGEDSINCE test.
	ModifiedSeqNums SeqSet

	// ModifiedUIDs are the UIDs from a MODIFIED response code on a UID STORE.
	ModifiedUIDs UIDSet

	_ struct{}
}

// HasModified reports whether any message failed the UNCHANGEDSINCE test.
func (d *SyncStoreData) HasModified() bool {
	if d == nil {
		return false
	}
	return !d.ModifiedSeqNums.IsEmpty() || !d.ModifiedUIDs.IsEmpty()
}
