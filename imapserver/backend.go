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
// you implement, skipping absent interfaces and capabilities explicitly
// withdrawn through CapabilityFilter. The interfaces say what the
// methods are; backendtest says whether you got them right.
//
// [github.com/kiliant/go-imap/imapserver/memory] is a complete worked example
// and is supported, not a toy — it is the backend this project's own conformance
// and interoperability suites run against.
//
// # Selected notifications
//
// The framework applies NOTIFY's selected-mailbox policy to every Updater
// batch; backends continue to publish all changes. Suppressed additions stay
// outside the client's sequence view, removals leave stable placeholders in
// it, and the latest flags per UID are retained. A successful NOTIFY SET
// reports accumulated changes, including when the new registration requests
// NONE. Until they are reported, an explicit HIGHESTMODSEQ checkpoint keeps
// later per-message MODSEQs from concealing those changes on resynchronisation.
// Commands on a removed placeholder succeed without message data, as commands
// on an expunged UID do. The client's own mutations bypass the selected policy;
// an own addition first announces any earlier withheld additions in UID order.
//
// A backend's refusal of a replacement NOTIFY leaves the previous selected
// policy in force. Its session-scoped updater has already been closed; the
// next successful registration installs a new one. Validation refusals before
// the backend call leave both the registration and selected policy untouched.
//
// # Backend errors
//
// The framework finds *imap.Error values with errors.As. It sends their Code,
// CodeArgs and Text to the client in the current command's tagged response:
// Type BAD produces BAD, and every other Type produces NO. Empty Text becomes
// "<operation> failed". Backend authors must keep secrets out of these fields,
// especially Text, because they are client-visible.
//
// Without an *imap.Error, context.Canceled and context.DeadlineExceeded produce
// NO [UNAVAILABLE]; other errors produce NO [SERVERBUG]. Both use the generic
// "<operation> failed" text. The underlying error text is not sent to clients.
// Wrapped errors retain these mappings; an *imap.Error takes precedence over a
// cancellation error in the same chain.
// UNAUTHENTICATE is the exception: an error sends BYE and ends the connection,
// as required by RFC 8437. A failed update-completion FETCH likewise sends BYE
// because the selected view can no longer be reported safely.
package imapserver

import (
	"context"
	"crypto/tls"
	"fmt"
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
	// List with an empty pattern returns the reference's hierarchy delimiter
	// and root name, even when there are no matching mailboxes (RFC 3501
	// section 6.3.8). This is a hierarchy query, not an empty match result.
	//
	// List is also called by the framework during SELECT and EXAMINE on an
	// IMAP4rev2 session, to produce the untagged LIST response RFC 9051 §6.3.2
	// requires, with the selection already installed. An error returned from
	// that call fails the SELECT.
	List(ctx context.Context, writer *ListWriter, reference string, patterns []string, options *ListOptions) error
	// Status answers STATUS for any mailbox, including the selected mailbox.
	// Only the items options requests need be populated; the framework writes
	// what it asked for and nothing else.
	// Status must not expose a commit until its UpdateBatch has been pushed to
	// every updater attached before that commit, including external writes.
	// A change-feed backend must fence this read against publication.
	Status(ctx context.Context, mailbox string, options *StatusOptions) (*imap.StatusData, error)
	// Create makes a mailbox. Creating one that exists is an error, and
	// [imap.CodeAlreadyExists] is the code that says so precisely enough for a
	// client to act on. A trailing hierarchy delimiter is a declaration that
	// the client intends to create children; the backend must create the name
	// without that final delimiter (RFC 9051 section 6.3.4).
	// When OBJECTID is advertised, the framework queries STATUS MAILBOXID after
	// success. Failure to retrieve a valid ID returns NO [SERVERBUG] explicitly
	// saying the mailbox was created; the creation is not rolled back.
	Create(ctx context.Context, mailbox string, options *CreateOptions) error
	// Delete removes a mailbox and the messages in it. RFC 3501 §6.3.4 forbids
	// deleting INBOX, and refusing it here is the backend's job: the framework
	// does not know which name is the inbox in a given namespace.
	Delete(ctx context.Context, mailbox string, options *DeleteOptions) error
	// Rename moves a mailbox, and by RFC 3501 §6.3.5 its inferiors with it.
	// Renaming INBOX is the documented special case: its messages move to the
	// new name, and INBOX itself remains, empty, with its inferiors.
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
	// The framework stages the complete command before this call. Multi-message
	// commands go only to MultiAppendSession. literal is a staged stream, valid
	// only during the call: read it, do not retain it. Returning before EOF is
	// safe for the connection. Stream it into storage rather than buffering the
	// whole message, so memory use stays independent of the client's input size.
	//
	// The returned AppendData carries the new UID and UIDVALIDITY when the
	// backend can supply them, which is what RFC 4315's APPENDUID response code
	// is made of. Returning nil is allowed and simply means no code is sent —
	// but a backend that witnesses UIDPLUS has claimed it will supply them.
	Append(ctx context.Context, mailbox string, literal io.Reader, options *AppendOptions) (*imap.AppendData, error)
	// Select captures Snapshot and attaches updater to that exact state atomically.
	// If it attaches updater and then fails, it must detach before returning.
	// A snapshot must not expose a commit before publication to the previously
	// attached updaters, including external commits; see UpdateBatch.
	Select(ctx context.Context, mailbox string, updater *Updater, options *SelectOptions) (*SelectResult, error)
	// Close releases everything the session holds. The framework calls it once,
	// when the connection ends, including after an error and after
	// UNAUTHENTICATE. No other method is called afterwards, and the session is
	// discarded even if Close returns an error. Cleanup errors go to
	// Options.OnError; a successful UNAUTHENTICATE still answers OK.
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
	// modification-sequence comparison. Store need not include MODSEQ in its
	// output: when required by CONDSTORE, the framework fetches missing values
	// through Fetch before writing the response.
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
	// teardown or UNAUTHENTICATE — after which this handle is not used again,
	// even on error. The updater is closed first. Cleanup uses a fresh
	// context bounded by Limits.CommandTimeout, and errors go to
	// Options.OnError. A completed CLOSE, UNSELECT or UNAUTHENTICATE answers
	// OK; replacing SELECT continues with the new selection.
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
// [CapabilitySupport] witness for "MOVE" and, once selected, this interface.
type MoveMailbox interface {
	Move(ctx context.Context, uids imap.UIDSet, destination string, options *MoveOptions) (*imap.CopyData, error)
}

// CapabilitySupport is the open witness by which a Backend or Session declares
// that it implements the behaviour behind an optional capability. A Backend may
// implement it to describe support before authentication; a Session may
// implement it when support varies by authenticated user, and answers
// alone when it does: the Backend is not consulted.
//
// name is an upper-case capability token such as "CONDSTORE" or "SPECIAL-USE",
// or a backend-only witness such as "BINARY-FETCH" or "STATUS-DELETED".
// STATUS-DELETED promises STATUS DELETED for selected and unselected mailboxes
// and is required by IMAP4REV2. Neither backend-only name is advertised.
// BINARY-FETCH promises the FETCH half of BINARY:
// it promises BINARY, BINARY.PEEK and BINARY.SIZE FETCH support, including CTE
// decoding, decoded partial ranges, CRLF text lines and PEEK/Seen semantics.
// IMAP4REV2 requires it; it does not promise literal8 APPEND support. The wire
// BINARY token remains a separate promise of the full extension.
//
// The framework never advertises a capability using this spoken witness
// unless this returns true for it, so a backend that does not recognise a name
// must return false. Structural witnesses use their optional interfaces instead; see CapabilityFilter for withdrawal.
//
// Bearer authentication requires a Backend witness before any Session exists:
// return true for "AUTH=XOAUTH2" or "AUTH=OAUTHBEARER" only when Authenticate
// accepts that mechanism's token credentials. Each token opts in separately;
// the mechanisms are offered only over TLS. PLAIN and LOGIN need no witness.
// SCRAM also requires a separate Backend witness for each AUTH=SCRAM-* token,
// together with SCRAMCredentials; see that interface for the supported tokens
// and the TLS requirement for -PLUS variants.
//
// This witness is deliberately keyed by an open string rather than by one
// interface per capability: a future RFC is then a new token, which is a data
// change, not a type change.
//
// A few tokens route rather than gate. Their capability is advertised by the
// framework regardless, and true only says that the backend evaluates a
// behaviour the framework would otherwise evaluate itself. false never
// suppresses the advertisement. The one such token is "LIST-EXTENDED", for
// RECURSIVEMATCH; see [ListOptions.SelectRecursiveMatch]. Whether a token
// gates or routes is fixed when it is introduced and does not change.
//
// # Witness contract catalogue
//
// This catalogue includes every backend-dependent descriptor, the routing token
// and backend-only witnesses. Gate means the capability needs the stated
// contract; route changes who evaluates an already available feature. A
// backend-only token is never sent on the wire. A contract naming only an
// optional Session interface is structural: SupportsCapability is not called
// for it. CapabilitySupport in the contract denotes a separate spoken promise;
// return false for every unrecognised token.
//
// States are N (not authenticated), A (authenticated) and S (selected). They
// name the descriptor's advertisement states. /TLS is an additional transport
// requirement. Backend witnesses are queried while deriving CAPABILITY,
// greetings and ENABLE availability, and may be queried more than once.
// IMAP4REV2 also asks its incorporated witnesses in all three states, including
// before login: spoken promises come from Backend then; structural Session and
// selected-mailbox checks are deferred until those objects exist. An
// authenticated Session implementing CapabilitySupport answers alone, including
// false: there is no fallback to Backend. Answers must describe support
// consistently for that connection; use CapabilityFilter to withdraw wire
// capabilities after a structural witness succeeds.
//
// For MOVE, SORT and THREAD, the spoken promise is needed before selection and
// the named selected-mailbox interface must be present after selection.
// REPLACE and QRESYNC also require their named mailbox interfaces when used.
// SORT=DISPLAY inherits SortMailbox through SORT. Bearer and SCRAM witnesses
// are Backend authentication contracts, queried before a Session exists.
//
// The table's obligations supplement the detailed method contracts. Witnessing
// a name never relaxes atomicity, update delivery, streaming or ownership rules.
// Framework-only descriptors are omitted: they impose no extra backend promise.
// TestCapabilityWitnessCatalogue keeps this table level with the registries.
//
//	Token | Mode | Contract | States | Obligation
//	ACL | gate | ACLSession | AS | Supply RFC 4314 GETACL/LISTRIGHTS/MYRIGHTS including texk rights; framework advertises RIGHTS=texk. ACLSetSession separately enables writes.
//	APPENDLIMIT | gate | CapabilitySupport | AS | Return per-mailbox STATUS APPENDLIMIT and enforce upload policy.
//	AUTH=LOGIN | gate | Backend | N | Accept LOGIN password credentials; TLS or explicit insecure-auth policy.
//	AUTH=OAUTHBEARER | gate | CapabilitySupport + Backend | N/TLS | Authenticate OAUTHBEARER token credentials.
//	AUTH=PLAIN | gate | Backend | N | Accept PLAIN password credentials; TLS or explicit insecure-auth policy.
//	AUTH=SCRAM-SHA-1 | gate | CapabilitySupport + SCRAMCredentials | N | Supply SHA-1 SCRAM derivations and accept verified credentials.
//	AUTH=SCRAM-SHA-1-PLUS | gate | CapabilitySupport + SCRAMCredentials | N/TLS | Supply SHA-1 SCRAM derivations with TLS channel binding.
//	AUTH=SCRAM-SHA-256 | gate | CapabilitySupport + SCRAMCredentials | N | Supply SHA-256 SCRAM derivations and accept verified credentials.
//	AUTH=SCRAM-SHA-256-PLUS | gate | CapabilitySupport + SCRAMCredentials | N/TLS | Supply SHA-256 SCRAM derivations with TLS channel binding.
//	AUTH=XOAUTH2 | gate | CapabilitySupport + Backend | N/TLS | Authenticate XOAUTH2 token credentials.
//	BINARY | gate | CapabilitySupport | AS | Decode BINARY FETCH and accept literal8 APPEND; full extension promise.
//	BINARY-FETCH | backend-only | CapabilitySupport | NAS | Supply decoded FETCH, decoded partials, SIZE, text CRLF and PEEK/Seen.
//	CATENATE | gate | CatenateSession | AS | Resolve authorized IMAP URLs into message-part streams.
//	CHILDREN | gate | CapabilitySupport | AS | Return accurate child attributes in LIST data.
//	CONDSTORE | gate | CapabilitySupport | AS | Supply monotonic modseqs, conditional STORE and changed-since FETCH.
//	CREATE-SPECIAL-USE | gate | CapabilitySupport | AS | Honor CREATE special-use attributes; requires SPECIAL-USE.
//	FILTERS | gate | FilterSession | AS | List available search filters and expand FILTER criteria.
//	I18NLEVEL=1 | gate | CapabilitySupport | AS | Evaluate internationalized comparisons with i;unicode-casemap.
//	I18NLEVEL=2 | gate | ComparatorSession | AS | List and select comparators; requires I18NLEVEL=1.
//	IMAP4REV2 | gate | rev2Incorporated | NAS | Satisfy every incorporated member, including BINARY-FETCH and STATUS-DELETED.
//	JMAPACCESS | gate | CapabilitySupport | AS | Provide the account JMAP access information required by the extension.
//	LANGUAGE | gate | LanguageSession | AS | List supported language tags and adopt a requested language.
//	LIST-EXTENDED | route | CapabilitySupport | AS | Evaluate RECURSIVEMATCH natively; false uses framework evaluation.
//	LIST-METADATA | gate | MetadataSession | AS | Supply requested LIST metadata; requires METADATA.
//	LIST-MYRIGHTS | gate | ACLSession | AS | Supply MYRIGHTS for each listed mailbox; requires ACL.
//	LOGIN-REFERRALS | gate | CapabilitySupport | N | Support login referral response codes.
//	MAILBOX-REFERRALS | gate | CapabilitySupport | AS | Support mailbox referral response codes.
//	METADATA | gate | MetadataSession | AS | Get and set mailbox and server metadata with depth/size semantics.
//	METADATA-SERVER | gate | MetadataSession | AS | Get and set server-scope metadata.
//	MOVE | gate | CapabilitySupport + MoveMailbox | AS | Provide atomic MOVE with the selected mailbox interface.
//	MULTIAPPEND | gate | MultiAppendSession | AS | Commit complete batches atomically or leave the mailbox unchanged.
//	MULTISEARCH | gate | MultiSearchSession | AS | Evaluate extended searches across the requested mailboxes.
//	NAMESPACE | gate | NamespaceSession | AS | Return namespace prefixes and delimiters for the account.
//	NOTIFY | gate | NotifySession | AS | Validate and deliver the requested mailbox event subscriptions.
//	OBJECTID | gate | CapabilitySupport | AS | Return stable MAILBOXID, EMAILID and THREADID data, including STATUS immediately after CREATE and SELECT.
//	PREVIEW | gate | CapabilitySupport | AS | Produce message previews with PREVIEW/LAZY semantics.
//	QRESYNC | gate | CapabilitySupport + QResyncMailbox | AS | Resynchronize known UIDs/modseqs and report vanished/changed messages.
//	QUOTA | gate | QuotaSession | AS | Return quota roots and resource usage/limits.
//	QUOTASET | gate | QuotaSetSession | AS | Set quota resource limits; requires QUOTA and at least one unfiltered QUOTA=RES-* witness.
//	QUOTA=RES-STORAGE | gate | CapabilitySupport + QuotaSession | AS | Handle STORAGE quotas and return STATUS DELETED-STORAGE in octets.
//	QUOTA=RES-MESSAGE | gate | CapabilitySupport + QuotaSession | AS | Handle MESSAGE quotas and return STATUS DELETED.
//	QUOTA=RES-MAILBOX | gate | CapabilitySupport + QuotaSession | AS | Handle MAILBOX quotas.
//	QUOTA=RES-ANNOTATION-STORAGE | gate | CapabilitySupport + QuotaSession | AS | Handle ANNOTATION-STORAGE quotas.
//	REPLACE | gate | CapabilitySupport + ReplaceMailbox | AS | Provide atomic selected-message replacement into the destination mailbox.
//	SASL-IR | gate | Backend | N | Have an authentication backend; framework handles initial-response syntax.
//	SAVEDATE | gate | CapabilitySupport | AS | Return save dates and evaluate saved-date SEARCH criteria.
//	SEARCH=FUZZY | gate | CapabilitySupport | AS | Evaluate FUZZY criteria using the supported matching policy.
//	SORT | gate | CapabilitySupport + SortMailbox | AS | Evaluate SORT keys on the selected mailbox.
//	SORT=DISPLAY | gate | CapabilitySupport + SortMailbox | AS | Evaluate DISPLAYFROM/DISPLAYTO sorting; requires SORT.
//	SPECIAL-USE | gate | CapabilitySupport | AS | Return accurate LIST special-use attributes.
//	STATUS=SIZE | gate | CapabilitySupport | AS | Return the mailbox SIZE status item.
//	STATUS-DELETED | backend-only | CapabilitySupport | NAS | Return the count of Deleted messages in STATUS, including unselected mailboxes; required by IMAP4REV2.
//	THREAD=ORDEREDSUBJECT | gate | CapabilitySupport + ThreadMailbox | AS | Produce the ORDEREDSUBJECT thread tree.
//	THREAD=REFERENCES | gate | CapabilitySupport + ThreadMailbox | AS | Produce the REFERENCES thread tree.
//	UIDPLUS | gate | CapabilitySupport | AS | Return assigned UIDs in APPEND/COPY results and honor UID EXPUNGE.
//	UNAUTHENTICATE | gate | UnauthenticateSession | AS | Release authenticated state and reset the session.
//	URL-PARTIAL | gate | CapabilitySupport + URLAuthSession | AS | Resolve URL byte ranges; requires URLAUTH.
//	URLAUTH | gate | URLAuthSession | AS | Generate/reset authorization tokens and return owned URLFETCH bodies.
//	UTF8=APPEND | gate | CapabilitySupport | AS | Accept messages with UTF-8 headers; requires UTF8=ACCEPT.
//	WITHIN | gate | CapabilitySupport | AS | Evaluate OLDER/YOUNGER criteria against internal dates.
//
// Backend implementations must make SupportsCapability safe for concurrent use.
type CapabilitySupport interface {
	SupportsCapability(ctx context.Context, name string, options *CapabilitySupportOptions) bool
}

// CapabilitySupportOptions configures a capability-support query. A nil
// pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type CapabilitySupportOptions struct{ _ struct{} }

// CapabilityFilter optionally withdraws backend-backed capabilities after their
// ordinary spoken or structural witnesses succeed. It cannot grant support.
// Implement it on a Backend for pre-authentication policy, or on a Session for
// per-user or wrapper policy. A Session implementing it answers alone; otherwise
// the Backend is consulted. Without either implementation, nothing is filtered.
// CapabilitySupport retains its existing meaning, including for structural
// capabilities it does not witness.
//
// name is the upper-case descriptor token, such as "ACL" or "QUOTA=RES-STORAGE";
// a token whose advertisement carries a value is named without it. Return true for tokens that should
// remain available, including names the filter does not intend to suppress.
// False also withdraws dependent capabilities and an IMAP4REV2 umbrella that
// incorporates the withdrawn behaviour. Framework-only tokens such as
// IMAP4REV1, STARTTLS, LOGINDISABLED, ENABLE and LIST-EXTENDED are not filtered.
// The framework may ask about an incorporated token while deriving IMAP4REV2
// even when that token is not advertised in the current connection state.
//
// Like CapabilitySupport, answers must be consistent for the same connection
// state. Backend implementations must be safe for concurrent use.
type CapabilityFilter interface {
	AllowCapability(ctx context.Context, name string, options *CapabilityFilterOptions) bool
}

// CapabilityFilterOptions configures a capability-filter query. Nil selects
// the defaults. Construct with keyed fields only; fields may be added later.
type CapabilityFilterOptions struct{ _ struct{} }

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
	// SelectRecursiveMatch modifies the other selection options: a name
	// matching the patterns is also returned when it does not meet the
	// selection criteria itself but a descendant does, whether or not that
	// descendant matches the patterns. Such a result, and any other result
	// with a descendant that meets the criteria, sets [imap.ListData]'s
	// ChildInfo to the options the descendant met, such as "SUBSCRIBED". A
	// returned name that is not a mailbox carries \NonExistent. RFC 5258
	// sections 3 and 3.5. ChildInfo is redundant when every descendant
	// meeting the criteria is itself returned; the backend should then omit
	// it, and omit a name returned only for it (section 3.5, example 9).
	//
	// The framework sets it only together with Subscribed, the one base
	// selection option it implements, and answers any other combination BAD
	// (RFC 5258 section 6).
	//
	// It also sets it only for a backend that witnesses "LIST-EXTENDED"
	// through [CapabilitySupport], which says the backend evaluates it; a
	// backend without the witness never receives it. For such a backend the
	// framework evaluates RECURSIVEMATCH itself, from List calls without it.
	// It starts from the backend's own answer to the command without the
	// option, so the result never holds less than that, and adds the parents
	// of descendants that meet the base options, found through List for "*",
	// unless all of those descendants are returned as well. Limits follow,
	// and only the added parents and ChildInfo are affected. The framework
	// decides which parents match the patterns by taking the canonical
	// pattern to be the reference followed by the pattern, unless the pattern
	// starts with the hierarchy delimiter (RFC 3501 section 6.3.8), and
	// matches it case-sensitively, INBOX included. And a name the backend
	// leaves out of "*" gets no added parents. A backend for which any of
	// this matters evaluates the option itself and witnesses "LIST-EXTENDED".
	SelectRecursiveMatch bool `imapfeature:"list-extended"`
	// SelectSpecialUse restricts the result to special-use mailboxes: those
	// with a use attribute such as \Archive. RFC 6154 section 2.
	//
	// The framework also drops every other result before writing it, so a
	// backend that ignores the option is still correct on the wire. Honouring
	// it saves producing results that are then discarded.
	SelectSpecialUse bool `imapfeature:"list-special-use"`
	// ReturnSubscribed asks for the \Subscribed attribute on returned
	// mailboxes. RFC 5258 section 3.
	ReturnSubscribed bool `imapfeature:"list-extended"`
	// ReturnChildren asks for the \HasChildren and \HasNoChildren attributes.
	// RFC 3348, incorporated into IMAP4rev2.
	ReturnChildren bool `imapfeature:"list-children"`
	// ReturnSpecialUse asks for special-use attributes such as \Archive.
	// RFC 6154 section 2.
	ReturnSpecialUse bool `imapfeature:"list-special-use"`
	_                struct{}
}

// StatusOptions configures STATUS. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type StatusOptions struct {
	// Items are the requested status values. For framework-supplied calls, each
	// item is in the enabled revision's baseline or belongs to a capability
	// this session advertised. Unknown or unadvertised client items receive
	// BAD before Status is called, including through LIST-STATUS.
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
	// Size is the ordinary literal's declared octet count when HasSize is true.
	// It is available before the backend reads the stream. It does not guarantee
	// that the reader will supply that many bytes: reads may fail or end early.
	Size int64
	// HasSize distinguishes a known size, including zero, from an unknown one.
	// The framework sets it for ordinary APPEND literals and leaves it false
	// for CATENATE, whose assembled size is not declared by the client.
	HasSize bool
	_       struct{}
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
	// counts when present. The framework supplies only static, validated sets:
	// no wildcards, strictly ascending sequence-match data and equal expanded
	// counts. Invalid wire parameters receive BAD before Select runs.
	SeqMatchSeqNums imap.SeqSet `imapfeature:"qresync"`
	// SeqMatchUIDs pairs positionally with SeqMatchSeqNums.
	SeqMatchUIDs imap.UIDSet `imapfeature:"qresync"`
	_            struct{}
}

// FetchOptions configures FETCH. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type FetchOptions struct {
	// MutationOptions identifies this FETCH's implicit \Seen changes. Backends
	// copy Origin into the UpdateBatch that publishes those changes. Backends
	// that leave the batch origin at zero remain correct; the framework treats
	// their updates as external changes and still delivers them.
	MutationOptions
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
	// Silent reports the client's .SILENT suffix. It suppresses the new FLAGS
	// the command would return, never the mutation or the updates other
	// sessions see. The framework applies it: a backend may write nothing, or
	// what it writes without Silent. A conditional store (HasUnchangedSince)
	// still reports each stored message's MODSEQ, as RFC 7162 section 3.1.3
	// requires; the framework keeps it from what the backend wrote and
	// fetches it for any stored message the backend did not report.
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
	// Status is the sole source of SELECT values for this atomic state:
	// Mailbox, Flags, PermanentFlags, NumMessages, NumRecent, UIDValidity,
	// UIDNext, Unseen, ReadOnly, HighestModSeq and NoModSeq. NumMessages must
	// equal len(UIDs), NumRecent must not exceed it, UIDValidity must be nonzero
	// and UIDNext must exceed every visible UID. The framework copies its flag
	// slices before retaining them. NoModSeq suppresses HIGHESTMODSEQ; RECENT
	// and UNSEEN are reported only for IMAP4rev1.
	Status imap.MailboxStatus
	// Revision starts the selected updater's mandatory Before/After chain.
	Revision MailboxRevision
	_        struct{}
}

// MailboxRevision is an opaque, comparable backend revision. Backends should
// use their natural transaction identifier when possible. The empty value is
// valid; revisions need only be comparable and form the Before/After chain.
type MailboxRevision string

// ChangeToken identifies the currently executing mutating command. Zero means
// that no framework command supplied an origin.
type ChangeToken uint64

// UpdateBatch is every observable change from one atomic backend commit.
// Publication is synchronous: before a mutating call returns, the backend
// must Push its committed changes, with the supplied MutationOptions.Origin,
// to every attached updater. This also applies to commits followed by an error.
// Status and Select must not expose a commit until its batch has been pushed
// to every updater attached before that commit, including external changes.
// A backend using an asynchronous change feed must fence these calls against
// publication. Push never waits for the framework's event loop, so publication
// can run under the same serialization as the commit and snapshot reads.
// Changes are applied in slice order and batches are never merged.
// Removal announcements may be delayed where the protocol forbids them;
// later additions and flags still use the client's unrenumbered sequence view.
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

// Update is one selected-mailbox change. Message changes are UID-keyed. The set is open to this
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

// UpdateRecent replaces the number of messages recent to this selection.
// Backends track per-selection RECENT ownership and publish this after additions
// or removals in the same batch. The count must not exceed the visible message
// count. The framework suppresses this response in IMAP4rev2 sessions.
// Construct with keyed fields only; fields may be added in a future release.
type UpdateRecent struct {
	// NumRecent is the absolute count recent to this selection after the batch.
	NumRecent uint32
	_         struct{}
}

func (*UpdateRecent) update() {}

// UpdateFlags replaces one message's flags and optionally reports its modseq.
// Construct with keyed fields only; fields may be added in a future release.
type UpdateFlags struct {
	// UID identifies the affected message.
	UID imap.UID
	// Flags is the message's complete new flag set.
	Flags []imap.Flag
	// ModSeq is the new modification sequence, or zero when unavailable.
	// After CONDSTORE is enabled, the framework fetches current FLAGS and
	// MODSEQ together when this value is zero.
	// If that FETCH fails, the framework reports the error to Options.OnError
	// and ends the connection with BYE so the client can resynchronise.
	ModSeq uint64
	_      struct{}
}

func (*UpdateFlags) update() {}

// UpdateMailboxFlags replaces the selected mailbox's complete set of applicable
// flags. A backend must publish this update before reporting a newly created
// keyword in [UpdateFlags], so the framework can satisfy RFC 3501 section 7.2.6.
// Construct with keyed fields only; fields may be added in a future release.
type UpdateMailboxFlags struct {
	// Flags is the mailbox's complete new set of applicable flags.
	Flags []imap.Flag
	_     struct{}
}

func (*UpdateMailboxFlags) update() {}

// UpdateExpunge removes one UID. EXPUNGE updates remain individually ordered.
// Construct with keyed fields only; fields may be added in a future release.
type UpdateExpunge struct {
	// UID identifies the removed message.
	UID imap.UID
	_   struct{}
}

func (*UpdateExpunge) update() {}

// UpdateVanished removes one or more UIDs from the live selection. Unknown
// UIDs and additions not yet announced to the client produce no response.
// The framework projects removals to EXPUNGE or VANISHED according to the
// session's enabled features; a backend need not track QRESYNC negotiation.
// Construct with keyed fields only; fields may be added in a future release.
type UpdateVanished struct {
	// UIDs is a concrete, non-dynamic set of removed UIDs.
	UIDs imap.UIDSet
	// Earlier is retained for source compatibility but ignored in live update
	// batches. EARLIER is legal only in SELECT/FETCH resynchronisation reports;
	// use QResyncResult.Vanished for those.
	Earlier bool
	_       struct{}
}

func (*UpdateVanished) update() {}

// SearchResult is the UID-keyed result returned by SelectedMailbox.Search.
// Construct with keyed fields only; fields may be added in a future release.
type SearchResult struct {
	// UIDs are the matching message identifiers, without duplicates. Their
	// order is unrestricted.
	UIDs []imap.UID
	// ModSeq is the highest modification sequence among the matching messages,
	// or zero when the backend does not track them. The framework reports it
	// only when the client asked for it and CONDSTORE is active.
	// RFC 7162 section 3.1.5.
	ModSeq uint64
	_      struct{}
}

// SearchQuery is a UID-normalised SEARCH tree. The framework and NewSearchQuery
// construct values containing no SearchSeqNum, SearchSavedResult or
// unsubstituted SearchFilter.
// The framework and backendtest always pass non-nil queries to backends; a
// backend is not required to accept a nil query or a query's zero value.
type SearchQuery struct{ criteria imap.SearchCriteria }

// SearchQueryOptions configures NewSearchQuery. A nil pointer selects defaults.
// Construct with keyed fields only; fields may be added in a future release.
type SearchQueryOptions struct{ _ struct{} }

// NewSearchQuery constructs a query for direct backend calls, including tests.
// Nil criteria mean ALL. It copies the tree and its UID sets, rejecting sequence
// numbers, saved-result references, unsubstituted filters, unclassified criteria
// and malformed containers at every depth. Trees deeper than 64 levels, including
// cyclic trees, are refused.
//
// This constructor has no session and does not negotiate capabilities. The
// caller must restrict extension keys to those the backend witnesses. Queries
// supplied by the framework also carry the capability guarantee on Criteria.
func NewSearchQuery(criteria imap.SearchCriteria, options *SearchQueryOptions) (*SearchQuery, error) {
	if criteria == nil {
		criteria = imap.SearchAll
	}
	criteria, err := copyBackendSearchCriteria(criteria, 0)
	if err != nil {
		return nil, err
	}
	if err := imapcodec.ValidateSearchCriteria(criteria); err != nil {
		return nil, err
	}
	return &SearchQuery{criteria: criteria}, nil
}

func copyBackendSearchCriteria(criteria imap.SearchCriteria, depth int) (imap.SearchCriteria, error) {
	if depth > 64 {
		return nil, fmt.Errorf("imapserver: SEARCH query exceeds 64 nesting levels")
	}
	if _, classified := criterionCapability(criteria); !classified {
		return nil, fmt.Errorf("imapserver: unsupported SEARCH criterion %s", describeCriterion(criteria))
	}
	switch criteria.(type) {
	case imap.SearchSeqNum, imap.SearchFilter, imap.SearchSavedResult:
		return nil, fmt.Errorf("imapserver: SEARCH query requires resolved UIDs, saved results and filters")
	}
	if children, rebuild := searchCriteriaChildren(criteria); rebuild != nil {
		copied := make([]imap.SearchCriteria, len(children))
		for i, child := range children {
			var err error
			copied[i], err = copyBackendSearchCriteria(child, depth+1)
			if err != nil {
				return nil, err
			}
		}
		return rebuild(copied), nil
	}
	if uid, ok := criteria.(imap.SearchUID); ok {
		uid.Set = slices.Clone(uid.Set)
		return uid, nil
	}
	return criteria, nil
}

// newSearchQuery constructs the only SEARCH tree that may cross the backend
// boundary. uids is the selected mailbox's sequence-order snapshot.
func newSearchQuery(criteria imap.SearchCriteria, uids []imap.UID, saved imap.UIDSet) *SearchQuery {
	return &SearchQuery{criteria: normalizeSearchCriteria(criteria, uids, saved)}
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

func searchMentionsSavedResult(criteria imap.SearchCriteria) bool {
	if children, rebuild := searchCriteriaChildren(criteria); rebuild != nil {
		return slices.ContainsFunc(children, searchMentionsSavedResult)
	}
	_, ok := criteria.(imap.SearchSavedResult)
	return ok
}

func normalizeSearchCriteria(criteria imap.SearchCriteria, uids []imap.UID, saved imap.UIDSet) imap.SearchCriteria {
	if children, rebuild := searchCriteriaChildren(criteria); rebuild != nil {
		normalized := make([]imap.SearchCriteria, len(children))
		for i, child := range children {
			normalized[i] = normalizeSearchCriteria(child, uids, saved)
		}
		return rebuild(normalized)
	}
	switch criteria := criteria.(type) {
	case imap.SearchSavedResult:
		if saved.IsEmpty() {
			return imap.SearchNot{Criteria: imap.SearchAll}
		}
		return imap.SearchUID{Set: slices.Clone(saved)}
	case imap.SearchUID:
		// A UID wildcard denotes the highest UID in this selected view,
		// including reversed ranges such as UIDNEXT:*. Resolve it before
		// backends evaluate the set without that view's maximum.
		if !criteria.Set.Dynamic() {
			return criteria
		}
		var set imap.UIDSet
		if len(uids) > 0 {
			for _, uid := range uids {
				if uidSetContains(criteria.Set, uid, uids[len(uids)-1]) {
					set.AddNum(uid)
				}
			}
		}
		return imap.SearchUID{Set: set.Normalized()}
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
// [imap.SearchFilter], which is substituted for the criteria it names.
// [imap.SearchSavedResult] is replaced with the saved UID set, or
// [imap.SearchNot]{Criteria: imap.SearchAll} when the result is unset or empty.
// All three guarantees hold at every nesting depth,
// including inside [imap.SearchNot], [imap.SearchOr] and [imap.SearchFuzzy].
// A backend never has to handle these keys, allowing the root package to grow
// new [imap.SearchCriteria]
// implementations without breaking backends compiled against an earlier
// version. See docs/API-STABILITY.md section 10.
//
// The same guarantee holds for [MultiSearchSession.MultiSearch], which takes
// criteria directly rather than a query: when the selection is the only source
// (no IN clause, or "selected" alone), sequence numbers and saved-result
// references resolve against it. With any other source those keys are refused,
// so a backend never receives an unresolvable reference.
//
// A criterion outside that guarantee reaching a backend is a framework bug, not
// a case for the backend to interpret. TestSearchQueryNormalisationGuarantee
// enforces it for every command that builds a query, and
// TestSearchCriteriaContainersAreTraversed fails if a future container key is
// added to package imap without the traversal learning to descend into it.
//
// # Capability
//
// For queries supplied by the framework, every criterion in the tree belongs
// to the IMAP4rev1 baseline or to a
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
//
// Attrs, Delimiter and Mailbox are written. ChildInfo is written as the
// CHILDINFO extended data item (RFC 5258 section 3.5), but only when the LIST
// command used RECURSIVEMATCH and only for the base selection options it
// requested; the framework drops anything else, so a backend may set it
// whenever it knows it.
//
// A previous name in the result's OldName field is not written: OLDNAME has no
// meaning in a LIST result. RFC 5465 section 5.4 gives it one in a NOTIFY
// MailboxName event, and RFC 9051 section 6.3.2 in the LIST answering a SELECT
// of a non-canonical name. The framework will write it itself in those cases,
// where what the session negotiated decides, as for every other extension
// data. Extended is never written: raw items from a backend can be neither
// validated nor gated on what the client asked for.
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
// Item keys are case-insensitive, including UID, FLAGS and extension items.
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
// A UID is a scalar with nowhere to grow, which leaves this the only streaming
// method on the mandatory contract where nothing could ever be said about an
// individual write.
//
// No current RFC stresses it, and this deliberately names none. RFC 7162's
// VANISHED (EARLIER) is the near miss and is worth recording as excluded rather
// than left for the next reader to reach for: section 3.2.10 confines EARLIER
// to the QRESYNC resynchronisation response and to UID FETCH (VANISHED), and
// forbids it when VANISHED results from EXPUNGE or UID EXPUNGE — which is the
// only path that constructs an [ExpungeWriter]. That distinction already lives
// on [UpdateVanished] and QResyncResult, neither of which routes through here.
//
// Construct with keyed fields only; fields may be added in a future release.
type WriteExpungeOptions struct{ _ struct{} }

// WriteExpunge reports one removed UID. The framework validates the UID here;
// it emits removals from the synchronously published UpdateBatch after the
// backend call returns, converting UIDs through the ordered revision chain.
// It suppresses removal responses for CLOSE.
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
// Publish before the mutating call returns or Status/Select exposes the commit;
// see UpdateBatch. An error from one updater does not prevent publication to
// the other attached updaters or roll back an already committed change.
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
