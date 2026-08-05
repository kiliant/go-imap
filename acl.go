package imap

// ACLRights is an ACL rights string: the set of rights letters an identifier
// holds on a mailbox. ACL, RFC 4314.
//
// It is deliberately open-ended. RFC 4314 defines the base letters and a
// RIGHTS= capability advertisement names further sets, so a closed enumeration
// would break on the next rights extension. A letter this library does not know
// is carried verbatim in both directions.
type ACLRights string

// ACLEntry is one identifier/rights pair of an ACL response. RFC 4314
// section 3.6.
//
// Construct with keyed fields only; fields may be added in a future release.
type ACLEntry struct {
	Identifier string
	Rights     ACLRights
	_          struct{}
}

// ACLData is the content of an untagged ACL response. RFC 4314 section 3.6.
//
// Construct with keyed fields only; fields may be added in a future release.
type ACLData struct {
	Mailbox string
	Entries []ACLEntry
	_       struct{}
}

// ListRightsData is the content of an untagged LISTRIGHTS response. RFC 4314
// section 3.7.
//
// Required holds the rights always granted to the identifier. Optional holds
// the rights that may be granted individually, each element being one
// separately grantable group.
//
// Construct with keyed fields only; fields may be added in a future release.
type ListRightsData struct {
	Mailbox    string
	Identifier string
	Required   ACLRights
	Optional   []ACLRights
	_          struct{}
}

// MyRightsData is the content of an untagged MYRIGHTS response. RFC 4314
// section 3.8.
//
// Construct with keyed fields only; fields may be added in a future release.
type MyRightsData struct {
	Mailbox string
	Rights  ACLRights
	_       struct{}
}
