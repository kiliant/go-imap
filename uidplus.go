package imap

// This file holds the UIDPLUS (RFC 4315) response-code data: the UIDs a server
// assigns when messages are placed into a mailbox.
//
// Each type carries an explicit presence field rather than inferring presence
// from a zero UIDVALIDITY. Inference only works in one direction: a decoder can
// rely on UIDVALIDITY never being zero on the wire, but a producer holding a
// value it did not decode has no way to say "this code is absent" that is
// distinguishable from "I have not filled this in yet". RFC 4315 section 3
// makes absence a normal outcome — a server omits APPENDUID and COPYUID when
// the destination mailbox is not selectable by that user, and when the
// destination has UIDNOTSTICKY status — so absence has to be expressible from
// both sides.

// AppendData is the APPENDUID data of a single-message APPEND. UIDPLUS,
// RFC 4315 section 3.
//
// UIDValidity and UID are meaningful only when HasUID is set. MULTIAPPEND may
// assign several destination UIDs; that case is [MultiAppendData], and this
// type leaves UID zero rather than picking one arbitrarily.
//
// A consumer that finds HasUID false must locate the message with SEARCH or
// FETCH instead, and must accept the race that implies: another session may
// have appended in between, so a search for a Message-ID can legitimately match
// more than the message just written.
//
// Construct with keyed fields only; fields may be added in a future release.
type AppendData struct {
	// HasUID reports whether the APPENDUID response code is present. It is
	// the reliable presence test; a zero UIDValidity is not, because a value
	// that was never filled in is indistinguishable from one deliberately
	// left empty.
	HasUID bool

	// UIDValidity is the UIDVALIDITY of the destination mailbox.
	UIDValidity uint32

	// UID is the UID assigned to the appended message.
	UID UID

	_ struct{}
}

// CopyData is the UIDPLUS response-code data for a command that placed messages
// into a mailbox: COPYUID for COPY and MOVE, and APPENDUID when the destination
// UIDs are reported as a set. UIDPLUS, RFC 4315 section 3.
//
// SourceUIDs and DestinationUIDs correspond positionally, so neither may
// contain "*" and both must hold the same number of UIDs. SourceUIDs is empty
// for APPENDUID, which has no source.
//
// Construct with keyed fields only; fields may be added in a future release.
type CopyData struct {
	// HasUIDs reports whether the COPYUID or APPENDUID response code is
	// present. See [AppendData.HasUID] for why this is a field.
	HasUIDs bool

	// UIDValidity is the UIDVALIDITY of the destination mailbox.
	UIDValidity uint32

	// SourceUIDs are the UIDs in the source mailbox, in the order the
	// messages were copied or moved. It is empty for APPENDUID.
	SourceUIDs UIDSet

	// DestinationUIDs are the UIDs assigned in the destination mailbox, in
	// the same order as SourceUIDs, or in append order for APPENDUID.
	DestinationUIDs UIDSet

	_ struct{}
}

// MultiAppendData is the APPENDUID data of a MULTIAPPEND or CATENATE APPEND,
// where several destination UIDs may be assigned. MULTIAPPEND, RFC 3502;
// UIDPLUS, RFC 4315 section 3.
//
// Construct with keyed fields only; fields may be added in a future release.
type MultiAppendData struct {
	// HasUIDs reports whether the APPENDUID response code is present. See
	// [AppendData.HasUID] for why this is a field.
	HasUIDs bool

	// UIDValidity is the UIDVALIDITY of the destination mailbox.
	UIDValidity uint32

	// UIDs are the assigned UIDs, in append order. A single-message APPEND
	// reported this way still fills UIDs with one element.
	UIDs UIDSet

	_ struct{}
}
