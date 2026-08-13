package imapserver

// OBJECTID (RFC 8474), SAVEDATE (RFC 8514), STATUS=SIZE (RFC 8438),
// APPENDLIMIT (RFC 7889) and PREVIEW (RFC 8970).
//
// These five need no framework machinery at all, and that is the point worth
// recording rather than the code. Each adds a FETCH data item or a STATUS item,
// and both of those are open types in package imap: imap.FetchItem and
// imap.StatusItem are interfaces, not closed enumerations, so a new item is a
// value the backend produces and the codec already knows how to write.
//
// So the whole server-side cost of these RFCs is a capability descriptor saying
// "this backend can produce that item". There is no options field, no optional
// interface, and no change to any existing signature — which is what API rule 1
// was buying when it insisted these sets stay open-ended. A closed enum here
// would have meant five breaking changes.
//
// The framework deliberately does not validate that a backend returned the items
// it claims to support. A backend that advertises SAVEDATE and returns nothing
// for it is wrong, but the framework cannot tell that from a message that
// genuinely has no save date, which RFC 8514 section 3 explicitly permits.

func init() {
	registerCapabilities(
		// OBJECTID supplies EMAILID and THREADID on FETCH and MAILBOXID on
		// STATUS. RFC 8474.
		capabilityDescriptor{
			Name:            "OBJECTID",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("OBJECTID"),
		},
		// SAVEDATE distinguishes when a message arrived in this mailbox from
		// the internal date the message itself carries. RFC 8514.
		capabilityDescriptor{
			Name:            "SAVEDATE",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("SAVEDATE"),
		},
		// STATUS=SIZE adds the SIZE status item. RFC 8438.
		capabilityDescriptor{
			Name:            "STATUS=SIZE",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("STATUS=SIZE"),
		},
		// APPENDLIMIT advertises the largest message the server accepts. The
		// per-mailbox form is a STATUS item; the server-wide form is the
		// capability token itself, which a backend expresses by witnessing
		// APPENDLIMIT and answering the STATUS item. RFC 7889.
		capabilityDescriptor{
			Name:            "APPENDLIMIT",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("APPENDLIMIT"),
		},
		// PREVIEW supplies a short textual preview of a message. RFC 8970.
		capabilityDescriptor{
			Name:            "PREVIEW",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("PREVIEW"),
		},
	)
}
