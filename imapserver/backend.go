// Package imapserver provides the server-side IMAP protocol framework.
//
// The package owns protocol framing, connection state, capability negotiation,
// sequence-number translation and update delivery. A Backend owns accounts and
// stored mail. The mandatory interfaces in this file are the fixed rev1
// baseline; later extensions add optional interfaces or guarded option fields,
// never methods to an existing interface.
//
// # Stability
//
// This is a separate module from github.com/kiliant/go-imap, versioned v0.x,
// and it does not carry that module's v1 compatibility promise. The root module
// is frozen: an incompatible change there fails CI. This one may break between
// minor versions, deliberately — the server contract has had one round of real
// backend authors and no more, and freezing it on that evidence is how the
// library this one exists to replace ended up in beta for years.
//
// Breaks will be deliberate and named in CHANGELOG.md, never silent: apidiff
// runs against the previous imapserver/v* tag on every pull request. See
// docs/RELEASING.md for the two-module scheme and docs/API-STABILITY.md §10 for
// the rules this package is held to in the meantime.
//
// # Writing a backend
//
// Start with [github.com/kiliant/go-imap/imapserver/backendtest], not with the
// interface list below. It is a reusable conformance suite: point it at your
// Backend and it exercises the mandatory contract and every optional interface
// you implement, skipping the ones you do not. The interfaces say what the
// methods are; backendtest says whether you got them right.
//
// [github.com/kiliant/go-imap/imapserver/memory] is a complete worked example
// and is supported, not a toy — it is the backend this project's own conformance
// and interoperability suites run against.
package imapserver

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"slices"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
)

// Backend authenticates connections. A Backend is shared by all connections
// and must be safe for concurrent use.
//
// Authenticate is not called concurrently for one connection. A successful
// call transfers ownership of the returned Session to that connection.
type Backend interface {
	Authenticate(ctx context.Context, conn *ConnInfo, credentials *Credentials, options *AuthenticateOptions) (Session, error)
}

// Session is one authenticated connection's backend state. Its methods and the
// methods of a SelectedMailbox returned by it are never called concurrently for
// that session. State shared between sessions must still be concurrency-safe.
//
// This method set is the mandatory IMAP4rev1 baseline and is frozen. A future
// extension adds an optional interface or an option field, not a method here.
type Session interface {
	// List is also called by the framework during SELECT and EXAMINE on an
	// IMAP4rev2 session, to produce the untagged LIST response RFC 9051 §6.3.2
	// requires, with the selection already installed. An error returned from
	// that call fails the SELECT.
	List(ctx context.Context, writer *ListWriter, reference string, patterns []string, options *ListOptions) error
	// Status answers STATUS for a mailbox that is not selected. Only the items
	// options requests need be populated; the framework writes what it asked
	// for and nothing else.
	Status(ctx context.Context, mailbox string, options *StatusOptions) (*imap.StatusData, error)
	// Create makes a mailbox. Creating one that exists is an error, and
	// [imap.CodeAlreadyExists] is the code that says so precisely enough for a
	// client to act on.
	Create(ctx context.Context, mailbox string, options *CreateOptions) error
	// Delete removes a mailbox and the messages in it. RFC 3501 §6.3.4 forbids
	// deleting INBOX, and refusing it here is the backend's job: the framework
	// does not know which name is the inbox in a given namespace.
	Delete(ctx context.Context, mailbox string, options *DeleteOptions) error
	// Rename moves a mailbox, and by RFC 3501 §6.3.5 its inferiors with it.
	// Renaming INBOX is the documented special case: its messages move to the
	// new name and INBOX itself remains.
	Rename(ctx context.Context, oldName, newName string, options *RenameOptions) error
	// Subscribe adds a mailbox to the subscription list. RFC 3501 §6.3.6 allows
	// subscribing to a name that does not exist, so this is not required to
	// validate existence.
	Subscribe(ctx context.Context, mailbox string, options *SubscribeOptions) error
	// Unsubscribe removes a mailbox from the subscription list. Unsubscribing
	// from a name that is not subscribed is not an error.
	Unsubscribe(ctx context.Context, mailbox string, options *UnsubscribeOptions) error
	// Append stores a message read from literal.
	//
	// literal is a stream, valid only for the duration of the call: read it,
	// do not retain it. The framework drains whatever is left afterwards — the
	// rest of the command cannot be parsed until the message's bytes are off
	// the wire — so returning early is safe for the connection. It is not safe
	// for memory: reading the whole literal into a buffer before storing it
	// makes the server's footprint a function of what the client chose to send.
	//
	// The returned AppendData carries the new UID and UIDVALIDITY when the
	// backend can supply them, which is what RFC 4315's APPENDUID response code
	// is made of. Returning nil is allowed and simply means no code is sent —
	// but a backend that witnesses UIDPLUS has claimed it will supply them.
	Append(ctx context.Context, mailbox string, literal io.Reader, options *AppendOptions) (*imap.AppendData, error)
	// Select captures Snapshot and attaches updater to that exact state atomically.
	// If it attaches updater and then fails, it must detach before returning.
	Select(ctx context.Context, mailbox string, updater *Updater, options *SelectOptions) (*SelectResult, error)
	// Close releases everything the session holds. The framework calls it once,
	// when the connection ends, including after an error and after
	// UNAUTHENTICATE. No other method is called afterwards, and the session is
	// discarded even if Close returns an error.
	Close(ctx context.Context, options *SessionCloseOptions) error
}

// SessionCloseOptions configures the end of a session. A nil pointer selects
// the defaults.
//
// It is empty and exists anyway, because the alternative is that the mandatory
// interface's promise — "a future extension adds an optional interface or an
// option field, not a method here" — is false for this method. RFC 6785
// (IMAPSIEVE) is the concrete pressure: it fires backend-side scripts on IMAP
// events, and a session ending because the connection closed is a different
// event from one ending because UNAUTHENTICATE reclaimed the connection for
// another identity. Today the backend cannot tell them apart.
//
// Construct with keyed fields only; fields may be added in a future release.
type SessionCloseOptions struct{ _ struct{} }

// SelectedMailbox is a backend's per-connection handle for one selection. The
// framework, not this value, owns the UID/sequence-number map, enabled features,
// saved search result and pending-update queue.
//
// Backends receive UIDs only. Sequence numbers are resolved by the framework
// before any method here is called.
type SelectedMailbox interface {
	// Status answers STATUS for this selection, which a client may ask for even
	// while the mailbox is selected.
	Status(ctx context.Context, options *StatusOptions) (*imap.MailboxStatus, error)
	// Fetch streams the requested items through writer, one message at a time.
	//
	// Streaming is the contract, not an optimisation: a backend that buffers a
	// mailbox of large messages before writing has made the server's memory
	// use a function of the client's request. writer is valid only during the
	// call.
	//
	// A UID in uids that no longer exists is skipped, not an error — RFC 3501
	// §6.4.8 makes a UID FETCH of a vanished message an empty result.
	Fetch(ctx context.Context, writer *FetchWriter, uids imap.UIDSet, options *FetchOptions) error
	// Search evaluates query and returns the matching UIDs.
	//
	// The criteria tree arrives UID-normalised: the framework has already
	// resolved every sequence number in it, so a backend never sees one and
	// never needs the sequence view to answer. See [SearchQuery].
	Search(ctx context.Context, query *SearchQuery, options *SearchOptions) (*SearchResult, error)
	// Store applies a flag mutation and streams the resulting flags through
	// writer, unless options.Silent suppresses the responses — which suppresses
	// only the responses, never the mutation or the updates other sessions see.
	//
	// A conditional store (UNCHANGEDSINCE) does not arrive here; it goes to
	// [CondStoreMailbox.StoreCondStore], so this method never implements the
	// modification-sequence comparison.
	Store(ctx context.Context, writer *FetchWriter, uids imap.UIDSet, flags *StoreFlags, options *StoreOptions) error
	// Copy copies messages to another mailbox. The returned CopyData carries
	// the source and destination UIDs when the backend can supply them, which
	// is what RFC 4315's COPYUID response code is made of; see Append for what
	// returning nil means.
	//
	// A destination that does not exist is the one error with a required
	// response code: RFC 3501 §6.4.7 obliges TRYCREATE, and a client uses it to
	// decide whether to create the mailbox and retry.
	Copy(ctx context.Context, uids imap.UIDSet, destination string, options *CopyOptions) (*imap.CopyData, error)
	// Expunge permanently removes messages flagged \Deleted, reporting each
	// removed UID through writer.
	//
	// uids is the filter RFC 4315's UID EXPUNGE supplies: nil means every
	// \Deleted message, and non-nil restricts the removal to that set. A
	// backend that ignores it deletes messages the client asked to keep, which
	// is why the parameter is a pointer rather than an empty-means-all set —
	// the two cases had to be impossible to confuse.
	Expunge(ctx context.Context, writer *ExpungeWriter, uids *imap.UIDSet, options *ExpungeOptions) error
	// Unselect releases the selection. The framework calls it once per
	// selection — on CLOSE, UNSELECT, a replacing SELECT, or connection
	// teardown — after which this handle is not used again.
	Unselect(ctx context.Context, options *UnselectOptions) error
}

// UnselectOptions configures the end of a selection. A nil pointer selects the
// defaults.
//
// Empty for the same reason as [SessionCloseOptions], and with a sharper
// example: RFC 3501 CLOSE performs an implicit expunge and RFC 3691 UNSELECT
// deliberately does not, yet both arrive here identically. A backend that has
// to distinguish them — for RFC 6785 event scripts, or for an audit log — has
// nowhere to learn which happened.
//
// Construct with keyed fields only; fields may be added in a future release.
type UnselectOptions struct{ _ struct{} }

// MoveMailbox is the optional atomic MOVE operation. The framework never
// synthesises MOVE from COPY, STORE and EXPUNGE; advertisement requires a
// [MoveSupport] witness and, once selected, this interface.
type MoveMailbox interface {
	Move(ctx context.Context, uids imap.UIDSet, destination string, options *MoveOptions) (*imap.CopyData, error)
}

// MoveSupport is the optional capability witness for atomic MOVE. A Backend
// may implement it to describe support before authentication; a Session may
// implement it when support varies by authenticated user. When SupportsMove
// returns true, every selected mailbox on which MOVE is available must also
// implement [MoveMailbox].
//
// Backend implementations must make SupportsMove safe for concurrent use.
//
// Deprecated in intent, not yet in fact: [CapabilitySupport] expresses the same
// thing keyed by wire token, and this interface exists only because it predates
// it. It is kept because MOVE's witness also gates IMAP4rev2 advertisement, so
// collapsing it is a behavioural change rather than a rename. See
// docs/API-STABILITY.md section 10; the window to collapse it closes at
// imapserver v1.0.
type MoveSupport interface {
	SupportsMove() bool
}

// CapabilitySupport is the open witness by which a Backend or Session declares
// that it implements the behaviour behind an optional capability. A Backend may
// implement it to describe support before authentication; a Session may
// implement it when support varies by authenticated user, and is consulted
// first when it does.
//
// name is an upper-case capability token exactly as it appears on the wire,
// such as "CONDSTORE" or "SPECIAL-USE". The framework never advertises a
// capability whose behaviour the backend must implement unless this returns
// true for it, so a backend that does not recognise a name must return false.
//
// This witness is deliberately keyed by an open string rather than by one
// interface per capability: a future RFC is then a new token, which is a data
// change, not a type change. Atomic MOVE predates it and keeps its own
// [MoveSupport] witness.
//
// Backend implementations must make SupportsCapability safe for concurrent use.
type CapabilitySupport interface {
	SupportsCapability(name string) bool
}

// ConnInfo describes the transport presented to Backend.Authenticate.
// Construct with keyed fields only; fields may be added in a future release.
type ConnInfo struct {
	// LocalAddr is the server-side transport address.
	LocalAddr net.Addr
	// RemoteAddr is the peer transport address.
	RemoteAddr net.Addr
	// TLS is a copy of the negotiated TLS state, or nil on cleartext.
	TLS *tls.ConnectionState
	_   struct{}
}

// Credentials are credentials extracted by a framework-owned authentication
// mechanism. Password is empty for token mechanisms.
// Construct with keyed fields only; fields may be added in a future release.
type Credentials struct {
	// Mechanism is the upper-case SASL mechanism name.
	Mechanism string
	// AuthzID is the requested authorization identity, if distinct from the
	// authentication identity.
	AuthzID string
	// Username is the extracted authentication identity when the mechanism
	// carries one explicitly.
	Username string
	// Password is populated only for password mechanisms.
	Password string
	// Token is populated only for bearer-token mechanisms.
	Token string
	_     struct{}
}

// AuthenticateOptions configures one authentication attempt. A nil pointer
// selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type AuthenticateOptions struct {
	_ struct{}
}

// MutationOptions identifies a framework command that may publish updates.
// Backends copy Origin into the UpdateBatch produced by the same transaction.
// Construct with keyed fields only; fields may be added in a future release.
type MutationOptions struct {
	// Origin must be copied to the UpdateBatch published for this mutation.
	Origin ChangeToken
	_      struct{}
}

// ListOptions configures a LIST or LSUB operation. A nil pointer selects the
// defaults.
//
// Selection options restrict which mailboxes are returned; return options ask
// for extra attributes on the mailboxes that are returned. LIST-STATUS is not
// represented here: RFC 5819 delivers it as a separate untagged STATUS response
// per mailbox, which the framework issues through [Session.Status] once List has
// returned, so a backend needs no field for it.
// Construct with keyed fields only; fields may be added in a future release.
type ListOptions struct {
	// Subscribed requests subscribed-mailbox selection.
	Subscribed bool `imapfeature:"list-subscribed"`
	// SelectRemote requests that remote mailboxes be included. RFC 5258
	// section 3.
	SelectRemote bool `imapfeature:"list-extended"`
	// SelectRecursiveMatch requests that a mailbox be returned when a child
	// matches the selection criteria, carrying a CHILDINFO extended item.
	// It is meaningless without another selection option. RFC 5258 section 3.5.
	SelectRecursiveMatch bool `imapfeature:"list-extended"`
	// SelectSpecialUse restricts the result to special-use mailboxes.
	// RFC 6154 section 5.1.
	SelectSpecialUse bool `imapfeature:"list-special-use"`
	// ReturnSubscribed asks for the \Subscribed attribute on returned
	// mailboxes. RFC 5258 section 3.
	ReturnSubscribed bool `imapfeature:"list-extended"`
	// ReturnChildren asks for the \HasChildren and \HasNoChildren attributes.
	// RFC 3348, incorporated into IMAP4rev2.
	ReturnChildren bool `imapfeature:"list-children"`
	// ReturnSpecialUse asks for special-use attributes such as \Archive.
	// RFC 6154 section 5.2.
	ReturnSpecialUse bool `imapfeature:"list-special-use"`
	_                struct{}
}

// StatusOptions configures STATUS. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type StatusOptions struct {
	// Items are the requested status values.
	Items []imap.StatusItem
	_     struct{}
}

// CreateOptions configures CREATE. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type CreateOptions struct {
	// SpecialUse are the use attributes requested for the new mailbox, such as
	// [imap.MailboxAttrArchive]. It is populated only when the backend
	// witnesses CREATE-SPECIAL-USE through [CapabilitySupport]; a backend that
	// cannot honour an attribute must fail the command rather than create a
	// mailbox without it. RFC 6154 section 3.
	SpecialUse []imap.MailboxAttr `imapfeature:"create-special-use"`
	_          struct{}
}

// DeleteOptions configures DELETE. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type DeleteOptions struct{ _ struct{} }

// RenameOptions configures RENAME. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type RenameOptions struct{ _ struct{} }

// SubscribeOptions configures SUBSCRIBE. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type SubscribeOptions struct{ _ struct{} }

// UnsubscribeOptions configures UNSUBSCRIBE. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type UnsubscribeOptions struct{ _ struct{} }

// AppendOptions configures APPEND. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type AppendOptions struct {
	MutationOptions
	// Flags is the initial flag set.
	Flags []imap.Flag
	// InternalDate is the message's supplied internal date. The zero value asks
	// the backend to use its current time.
	InternalDate time.Time
	_            struct{}
}

// SelectOptions configures SELECT or EXAMINE. A nil pointer selects the
// defaults.
// Construct with keyed fields only; fields may be added in a future release.
type SelectOptions struct {
	// ReadOnly distinguishes EXAMINE from SELECT.
	ReadOnly bool
	// CondStore asks the backend to report per-message modification sequences
	// for this selection. RFC 7162 section 3.1.
	CondStore bool `imapfeature:"condstore-select"`
	// QResync carries the client's synchronisation state, or nil when the
	// selection is not a QRESYNC resynchronisation. Implies CondStore.
	// RFC 7162 section 3.2.5.
	QResync *QResyncSelect `imapfeature:"qresync"`
	_       struct{}
}

// QResyncSelect is the client's claimed synchronisation state, supplied with
// SELECT or EXAMINE under QRESYNC. A backend answers it by reporting the
// messages that changed or vanished since ModSeq.
//
// UIDValidity is the value the client last saw. When it does not match the
// mailbox's current UIDVALIDITY the client's state is stale and the backend
// must ignore the remaining fields.
// Construct with keyed fields only; fields may be added in a future release.
type QResyncSelect struct {
	// UIDValidity is the UIDVALIDITY the client last observed.
	UIDValidity uint32 `imapfeature:"qresync"`
	// ModSeq is the modification sequence the client last observed.
	ModSeq uint64 `imapfeature:"qresync"`
	// KnownUIDs optionally restricts the report to these UIDs. An empty set
	// means the client did not restrict it.
	KnownUIDs imap.UIDSet `imapfeature:"qresync"`
	// SeqMatchSeqNums and SeqMatchUIDs are the optional sequence match data of
	// RFC 7162 section 3.2.5.2, letting a backend detect a stale client view
	// without reporting every UID. Both are empty when absent, and have equal
	// length when present.
	SeqMatchSeqNums imap.SeqSet `imapfeature:"qresync"`
	// SeqMatchUIDs pairs positionally with SeqMatchSeqNums.
	SeqMatchUIDs imap.UIDSet `imapfeature:"qresync"`
	_            struct{}
}

// FetchOptions configures FETCH. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type FetchOptions struct {
	// Items are the data items requested for each matching message.
	//
	// Every item belongs to the IMAP4rev1 baseline or to a capability this
	// session advertised. See [SearchQuery.Criteria] for why the framework
	// refuses what it cannot classify rather than forwarding it.
	Items []imap.FetchItem
	// ChangedSince restricts the result to messages whose modification
	// sequence is greater than this value. Zero means unrestricted.
	// RFC 7162 section 3.1.4.
	ChangedSince uint64 `imapfeature:"condstore-fetch"`
	_            struct{}
}

// SearchOptions configures SEARCH. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type SearchOptions struct {
	// Charset is the declared search-string charset, or empty for the protocol
	// default.
	Charset string
	_       struct{}
}

// StoreFlagsOp selects how STORE changes the current flag set.
type StoreFlagsOp string

const (
	// StoreFlagsSet replaces the flag set.
	StoreFlagsSet StoreFlagsOp = "FLAGS"
	// StoreFlagsAdd adds flags.
	StoreFlagsAdd StoreFlagsOp = "+FLAGS"
	// StoreFlagsRemove removes flags.
	StoreFlagsRemove StoreFlagsOp = "-FLAGS"
)

// StoreFlags is the flag mutation supplied to SelectedMailbox.Store.
// Construct with keyed fields only; fields may be added in a future release.
type StoreFlags struct {
	// Op selects replace, add or remove semantics.
	Op StoreFlagsOp
	// Flags is the operand flag set.
	Flags []imap.Flag
	_     struct{}
}

// StoreOptions configures STORE. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type StoreOptions struct {
	MutationOptions
	// Silent suppresses the command's FETCH responses, but not state updates.
	Silent bool
	// UnchangedSince makes the store conditional: a message whose modification
	// sequence exceeds this value must be left unmodified. Backends report the
	// rejected messages through the CONDSTORE optional interface rather than an
	// error, since a partial failure is a successful command.
	// RFC 7162 section 3.1.3.
	//
	// HasUnchangedSince carries presence separately because zero is a real
	// value here, not an absence: the grammar is mod-sequence-valzer, and RFC
	// 7162 Example 8 uses UNCHANGEDSINCE 0 as a probe that always fails, which
	// is how a client tests atomically for the presence of a keyword. Reading
	// UnchangedSince without checking HasUnchangedSince turns that probe into
	// an unconditional store of the messages it was meant to protect.
	UnchangedSince uint64 `imapfeature:"condstore-store"`
	// HasUnchangedSince reports whether the client supplied UNCHANGEDSINCE.
	HasUnchangedSince bool `imapfeature:"condstore-store"`
	_                 struct{}
}

// CopyOptions configures COPY. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type CopyOptions struct {
	MutationOptions
	_ struct{}
}

// MoveOptions configures atomic MOVE. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type MoveOptions struct {
	MutationOptions
	_ struct{}
}

// ExpungeOptions configures EXPUNGE. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type ExpungeOptions struct {
	MutationOptions
	_ struct{}
}

// SelectResult is the atomic result of Session.Select. Snapshot describes the
// exact state to which the updater was attached before Select returned.
// Construct with keyed fields only; fields may be added in a future release.
type SelectResult struct {
	// Mailbox is the backend's per-connection selection handle.
	Mailbox SelectedMailbox
	// Snapshot is the exact state to which the updater was attached.
	Snapshot SelectSnapshot
	_        struct{}
}

// SelectSnapshot is one atomic mailbox state and the initial sequence-number
// view. UIDs must be non-zero and strictly ascending.
// Construct with keyed fields only; fields may be added in a future release.
type SelectSnapshot struct {
	// UIDs lists every visible message in strictly ascending sequence order.
	UIDs []imap.UID
	// Status carries the message count, UIDVALIDITY, UIDNEXT and related
	// mailbox values for the same atomic state.
	Status imap.MailboxStatus
	// Flags is the set of flags currently present in the mailbox.
	Flags []imap.Flag
	// PermanentFlags is the set the client may change.
	PermanentFlags []imap.Flag
	// ReadOnly reports that the mailbox was selected without write access.
	ReadOnly bool
	// NumRecent is the rev1 RECENT count.
	NumRecent uint32
	// HighestModSeq is the highest modification sequence for CONDSTORE.
	HighestModSeq uint64
	// NoModSeq selects NOMODSEQ instead of a zero-valued HIGHESTMODSEQ.
	NoModSeq bool
	// Revision starts the selected updater's mandatory Before/After chain.
	Revision MailboxRevision
	_        struct{}
}

// MailboxRevision is an opaque, comparable backend revision. Backends should
// use their natural transaction identifier when possible.
type MailboxRevision string

// ChangeToken identifies the currently executing mutating command. Zero means
// that no framework command supplied an origin.
type ChangeToken uint64

// UpdateBatch is every observable change from one atomic backend commit.
// Changes are applied in slice order and batches are never merged.
// Construct with keyed fields only; fields may be added in a future release.
type UpdateBatch struct {
	// Before must equal the selected snapshot revision or previous batch's
	// After value.
	Before MailboxRevision
	// After identifies the state after every Changes entry has been applied.
	After MailboxRevision
	// Origin identifies the mutating command, or is zero for an external
	// change.
	Origin ChangeToken
	// Changes are applied in order and belong to one atomic backend commit.
	Changes []Update
	_       struct{}
}

// Update is one UID-keyed selected-mailbox change. The set is open to this
// package and closed to external implementations; future extensions add a new
// concrete value without changing UpdateBatch.
type Update interface{ update() }

// UpdateAdd adds UIDs to the selected view. UIDs must be non-zero and in
// ascending order.
// Construct with keyed fields only; fields may be added in a future release.
type UpdateAdd struct {
	// UIDs are the newly visible UIDs in strictly ascending order.
	UIDs []imap.UID
	_    struct{}
}

func (*UpdateAdd) update() {}

// UpdateFlags replaces one message's flags and optionally reports its modseq.
// Construct with keyed fields only; fields may be added in a future release.
type UpdateFlags struct {
	// UID identifies the affected message.
	UID imap.UID
	// Flags is the message's complete new flag set.
	Flags []imap.Flag
	// ModSeq is the new modification sequence, or zero when unavailable.
	ModSeq uint64
	_      struct{}
}

func (*UpdateFlags) update() {}

// UpdateExpunge removes one UID. EXPUNGE updates remain individually ordered.
// Construct with keyed fields only; fields may be added in a future release.
type UpdateExpunge struct {
	// UID identifies the removed message.
	UID imap.UID
	_   struct{}
}

func (*UpdateExpunge) update() {}

// UpdateVanished removes one or more UIDs using QRESYNC VANISHED semantics.
// Earlier and non-Earlier values are never coalesced together.
// Construct with keyed fields only; fields may be added in a future release.
type UpdateVanished struct {
	// UIDs is a concrete, non-dynamic set of removed UIDs.
	UIDs imap.UIDSet
	// Earlier selects the VANISHED (EARLIER) wire form.
	Earlier bool
	_       struct{}
}

func (*UpdateVanished) update() {}

// SearchResult is the UID-keyed result returned by SelectedMailbox.Search.
// Construct with keyed fields only; fields may be added in a future release.
type SearchResult struct {
	// UIDs are the matching message identifiers.
	UIDs []imap.UID
	// ModSeq is the highest modification sequence among the matching messages,
	// or zero when the backend does not track them. The framework reports it
	// only when the client asked for it and CONDSTORE is active.
	// RFC 7162 section 3.1.5.
	ModSeq uint64
	_      struct{}
}

// SearchQuery is a UID-normalised SEARCH tree. Values are constructed only by
// the framework, so a backend can rely on Criteria containing no SearchSeqNum.
type SearchQuery struct{ criteria imap.SearchCriteria }

// newSearchQuery constructs the only SEARCH tree that may cross the backend
// boundary. uids is the selected mailbox's sequence-order snapshot.
func newSearchQuery(criteria imap.SearchCriteria, uids []imap.UID) *SearchQuery {
	return &SearchQuery{criteria: normalizeSearchCriteria(criteria, uids)}
}

// searchCriteriaChildren decomposes a container criterion into its children and
// a function that rebuilds it from replacements. A leaf reports nil.
//
// This is the single definition of "which search keys contain other search
// keys", and every traversal of a criteria tree goes through it.
//
// It exists because there were two such traversals and they disagreed.
// normalizeSearchCriteria descended into SearchFuzzy; the FILTER substitution
// walk did not, so `SEARCH FUZZY FILTER "x"` delivered an unsubstituted
// imap.SearchFilter to the backend and skipped the FILTERS capability check with
// it. Nothing forced the two lists to agree, and a hand-maintained list of
// container types is exactly the thing that silently falls behind when RFC N+1
// adds a container key. TestSearchCriteriaContainersAreTraversed fails if one
// is added to package imap without appearing here.
func searchCriteriaChildren(criteria imap.SearchCriteria) ([]imap.SearchCriteria, func([]imap.SearchCriteria) imap.SearchCriteria) {
	return imapcodec.SearchCriteriaChildren(criteria)
}

// searchMentionsSeqNum reports whether a criteria tree names a message sequence
// number anywhere in it.
//
// It shares searchCriteriaChildren with the normalisation walk, so a container
// key added by a future RFC is traversed by both or by neither —
// TestSearchCriteriaContainersAreTraversed is what makes that true rather than
// hoped for.
func searchMentionsSeqNum(criteria imap.SearchCriteria) bool {
	if children, rebuild := searchCriteriaChildren(criteria); rebuild != nil {
		return slices.ContainsFunc(children, searchMentionsSeqNum)
	}
	_, ok := criteria.(imap.SearchSeqNum)
	return ok
}

func normalizeSearchCriteria(criteria imap.SearchCriteria, uids []imap.UID) imap.SearchCriteria {
	if children, rebuild := searchCriteriaChildren(criteria); rebuild != nil {
		normalized := make([]imap.SearchCriteria, len(children))
		for i, child := range children {
			normalized[i] = normalizeSearchCriteria(child, uids)
		}
		return rebuild(normalized)
	}
	switch criteria := criteria.(type) {
	case imap.SearchSeqNum:
		var set imap.UIDSet
		for i, uid := range uids {
			if searchSeqSetContains(criteria.Set, imap.SeqNum(i+1), imap.SeqNum(len(uids))) {
				set.AddNum(uid)
			}
		}
		return imap.SearchUID{Set: set.Normalized()}
	default:
		return criteria
	}
}

func searchSeqSetContains(set imap.SeqSet, seqNum, maximum imap.SeqNum) bool {
	if seqNum == 0 || maximum == 0 {
		return false
	}
	for _, r := range set {
		start, stop := r.Start, r.Stop
		if start == 0 {
			start = maximum
		}
		if stop == 0 {
			stop = maximum
		}
		if start > stop {
			start, stop = stop, start
		}
		if start <= seqNum && seqNum <= stop {
			return true
		}
	}
	return false
}

// Criteria returns the UID-normalised criteria. Callers must treat the returned
// tree as immutable for the duration of Search.
//
// The framework guarantees the tree contains no [imap.SearchSeqNum] — sequence
// numbers are resolved to UIDs before the backend sees them — and no
// [imap.SearchFilter], which is substituted for the criteria it names. Both hold
// at every nesting depth, including inside [imap.SearchNot] and
// [imap.SearchFuzzy]. A backend therefore never has to handle either, which is
// what allows the root package to grow new [imap.SearchCriteria]
// implementations without breaking backends compiled against an earlier
// version. See docs/API-STABILITY.md section 10.
//
// The same guarantee holds for [MultiSearchSession.MultiSearch], which takes
// criteria directly rather than a query: with no IN clause sequence numbers
// resolve against the selection, and with one the command is refused, so a
// backend never receives an unresolvable number there either.
//
// A criterion outside that guarantee reaching a backend is a framework bug, not
// a case for the backend to interpret. TestSearchQueryNormalisationGuarantee
// enforces it for every command that builds a query, and
// TestSearchCriteriaContainersAreTraversed fails if a future container key is
// added to package imap without the traversal learning to descend into it.
//
// # Capability
//
// Every criterion in the tree belongs to the IMAP4rev1 baseline or to a
// capability this session advertised, at every nesting depth. A backend that
// witnesses no extension capability therefore receives baseline keys only, and
// a criterion added by a later release of package imap cannot reach a backend
// compiled before it existed — the framework refuses what it cannot classify
// rather than passing it through.
//
// That matters more than it looks. Without it, teaching package imap a new key
// silently widens what every already-compiled backend receives, and the
// realistic outcome is not a crash but a permissive default branch returning an
// empty result. See capability_keys.go, and docs/API-STABILITY.md §10.
func (q *SearchQuery) Criteria() imap.SearchCriteria {
	if q == nil {
		return nil
	}
	return q.criteria
}

// ListWriter streams LIST results to the client. Its zero value is invalid.
// Construct with keyed fields only; fields may be added in a future release.
type ListWriter struct {
	// WriteFunc receives streamed results when the writer is constructed by an
	// adapter such as backendtest. Ordinary backends call WriteList and leave
	// this field unset on framework-provided writers.
	WriteFunc func(context.Context, *imap.ListData) error
	core      *writerCore[*imap.ListData]
}

// WriteList writes one LIST result. ctx is normally the context passed to the
// enclosing Session.List call.
func (w *ListWriter) WriteList(ctx context.Context, data *imap.ListData) error {
	if w == nil {
		return ErrWriterClosed
	}
	if w.core != nil {
		return w.core.writeValue(ctx, data)
	}
	if w.WriteFunc != nil {
		return w.WriteFunc(ctx, data)
	}
	return ErrWriterClosed
}

// FetchWriter streams FETCH results to the client. Its zero value is invalid.
//
// Every message written to a framework-provided writer by Fetch or Store must
// contain exactly one non-zero [imap.FetchDataUID] under the UID key. The
// framework ignores FetchMessageData.SeqNum and uses that UID to derive the
// response sequence number from its atomic selection map. It removes the
// internally requested UID again when the client did not request it.
// Construct with keyed fields only; fields may be added in a future release.
type FetchWriter struct {
	// WriteFunc receives streamed results when the writer is constructed by an
	// adapter such as backendtest. Ordinary backends call WriteMessage and leave
	// this field unset on framework-provided writers.
	WriteFunc func(context.Context, *imap.FetchMessageData) error
	core      *writerCore[*imap.FetchMessageData]
}

// WriteMessage writes one FETCH result. ctx is normally the context passed to
// the enclosing SelectedMailbox method.
func (w *FetchWriter) WriteMessage(ctx context.Context, data *imap.FetchMessageData) error {
	if w == nil {
		return ErrWriterClosed
	}
	if w.core != nil {
		return w.core.writeValue(ctx, data)
	}
	if w.WriteFunc != nil {
		return w.WriteFunc(ctx, data)
	}
	return ErrWriterClosed
}

// ExpungeWriter streams UID-keyed removals to the framework. Its zero value is
// invalid. Construct with keyed fields only; fields may be added in a future
// release.
type ExpungeWriter struct {
	// WriteFunc receives streamed removals when the writer is constructed by an
	// adapter such as backendtest. Ordinary backends call WriteExpunge and leave
	// this field unset on framework-provided writers.
	WriteFunc func(context.Context, imap.UID, *WriteExpungeOptions) error
	core      *writerCore[imap.UID]
}

// WriteExpungeOptions configures one streamed removal. A nil pointer selects
// the defaults.
//
// Empty today, and the reason this writer has one while its siblings do not:
// [ListWriter.WriteList], [FetchWriter.WriteMessage] and [Updater.Push] all
// carry a growable struct as their payload, so a new RFC adds a field there.
// A UID is a scalar with nowhere to grow. RFC 7162 is the pressure — VANISHED
// (EARLIER) distinguishes a removal the client already knew about from one it
// did not, which is a property of the write and not of the UID.
//
// Construct with keyed fields only; fields may be added in a future release.
type WriteExpungeOptions struct{ _ struct{} }

// WriteExpunge writes one removed UID. The framework converts it to the
// sequence number current at this exact point in the response.
//
// options may be nil.
func (w *ExpungeWriter) WriteExpunge(ctx context.Context, uid imap.UID, options *WriteExpungeOptions) error {
	if w == nil {
		return ErrWriterClosed
	}
	if w.core != nil {
		return w.core.writeValue(ctx, uid)
	}
	if w.WriteFunc != nil {
		return w.WriteFunc(ctx, uid, options)
	}
	return ErrWriterClosed
}

// Updater publishes selected-mailbox updates. Push never calls into the
// backend and never blocks waiting for the connection's event loop.
// Construct with keyed fields only; fields may be added in a future release.
type Updater struct {
	// PushFunc receives update batches when the updater is constructed by an
	// adapter such as backendtest. Ordinary backends call Push and leave this
	// field unset on framework-provided updaters.
	PushFunc func(*UpdateBatch) error
	core     *updaterCore
}

// Push publishes one atomic batch. It returns ErrUpdaterClosed after the
// selection ends and ErrUpdateOverflow when bounded update accounting forces
// the connection to terminate.
func (u *Updater) Push(batch *UpdateBatch) error {
	if u == nil {
		return ErrUpdaterClosed
	}
	if u.core != nil {
		return u.core.push(batch)
	}
	if u.PushFunc != nil {
		return u.PushFunc(batch)
	}
	return ErrUpdaterClosed
}
