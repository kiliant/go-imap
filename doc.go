// Package imap provides the shared vocabulary of the IMAP protocol: flags,
// mailbox attributes, envelopes, body structures, search criteria, fetch and
// status items, response codes and errors.
//
// This package performs no I/O and imports nothing else from this module. It is
// the common ground between the client in
// [github.com/kiliant/go-imap/imapclient] and the server framework planned for a
// later release, which is what allows the latter to be added without a breaking
// change here.
//
// # Stability
//
// The types in this package are the ones that freeze at v1.0. They are designed
// so that a future IMAP extension can be supported by adding new types, never by
// changing existing ones. See docs/API-STABILITY.md in the repository.
//
// The three sets that nearly every IMAP extension grows — fetch items, search
// criteria and status items — are modelled as marker interfaces with an
// unexported method: [FetchItem], [SearchCriteria] and [StatusItem]. They are
// open to this library and closed to external implementers, so a new extension
// adds a type or a constant and breaks nothing. [Flag], [MailboxAttr] and
// [ResponseCode] are string-backed named types for the same reason, and a value
// this library does not know is passed through verbatim rather than discarded.
//
// The sequence-number and UID distinction is carried in the type system:
// [SeqNum] and [UID], and [SeqSet] and [UIDSet], are distinct types, so the
// classic mistake of addressing messages by the wrong kind of number does not
// compile.
//
// # Status
//
// Pre-alpha. The core vocabulary is implemented; the client is not. See
// docs/ROADMAP.md.
package imap
