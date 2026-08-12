// Package imapserver provides the server-side IMAP protocol framework.
//
// The package owns protocol framing, connection state, capability negotiation,
// sequence-number translation and update delivery. A Backend owns accounts and
// stored mail. The mandatory interfaces in this file are the fixed rev1
// baseline; later extensions add optional interfaces or guarded option fields,
// never methods to an existing interface.
package imapserver

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"time"

	"github.com/kiliant/go-imap"
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
	List(ctx context.Context, writer *ListWriter, reference string, patterns []string, options *ListOptions) error
	Status(ctx context.Context, mailbox string, options *StatusOptions) (*imap.StatusData, error)
	Create(ctx context.Context, mailbox string, options *CreateOptions) error
	Delete(ctx context.Context, mailbox string, options *DeleteOptions) error
	Rename(ctx context.Context, oldName, newName string, options *RenameOptions) error
	Subscribe(ctx context.Context, mailbox string, options *SubscribeOptions) error
	Unsubscribe(ctx context.Context, mailbox string, options *UnsubscribeOptions) error
	Append(ctx context.Context, mailbox string, literal io.Reader, options *AppendOptions) (*imap.AppendData, error)
	// Select captures Snapshot and attaches updater to that exact state atomically.
	// If it attaches updater and then fails, it must detach before returning.
	Select(ctx context.Context, mailbox string, updater *Updater, options *SelectOptions) (*SelectResult, error)
	Close(ctx context.Context) error
}

// SelectedMailbox is a backend's per-connection handle for one selection. The
// framework, not this value, owns the UID/sequence-number map, enabled features,
// saved search result and pending-update queue.
//
// Backends receive UIDs only. Sequence numbers are resolved by the framework
// before any method here is called.
type SelectedMailbox interface {
	Status(ctx context.Context, options *StatusOptions) (*imap.MailboxStatus, error)
	Fetch(ctx context.Context, writer *FetchWriter, uids imap.UIDSet, options *FetchOptions) error
	Search(ctx context.Context, query *SearchQuery, options *SearchOptions) (*SearchResult, error)
	Store(ctx context.Context, writer *FetchWriter, uids imap.UIDSet, flags *StoreFlags, options *StoreOptions) error
	Copy(ctx context.Context, uids imap.UIDSet, destination string, options *CopyOptions) (*imap.CopyData, error)
	Expunge(ctx context.Context, writer *ExpungeWriter, uids *imap.UIDSet, options *ExpungeOptions) error
	Unselect(ctx context.Context) error
}

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
type MoveSupport interface {
	SupportsMove() bool
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
// Construct with keyed fields only; fields may be added in a future release.
type ListOptions struct {
	// Subscribed requests subscribed-mailbox selection.
	Subscribed bool `imapfeature:"list-subscribed"`
	_          struct{}
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
type CreateOptions struct{ _ struct{} }

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
	_        struct{}
}

// FetchOptions configures FETCH. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type FetchOptions struct {
	// Items are the data items requested for each matching message.
	Items []imap.FetchItem
	_     struct{}
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
	_      struct{}
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
	_    struct{}
}

// SearchQuery is a UID-normalised SEARCH tree. Values are constructed only by
// the framework, so a backend can rely on Criteria containing no SearchSeqNum.
type SearchQuery struct{ criteria imap.SearchCriteria }

// newSearchQuery constructs the only SEARCH tree that may cross the backend
// boundary. uids is the selected mailbox's sequence-order snapshot.
func newSearchQuery(criteria imap.SearchCriteria, uids []imap.UID) *SearchQuery {
	return &SearchQuery{criteria: normalizeSearchCriteria(criteria, uids)}
}

func normalizeSearchCriteria(criteria imap.SearchCriteria, uids []imap.UID) imap.SearchCriteria {
	switch criteria := criteria.(type) {
	case imap.SearchAnd:
		normalized := make(imap.SearchAnd, len(criteria))
		for i, child := range criteria {
			normalized[i] = normalizeSearchCriteria(child, uids)
		}
		return normalized
	case imap.SearchOr:
		return imap.SearchOr{
			Left:  normalizeSearchCriteria(criteria.Left, uids),
			Right: normalizeSearchCriteria(criteria.Right, uids),
		}
	case imap.SearchNot:
		return imap.SearchNot{Criteria: normalizeSearchCriteria(criteria.Criteria, uids)}
	case imap.SearchFuzzy:
		return imap.SearchFuzzy{Criteria: normalizeSearchCriteria(criteria.Criteria, uids)}
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
	WriteFunc func(context.Context, imap.UID) error
	core      *writerCore[imap.UID]
}

// WriteExpunge writes one removed UID. The framework converts it to the
// sequence number current at this exact point in the response.
func (w *ExpungeWriter) WriteExpunge(ctx context.Context, uid imap.UID) error {
	if w == nil {
		return ErrWriterClosed
	}
	if w.core != nil {
		return w.core.writeValue(ctx, uid)
	}
	if w.WriteFunc != nil {
		return w.WriteFunc(ctx, uid)
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
