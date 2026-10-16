package imap

// MetadataEntryName is a METADATA entry name such as "/shared/comment".
// METADATA, RFC 5464.
//
// Names are open-ended: RFC 5464 defines the /private and /shared trees and a
// server may serve further entries without an API change here.
type MetadataEntryName string

// MetadataEntry is one entry/value pair of a METADATA response or a SETMETADATA
// command. RFC 5464 section 4.4.
//
// Value is nil for NIL, which means the entry is unset — and, in SETMETADATA,
// means "remove it". An empty string is a present, empty value and is a
// different thing.
//
// Construct with keyed fields only; fields may be added in a future release.
type MetadataEntry struct {
	Name  MetadataEntryName
	Value *string
	_     struct{}
}

// MailboxMetadata is the content of one untagged METADATA response: the entries
// reported for one mailbox, or for the server annotation space when Mailbox is
// empty. RFC 5464 section 4.4.
//
// Construct with keyed fields only; fields may be added in a future release.
type MailboxMetadata struct {
	Mailbox string
	Entries []MetadataEntry
	_       struct{}
}

// MetadataChange reports entry names changed in a names-only METADATA
// notification (RFC 5464 section 4.4.2). An empty Mailbox identifies server
// metadata; otherwise Mailbox is the decoded mailbox name.
//
// Entries are names to refresh, not entry/value pairs: a notification does not
// say whether an entry is unset, empty, or has a value. Those states are
// represented by [MetadataEntry] in a GETMETADATA result.
//
// Construct with keyed fields only; fields may be added in a future release.
type MetadataChange struct {
	Mailbox string
	Entries []MetadataEntryName
	_       struct{}
}
