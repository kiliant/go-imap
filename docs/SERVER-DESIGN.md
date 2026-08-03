# Server framework — design

**Status: PROPOSED, revision 5. Not approved. No `imapserver` code may be
written against this document until a human approves it** (`CLAUDE.md`, and the
"Do not start" section of [T16](tasks/T16-server-framework.md)).

Revision 2 (2026-08-03) responded to a review of revision 1: the concurrency
model in §4 was internally inconsistent and was rewritten; §2 gained a compilable
interface sketch and renamed `Mailbox` to `SelectedMailbox`; §3 gained a
capability descriptor model; §5 resolved a contradiction about who owns SEARCH;
§9 **reversed** revision 1's versioning recommendation to a nested module; §1 was
new (protocol baseline); update semantics, resource accounting and security
testing were substantially expanded. §0 records a finding that materially
enlarges T17.

Revision 3 (2026-08-03) responds to a review of revision 2, which found one
architectural blocker: **revision 2 promised the framework owns the
sequence-number map, then gave it no way to build one atomically.** §2 now makes
`Select` take the `*Updater` and return a snapshot, so capture and attachment are
a single backend operation. Also in §2: SEARCH criteria reach the backend
UID-normalised via a framework-only `*SearchQuery`. §4 decides the writer
topology (synchronous, on the event loop), corrects the coalescing rule
(`VANISHED` may be coalesced; `EXPUNGE` may not), and replaces blanket echo
suppression with per-operation origin accounting. §3 gives the options/capability
pairing two mechanical gates. §1 stops describing rev2 as a list of legacy
capability tokens.

Revision 4 (2026-08-03) responds to a review of revision 3, which confirmed the
rev2/BINARY split §1 had flagged as unverified and raised one contract gap.
**"Updates carry the revision they follow" was not implementable**: one backend
commit produces several events, so per-event revisions leave the before/after
sense, duplicate tokens and coalescing behaviour all undefined. §4 now publishes
**batches** with an explicit `Before → After` chain, and defines updater detach
on `Select` failure. §3 splits capability descriptors from **feature**
descriptors, because the BINARY case proves an options field can be activated by
a revision *or* a capability and binding it to one token would force invented
pseudo-capabilities. §2 requires `MoveMailbox` before advertising `MOVE` *or*
`IMAP4REV2`, since the framework must not synthesise MOVE from
Copy+Store+Expunge. Plus five corrections: the `Session` method count (ten, not
eight), `SelectedMailbox` scoped to the backend's handle rather than the whole
selected state, `ChangeToken` given an explicit transport into mutating calls,
the UID snapshot given a resource bound, and overflow handling made independent
of an event loop that may be blocked mid-write.

Revision 5 (2026-08-03) responds to a review of revision 4, which recommended
approval subject to one contract correction and two stale sentences.
**Revision 4's batch-coalescing rule destroyed origin information**: a batch
carries one `Origin`, so merging two batches with different origins yields a
batch whose origin is untrue and silently mis-suppresses changes, even though the
revision chain is preserved. §4 now forbids merging batches outright and fixes
the order of operations — validate the chain, account origins, *then* coalesce
wire-level changes — so a bandwidth optimisation cannot corrupt transaction
accounting. Also: the updater lifetime no longer contradicts itself (invalidated
on every *failed* `Select` path; valid until teardown after a successful one),
origin matching is defined as exact rather than merely non-zero, `Changes` apply
in slice order, and the sketch's `SelectedMailbox` comment is aligned with §2's
ownership split.

Approval means approving §2, §6, §8 and §9 specifically. The rest is analysis
supporting them.

---

## 0. What already exists, and which direction it faces

The architecture was built to make this addition cheap, and partly it did.
`package imap` holds vocabulary and does no I/O, so the server reuses it without
touching a signature. That worked for the types that are actually in it.

"Reusable" splits three ways, and the second and third columns are the project:

| Component | Direction | Server can… |
|---|---|---|
| `package imap` — flags, envelope, body structure, search criteria, fetch/status items, response codes, `*Error`, `SeqSet`/`UIDSet` | none (pure data) | **use as-is** |
| `internal/imapwire` primitives — `Atom`, `Astring`, `String`, `Number`, `List`, `Literal`, `Mailbox`, `Flag`, UTF-7, limits, deadlines | none (grammar) | **use as-is** |
| `internal/imapsasl`, `internal/saslprep`, `internal/unicodenorm` | none | **use as-is** |
| `imapwire.BeginResponse`, `ExpectRespCond`, `ExpectRespText` | decode responses | needs the **mirror**: decode *commands*, encode resp-text |
| `imapclient/search.go` — `writeSearchCriteria` | encode only | needs the **mirror**: decode criteria |
| `imapclient/fetch.go` — `readFetchResponse` | decode only | needs the **mirror**: encode fetch data |
| `imapclient/structure.go` — `readEnvelope`, `readBodyStructure` | decode only | needs the **mirror**: encode |
| **generating** ENVELOPE/BODYSTRUCTURE from stored RFC 5322 bytes | — | **does not exist in either direction** (§5) |
| **evaluating** `imap.SearchCriteria` as a predicate | — | **does not exist in either direction** (§5) |

The last two rows are not mirrors of anything. A client only ever *receives* an
ENVELOPE and *asks* a SEARCH; it never constructs one or answers one. Those are
net-new subsystems, not reversed code.

The mirror rows are about 1 400 lines across `imapclient/{fetch,search,structure}.go`.

**None of this depends on the backend abstraction**, so it is specifiable and
buildable while §2 is still under review. It is also the bulk of the work.

### The finding that enlarges T17

Revision 1 assumed `package imap` was already bidirectionally complete and that
T17 would likely find nothing. **That assumption was wrong, and checking it was
cheap.** Every response *data* type lives in `imapclient`, not in `package imap`:

```
imapclient.StatusData      imapclient.ListData        imapclient.AppendData
imapclient.CopyData        imapclient.MailboxStatus   imapclient.NamespaceData
imapclient.QuotaData       imapclient.ACLData         imapclient.ESearchData
imapclient.VanishedData    … ~35 types in total
```

A backend interface cannot name any of them without `imapserver` importing
`imapclient`, which inverts the dependency graph the layering exists to protect.

Three consequences, all T17's:

1. **The shared ones must move to `package imap`.** This is *not* a breaking
   change if done properly: leaving `type StatusData = imap.StatusData` behind in
   `imapclient` preserves type identity, so every caller and every keyed struct
   literal keeps compiling. It is the technique the standard library used to
   relocate `context.Context`. `apidiff` should report it as compatible, and that
   claim gets verified rather than assumed.
2. **Some carry unexported fields, and those are a hard blocker.** `AppendData`
   and `CopyData` have unexported state (a "response code was received" flag). A
   type with unexported fields can only be fully constructed by its own package,
   so even after moving to `package imap`, a server in `imapserver` still cannot
   produce a complete value. **Verdict: an exported presence field, not a
   constructor** — a constructor preserving hidden state leaves the type readable
   by anyone but writable only through a blessed path, and a pure vocabulary type
   should be fully visible and constructible from both directions.
3. **At least one field is genuinely client-only.** `MailboxStatus.UIDValidityChanged`
   is a client-side derived observation — "this differs from what you last saw".
   No server produces it. **Verdict: it stays client-side**, rather than moving
   with a "server always leaves this false" note. A field one of the two users
   cannot meaningfully produce is evidence the shared type has the wrong
   boundary, and pre-v1 is when to remove the asymmetry instead of documenting it
   forever.

This is exactly the class of defect T17 exists to catch, at a scale revision 1
did not anticipate. It is also the strongest available argument for doing this
scoping before the freeze rather than after.

### Where the mirrors live

Not in `imapclient` (inverts the graph), not in `package imap` (which performs
no I/O). So:

```
github.com/kiliant/go-imap        package imap          vocabulary, no I/O
    ├── internal/imapwire         grammar primitives, both directions
    ├── internal/imapcodec        semantic codec for imap types, BOTH directions
    ├── internal/imapmessage      message analysis: generation + evaluation (§5)
    ├── internal/imapsasl, saslprep, unicodenorm
    ├── imapclient                the client
    └── imapserver                the server framework
```

`imapclient` migrates onto `internal/imapcodec` rather than keeping a private
copy. Two BODYSTRUCTURE implementations that must agree byte-for-byte is a bug
generator, and the client's is the one with fixtures, a fuzz corpus and five
servers of interop behind it — so the migration *moves* code, it does not rewrite
it.

That migration edits `imapclient/{fetch,search,structure}.go`, which T06 owns.
See §10 for why that is allowed now and was not allowed then.

---

## 1. Protocol baseline

Revision 1 never stated this and used "IMAP4rev1" and rev2 facilities
interchangeably. Fixed:

**The server implements the IMAP4rev1 wire baseline, advertises `IMAP4REV2`, and
switches to rev2 behaviour on `ENABLE IMAP4rev2`** — the mirror of the client's
settled decision in `ARCHITECTURE.md`, for the mirror reason: a rev2-only server
cannot be tested by most deployed clients, and a rev1-only server is obsolete on
arrival.

Concretely, per RFC 9051 §2 and its appendix on differences:

- `RECENT` and `\Recent` are maintained for rev1 sessions; not sent in rev2.
- `LSUB` is served in rev1; rev2 clients use `LIST (SUBSCRIBED)`. Both map to the
  same backend `List` call with a selection option.
- **rev2 is a set of incorporated behaviours, not a list of legacy capability
  tokens**, and the two must not be conflated. RFC 9051 folds in functionality
  that used to be separate extensions — ESEARCH, SEARCHRES, LIST-EXTENDED,
  LIST-STATUS, SPECIAL-USE, UIDPLUS, MOVE, ENABLE, IDLE, NAMESPACE, SASL-IR,
  LITERAL-, CHILDREN, UNSELECT, STATUS=SIZE among them. `IMAP4REV2` is
  advertised only when **all** of the incorporated behaviour is implemented;
  advertising it otherwise is a lie the client cannot detect.
- **`BINARY` is the case where the distinction bites.** The FETCH side —
  `BINARY`, `BINARY.PEEK`, `BINARY.SIZE` — is incorporated into IMAP4rev2. RFC
  3516's **APPEND** side is not. The separate `BINARY` capability token signals
  full RFC 3516 support *including binary APPEND*, so **implementing rev2 does
  not license advertising `BINARY`**. An earlier revision of this document listed
  `BINARY` among "mandatory rev2 capabilities", which was wrong in both
  directions: it is not a capability rev2 requires, and advertising it claims
  more than rev2 provides.

  Corroborated by RFC 9051's own restriction that unencoded binary strings are
  not permitted "unless returned in a `<literal8>` in response to a
  `BINARY.PEEK[...]` or `BINARY[...]` FETCH data item" — the FETCH direction and
  no other.
- rev2 `STATUS SIZE` semantics and UTF-8 handling follow the enabled revision.
- `CHECK` is accepted and treated as `NOOP`.

The enabled revision is per-connection state, so response encoding is a function
of the session, not a global.

---

## 2. The backend abstraction

The hard problem, and the one that decides whether this framework can be frozen.

A backend abstraction is a permanent compatibility commitment. Every new
extension wants to add a method to it, and adding a method to an exported
interface breaks every external implementer — precisely what
[API-STABILITY](API-STABILITY.md) §4 exists to prevent.

### The extension pressure, named concretely

*Can RFC N+1 be added without a breaking change?* We already know what N+1
through N+9 want, from the client we just built:

| Extension | What the backend must expose beyond rev1 |
|---|---|
| CONDSTORE (7162) | per-message `MODSEQ`, mailbox `HIGHESTMODSEQ`, modseq-filtered STORE/FETCH |
| QRESYNC (7162) | durable record of *expunged* UIDs, to answer `VANISHED (EARLIER)` |
| OBJECTID (8474) | `MAILBOXID`, `EMAILID`, `THREADID` |
| ACL (4314) | a rights model, per identifier |
| QUOTA (9208) | quota roots, resource limits and usage |
| METADATA (5464) | annotations at server *and* mailbox scope |
| SORT/THREAD (5256) | server-side ordering and threading |
| SAVEDATE (8514) | per-message save time, distinct from INTERNALDATE |
| PREVIEW (8970) | generated and cached previews |

Nine method-group additions from extensions that already exist. A growable
mandatory interface breaks nine times before it meets an RFC nobody has written.

### Options evaluated

**(a) Struct of functions** — consistent with `UnilateralDataHandler` and rule 4's
stated preference. A new RFC adds a field, which is not a break, so mechanically
it works. But it is the wrong shape here: IMAP backends are stateful and
hierarchical (`Backend` → `Session` → selected view), so a flat struct forces
every backend to rebuild session plumbing by closure; there is no compile-time
completeness check, so a missing mandatory operation fails at runtime in
production; and with the nine extensions above it reaches ~60 nilable fields.
Right for a handful of optional callbacks, wrong for the primary abstraction.

**(b) Small mandatory interface set + optional capability interfaces discovered
by type assertion** — the shape the standard library uses for optional behaviour
(`http.Flusher`, `http.Hijacker`, `io.ReaderFrom`).

The property that makes rule 4's blanket suspicion inapplicable: **a new RFC
never adds a method to an existing interface — it adds a new one.** CONDSTORE
ships `CondStoreMailbox`; backends that do not implement it keep compiling
untouched.

**(c) Hybrid.**

### Recommendation

**Option (c), weighted to (b), with options structs as a third mechanism.**

1. **Three mandatory interfaces**: `Backend`, `Session`, `SelectedMailbox`.
2. **Every capability beyond the baseline is an optional interface**, one per
   capability, discovered by type assertion.
3. **Backend→client updates are a concrete framework type, not an interface** —
   that direction has the same growth pressure as `UnilateralDataHandler` and
   gets the same answer (§4).
4. **Options structs on every backend method**, per API-STABILITY rule 3. This is
   the third mechanism and it carries more weight than revision 1 gave it: many
   extensions are a *modifier on an existing command* rather than a new
   operation, and those become a **field**, not a new interface. `CHANGEDSINCE`
   is a field on `FetchOptions`; `HIGHESTMODSEQ` is a method on
   `CondStoreMailbox`. Same RFC, two mechanisms, chosen by shape.
5. **The rule, and a mechanical gate.** *A new extension may add a new optional
   interface, or a field to an existing options struct. It may never add a method
   to an interface that already exists.* Enforced by a golden test recording every
   exported interface's method set — the same mechanism rules 2, 3, 6 and 7 have.
   API-STABILITY §3 is the standing record of what happens to a rule with no gate.

### The mandatory set is not small, and pretending otherwise would be dishonest

Ten methods on `Session` — nine protocol operations plus `Close` — and seven on
`SelectedMailbox`. That is the rev1 baseline; an IMAP server without `RENAME` is
not an IMAP server. What matters is not the count but that **the set is closed**:
it is the fixed point where no future RFC adds anything, because every future RFC
is either a new optional interface or a new options field.

### MOVE is optional to the interface but required for rev2

`MoveMailbox` is an optional interface, and the framework **must not synthesise
MOVE from `Copy` + `Store` + `Expunge`**. RFC 6851 is explicit that the sequence
exposes intermediate states and that an interruption can leave the messages
duplicated or the source inconsistent; MOVE carries stronger per-message outcome
requirements than the parts it superficially decomposes into.

So the framework refuses rather than approximates, and two advertisements depend
on it:

- `MOVE` is advertised to a rev1 client only when the backend implements
  `MoveMailbox`.
- `IMAP4REV2` is advertised only when the backend implements `MoveMailbox`,
  because rev2 incorporates MOVE.

This keeps the mandatory interfaces at the rev1 baseline while keeping the rev2
promise truthful — the same pattern as every other incorporated behaviour in §1.

### Interface sketch — non-binding, but it compiles

Reviewed against the real vocabulary and verified to build and vet clean.
Full text in the T16 working notes; the load-bearing parts:

```go
// Backend is shared by every connection and must be safe for concurrent use.
type Backend interface {
	Authenticate(ctx context.Context, conn *ConnInfo, cred *Credentials,
		opts *AuthenticateOptions) (Session, error)
}

// Session is one authenticated connection. Its methods are NEVER called
// concurrently with each other, so a backend needs no locking for per-session
// state. Anything shared with other sessions must be concurrency-safe.
type Session interface {
	List(ctx context.Context, w *ListWriter, ref string, patterns []string, opts *ListOptions) error
	Status(ctx context.Context, mailbox string, opts *StatusOptions) (*imap.StatusData, error)
	Create(ctx context.Context, mailbox string, opts *CreateOptions) error
	Delete(ctx context.Context, mailbox string, opts *DeleteOptions) error
	Rename(ctx context.Context, from, to string, opts *RenameOptions) error
	Subscribe(ctx context.Context, mailbox string, opts *SubscribeOptions) error
	Unsubscribe(ctx context.Context, mailbox string, opts *UnsubscribeOptions) error
	Append(ctx context.Context, mailbox string, r io.Reader, opts *AppendOptions) (*imap.AppendData, error)

	// Select is ATOMIC: it captures the snapshot and attaches updater to that
	// same state before returning. See "the atomic select boundary" below.
	Select(ctx context.Context, mailbox string, updater *Updater, opts *SelectOptions) (*SelectResult, error)

	Close(ctx context.Context) error // NOT protocol LOGOUT, which is framework-owned
}

type SelectResult struct {
	Mailbox  SelectedMailbox
	Snapshot SelectSnapshot
	_        struct{}
}

// SelectSnapshot is one atomic mailbox state, and everything the framework
// needs to build its sequence-number view.
type SelectSnapshot struct {
	// UIDs of every visible message, ascending — which is sequence-number
	// order, since UIDs ascend strictly within a mailbox. This IS the map.
	UIDs []imap.UID

	Status         imap.MailboxStatus
	Flags          []imap.Flag
	PermanentFlags []imap.Flag
	ReadOnly       bool
	NumRecent      uint32 // rev1 \Recent

	// HighestModSeq zero with NoModSeq set is reported as NOMODSEQ, which is
	// not the same as a modseq that happens to be zero.
	HighestModSeq uint64
	NoModSeq      bool

	// Revision identifies the state this snapshot describes. Updates carry
	// the revision they follow, so a gap is detectable rather than silent.
	Revision MailboxRevision
	_        struct{}
}

// SelectedMailbox is the BACKEND'S PER-CONNECTION HANDLE for one selected
// mailbox — not the durable mailbox, and not the whole selected state. The
// framework separately owns the UID/sequence map, the update queue, the enabled
// revision and features, and saved search state.
//
// A backend returns a distinct value per selection and never shares one.
type SelectedMailbox interface {
	Status(ctx context.Context, opts *StatusOptions) (*imap.MailboxStatus, error)
	Fetch(ctx context.Context, w *FetchWriter, uids imap.UIDSet, opts *FetchOptions) error
	Search(ctx context.Context, query *SearchQuery, opts *SearchOptions) (*SearchResult, error)
	Store(ctx context.Context, w *FetchWriter, uids imap.UIDSet, store *StoreFlags, opts *StoreOptions) error
	Copy(ctx context.Context, uids imap.UIDSet, dest string, opts *CopyOptions) (*imap.CopyData, error)
	Expunge(ctx context.Context, w *ExpungeWriter, uids *imap.UIDSet, opts *ExpungeOptions) error
	Unselect(ctx context.Context) error
}
```

Four decisions in that sketch that matter more than the names:

**The rename to `SelectedMailbox` is deliberate — and it is the backend's handle,
not the whole selected state.** Revision 3's prose said it "carries" the
sequence-number view, pending updates and enabled extensions, which contradicts
§4, where those are framework-owned. The correct split:

```
framework selected state
├── SelectedMailbox              ← the backend's per-selection resource handle
├── UID ↔ sequence map           ← framework
├── enabled revision/extensions  ← framework
├── update queue                 ← framework
└── saved search result          ← framework
```

`SelectedMailbox` means *this connection's handle on the backend's selection*:
whatever per-selection resources the backend needs, plus read-only selection and
its QRESYNC parameters. It is still strictly per-connection and must never be
shared between connections, which is what the name is for — that bug is easy to
write and its symptoms appear in a different session from the one that caused it.

**Backends see UIDs, never sequence numbers.** The framework resolves seqnums to
UIDs before calling and maps back on the way out. The cost is a UID list per
selection — 4 bytes per message, so 4 MB for a million-message mailbox. The
purchase is removing IMAP's single most error-prone correctness problem from
every backend anyone ever writes. Worth it, and it is the same decision as §4's
update model, for the same reason.

**The atomic select boundary.** Revision 2 made the UID-only promise without
giving the framework any way to *build* the map, which made the promise
unimplementable from its own interface. `Select` returned a `SelectedMailbox`
and `Status` returned counts; neither yielded the ordered UID list.

Worse, the obvious three-step repair is racy:

```
1. read status          ← an APPEND here inflates the count
2. enumerate UIDs       ← an EXPUNGE here drops a message from the map
3. register the updater ← changes between 2 and 3 are lost entirely
```

Any interleaving produces a sequence map that disagrees with the `EXISTS` the
client was just told, and the client has no way to detect it.

So **snapshot capture and updater attachment are one atomic backend
operation**, which is why `Select` takes the `*Updater` and returns a
`SelectResult`. The contract the backend must honour:

> `Snapshot` describes one atomic mailbox state. `updater` is attached to that
> same state before `Select` returns. Every change committed after that state is
> delivered exactly once, or the connection is terminated. No change is lost in
> the gap, because there is no gap.

The `Updater` is a parameter rather than a `SelectOptions` field on purpose: it
is *required*, and options structs carry the contract that `nil` means defaults.
A required collaborator hidden in an optional struct is a nil-pointer bug
waiting for its first backend author.

`MailboxRevision` lets the framework detect a gap rather than trust the
guarantee — updates carry the revision they follow. A backend that violates
atomicity is then caught by a divergence check instead of corrupting a client
silently.

This one boundary also gives rev1 `\Recent`, QRESYNC initialisation, `NOMODSEQ`
reporting and read-only selection a single coherent place to be decided, rather
than four independent lookups that can each observe a different mailbox state.

**Streaming is writer-style.** `List`, `Fetch`, `Store` and `Expunge` write
through a framework-owned writer rather than returning a slice, so no backend
materialises an unbounded result set. Body sections carry an `io.Reader` — the
contract `imap.FetchDataBodySection.Literal` already has — so a 200 MiB `BODY[]`
never lands in memory. `Append` receives an `io.Reader` bounded to exactly the
announced literal size.

**`Session.Close` is not `Logout`.** Revision 1 said LOGOUT was framework-owned
and then put logout on the backend interface. LOGOUT is a protocol command the
framework answers; the backend gets `Close`, a resource-release call, which the
framework also invokes on disconnect, timeout and error paths where no LOGOUT
ever arrives.

**SEARCH criteria reach the backend UID-normalised.** `imap.SearchCriteria` is a
public bidirectional type and *can* contain `imap.SearchSeqNum`, so handing one
straight to a backend would contradict the UID-only promise in the very method
most likely to be implemented natively. The framework therefore rewrites
sequence-set keys to UID keys **recursively, including inside `SearchNot`,
`SearchOr` and `SearchAnd`**, and passes a `*SearchQuery` — a framework-only
wrapper that cannot be constructed by a backend and whose invariant is
established at construction. `SearchResult` is UID-keyed for the same reason;
conversion back to sequence numbers happens only in the framework.

A parallel criteria type hierarchy with no sequence-number variant would make the
invariant purely mechanical, and was considered and rejected: `imap.SearchCriteria`
is one of the three *open* sets that every RFC extends, so a parallel hierarchy
would have to grow in lockstep forever, doubling the work of every future
extension — the exact cost this project exists to avoid. The wrapper plus a
gate is the affordable version, and the gate is not optional: a test walks the
normalised tree and fails if any `SearchSeqNum` survives, run over the criteria
fuzz corpus rather than a handful of examples.

### The cost of type assertion, stated plainly

It moves a class of error from compile time to runtime: a backend author who gets
a method signature subtly wrong gets a silently-unadvertised capability, not a
compile error. The mitigations are `imapserver/backendtest` (§7) and the
reference backend (§6) — not cleverness in the type system.

---

## 3. Framework versus backend, and how capabilities are derived

| Framework-owned | Backend-delegated | Cooperative |
|---|---|---|
| CAPABILITY, ENABLE, NOOP, LOGOUT, ID, CHECK | LIST, LSUB, STATUS, SELECT/EXAMINE | IDLE |
| STARTTLS, TLS termination | FETCH, STORE, APPEND, COPY, MOVE, EXPUNGE | SEARCH (§5) |
| LITERAL+/-, SASL-IR, UTF8=ACCEPT framing | ACL, QUOTA, METADATA, OBJECTID | CONDSTORE, QRESYNC |
| COMPRESS=DEFLATE, UNSELECT | SORT, THREAD | AUTHENTICATE |
| command parsing, response encoding, state machine, tags, seqnum↔UID mapping | | NOTIFY, NAMESPACE |

Revision 1 said "three cooperative cases" and then listed four, giving two of them
no analysis. All are covered now.

**IDLE.** The framework owns `+ idling`, `DONE` and the timing. It cannot own
*knowing something changed* — that needs the backend to push. But the backend must
not write to the connection: RFC 3501 §7.4.1 and RFC 7162 forbid delivering
EXPUNGE while a FETCH, STORE or SEARCH response is in flight, because it renumbers
messages the client is still reading. So the backend *signals*, the framework
*decides when to deliver*. Not fastidiousness — the reason the split has this shape.

**CONDSTORE / QRESYNC.** Durable state no protocol layer can reconstruct:
per-message modseqs, and for QRESYNC a record of which UIDs were expunged and at
which modseq, retained after the messages are gone. An in-memory shim would lose
it on restart, which is the exact case QRESYNC exists to serve.

**AUTHENTICATE.** The framework owns every SASL mechanism state machine
(`internal/imapsasl`); the backend answers the credential question. That works for
PLAIN, LOGIN, XOAUTH2 and OAUTHBEARER, where the backend receives extracted
credentials. It does **not** work for SCRAM, which needs stored key material
(salt, iteration count, StoredKey, ServerKey) that only the backend has — so
SCRAM is an optional interface returning those, and `AUTH=SCRAM-*` is advertised
only when the backend implements it. Channel binding (`-PLUS`) stays framework-side
via `tls.ConnectionState.ExportKeyingMaterial`.

**NOTIFY (RFC 5465)** extends IDLE's push model to mailboxes that are not
selected, which means the backend must publish events outside any selection. That
is a different lifetime from `Updater`, which is scoped to a selection, and it is
why NOTIFY is not simply "IDLE with more events". Deferred to T23 with an explicit
note: the `Updater` design must not make a session-scoped update channel
impossible to add later.

**NAMESPACE** is usually a property of the authenticated user, not global
configuration — shared and other-user namespaces differ per account. So it is an
optional `NamespaceSession` interface, with a configured default when absent.

### Capability advertisement is derived, but not by a bare type assertion

Revision 1 said "backend implements interface ⇒ advertise capability". That is too
absolute. Capabilities are state-dependent (RFC 9051 §7.2.1 requires re-issuing
CAPABILITY after STARTTLS and after authentication), have dependencies, and can
vary per mailbox and per user.

So the framework owns a **capability descriptor table**. Each entry declares:

| Field | Purpose |
|---|---|
| `Name` | wire spelling |
| `RequiresBackend` | the optional interface(s) that must be present |
| `RequiresFramework` | framework support that must be compiled/enabled |
| `Depends` | other capabilities implied (QRESYNC ⇒ CONDSTORE) |
| `States` | connection states where it is advertised (not-authenticated / authenticated / selected) |
| `RequiresTLS` | advertise only after TLS, or only before |
| `Enable` | behaviour when named in `ENABLE` |
| `Available` | optional dynamic check, per session or per mailbox |

`CAPABILITY` output is then computed from the table against the current
connection state — never a hand-written list anywhere in the tree. A configured
list drifts from what the backend actually does, and each drifted entry is a
client interop bug that presents as a server bug.

The cases the table exists to get right:

- `STARTTLS`/`LOGINDISABLED` before TLS; `AUTH=` mechanisms differing before and
  after TLS.
- `IDLE` advertised only when the selected path can actually produce
  notifications, not merely because the framework can parse `IDLE`.
- QRESYNC implying CONDSTORE.
- A server advertising CONDSTORE while a *particular* mailbox returns
  `NOMODSEQ` — capability is global, availability is per-mailbox, and they are
  not the same question.
- ACL/QUOTA/NAMESPACE varying per authenticated user.

**The pairing with options structs, and the gates that enforce it.** The
framework sets an options field only when the corresponding capability is
advertised for that session. Without this, a field added to a backend options
struct is silently ignored by an older backend that does not understand it, and
the server claims a capability it does not honour.

"Ships together" was a written promise in revision 2, which is the mechanism this
project distrusts. Two gates make it real:

1. **Every extension-owned options field declares the *feature* that activates
   it.** A struct tag or registry entry binds the field, and a test fails on any
   extension field with no binding. The framework consults that binding when
   populating options, so the pairing is executed rather than remembered.
2. **Every growable exported options struct blocks external unkeyed literals**
   via an unexported sentinel field — API-STABILITY rule 7, applied to the server
   surface from the first commit. Without it, adding a field is a breaking change
   for any caller who wrote a positional literal, which would defeat the entire
   mechanism.

### Features, not capabilities, are what options bind to

Revision 3 said fields bind to a *capability*. The `BINARY` correction in §1
disproves that: the same backend behaviour can be reachable through more than one
route, and some rev2 behaviour has no capability token at all.

- BINARY **FETCH** is available when IMAP4rev2 is enabled, *or* to a rev1 client
  when `BINARY` is advertised.
- Binary **APPEND** requires the `BINARY` capability specifically.
- Multiple LIST patterns still require `LIST-EXTENDED` even though other LIST
  behaviour is incorporated into rev2.

Binding an options field to one capability token would force us to invent
pseudo-capabilities for incorporated rev2 behaviour, which is the same
token-versus-behaviour confusion §1 exists to prevent.

So there are two descriptor layers:

| Layer | Keyed by | Produces |
|---|---|---|
| Capability descriptors | wire token | the `CAPABILITY` response (§ above) |
| Feature descriptors | internal `FeatureID` | whether a behaviour is active for this session |

A feature descriptor carries an activation expression over the enabled revision
and the advertised capabilities:

```
binary-fetch   = IMAP4rev2 enabled OR BINARY advertised
binary-append  = BINARY advertised
list-multi-pat = IMAP4rev2 enabled OR LIST-EXTENDED advertised
```

Wire capabilities stay outputs of the capability layer; backend options fields
bind to `FeatureID`s. The two layers share the backend-interface requirements, so
a feature whose backend interface is absent is inactive and its capability is
unadvertised, from one source of truth.

---

## 4. Concurrency, updates and backpressure

Revision 1's model was internally inconsistent. It claimed a single per-connection
goroutine that reads, executes and writes; *and* that reading is pipelined so the
client never blocks; *and* that a backend context is cancelled promptly on client
disconnect. Those cannot all hold: a goroutine blocked inside `Fetch` is not
reading, cannot observe EOF, and therefore cannot cancel on disconnect. Kernel
receive buffering hides this until it does not.

### The structure

- **A reader goroutine** owns the decoder. It parses command lines into typed
  commands and pushes them onto a **bounded command queue**. On EOF or read error
  it cancels the connection context immediately — that is the whole reason it is a
  separate goroutine.
- **An event loop goroutine** owns all session state: the state machine, the
  selected view, the sequence-number map, and the enabled-extension set. It takes
  commands off the queue and executes them **strictly sequentially**, calling the
  backend. It also drains the update queue at protocol-legal points.
- **The event loop writes synchronously.** Revision 2 left this open ("the event
  loop, or one writer goroutine fed from it"); it is decided now, and the choice
  is the event loop itself. With writer-style backend APIs and `io.Reader` body
  sections, synchronous writing gives clean ownership: a writer method consumes
  its body reader before returning, so backend stack frames stay alive for the
  duration; write errors propagate directly to the command that caused them;
  there is no second response queue needing its own byte bound; and the legal
  points for update delivery stay obvious. Never the reader, never the backend.

  A dedicated writer goroutine is only safe with a byte-bounded queue *and* a
  defined rule on whether a writer call blocks until its reader is drained. That
  complexity buys little while command execution is deliberately sequential, so
  it stays a later option rather than an initial design.
- **Continuations are coordinated.** A synchronising literal requires a `+` before
  the client sends the payload, and RFC 9051 requires a command continuation to be
  fully negotiated before another command begins. So the reader stalls at a
  synchronising literal until the event loop authorises the continuation, and the
  bounded queue does not admit a new command mid-literal. Non-synchronising
  literals (LITERAL+/-) arrive without permission and can only be drained or
  refused after the fact.

Sequential execution is deliberate and is not a simplification to be lifted later.
RFC 3501 §5.5 permits concurrent execution of non-interfering commands, but
"non-interfering" is ambiguous in the specification and is exactly where servers
get sequence-number renumbering wrong. Tightening to the safe end stays additive:
a future release can add parallelism behind an option, because loosening a
guarantee nobody was allowed to rely on breaks nobody.

### The re-entrancy contract — what backend authors get in writing

- `Session` and `SelectedMailbox` methods are **never called concurrently for the
  same session**. No locking needed for per-session state.
- The `Backend`, and anything shared between sessions, **must be safe for
  concurrent use**.
- The framework **never calls into the backend from the update-delivery path**.
  Updates are values pushed earlier; delivery touches only framework state. A
  backend therefore cannot deadlock by pushing an update while holding a lock a
  framework callback would need.

### The update contract

Revision 1 gave four method names and called it a design. The parts that actually
need freezing:

**Identity.** Updates are **UID-keyed**, never sequence-number-keyed. The
framework maintains each connection's sequence-number view and translates on
delivery. Making every backend compute per-connection sequence numbers would
spread IMAP's hardest correctness problem into every backend ever written;
centralising it means it is wrong in at most one place.

**Revisions are carried by batches, not by individual events.** Revision 3 said
"updates carry the revision they follow", which is not implementable: one backend
commit produces several observable events — an APPEND changes EXISTS, UIDNEXT,
HIGHESTMODSEQ and possibly flags; a MOVE adds at the destination and removes at
the source — and per-event revisions leave every interesting question open. Does
the token name the state before the event or after it? May two events share one?
How does coalescing preserve the chain?

So the unit of publication is a batch, and it is the backend's atomic commit:

```go
type UpdateBatch struct {
	Before  MailboxRevision // state this batch applies to
	After   MailboxRevision // state after applying it
	Origin  ChangeToken     // the command that caused it, or zero
	Changes []Update
	_       struct{}
}

func (u *Updater) Push(batch *UpdateBatch) error
```

The contract is then mechanical rather than interpretive:

1. `SelectSnapshot.Revision` identifies the snapshot state.
2. The first batch must have `Before == snapshot.Revision`.
3. Every later batch must have `Before == previous.After`.
4. All changes from one atomic backend commit go in **one** batch.
5. `Changes` are applied in slice order. A batch containing interacting changes
   therefore has deterministic semantics rather than depending on the
   framework's iteration strategy.
6. **Batches are never merged.** See below.
7. A revision mismatch **terminates the connection**. A gap in the chain means
   the framework's sequence map may already disagree with the mailbox, and there
   is no safe way to continue from that.

**Coalescing happens after accounting, never before it.** An earlier revision
said adjacent batches may be merged while preserving the revision chain. That is
wrong: a batch carries exactly one `Origin`, so merging two batches with
different origins produces a batch whose origin is a lie —

```
Batch A: Before=10 After=11 Origin=commandA
Batch B: Before=11 After=12 Origin=commandB
          → merged batch can truthfully claim neither
```

— and the §"origin accounting" rules below would then suppress the wrong
changes. The chain is preserved and the accounting is still destroyed.

So the two concerns are separated by order of operations:

1. Each batch is validated against the revision chain, **in order**.
2. Origin accounting runs per batch, per operation.
3. Only then may the surviving *wire-level changes* be coalesced, where the
   protocol permits (§"Coalescing" above).

Transaction accounting is a correctness question; wire coalescing is a bandwidth
optimisation. Keeping them in that order means the optimisation cannot corrupt
the accounting, which is the only ordering that is safe by construction.

`MailboxRevision` need only be comparable and opaque. Consecutive integers are
not required, and a backend with a natural transaction id or modseq should use
it rather than inventing a counter.

**Attachment failure.** If `Select` attaches the updater internally and then
fails, the backend must detach it before returning. The framework invalidates the
updater on **every failed `Select` path, including error and panic**, so a
backend that publishes after a failed `Select` gets an error rather than writing
into a half-built selection.

After a *successful* `Select` the updater remains valid until `Unselect`, a
replacement selection, session close, or connection teardown — whichever comes
first. (An earlier revision said "every path — success, error and panic", which
contradicted that lifetime.)

**Ordering.** Per selection, updates are delivered in the order pushed. Ordering
across selections is not defined and must not be relied on.

**Blocking.** Pushes are non-blocking while the bounded queue has room. The queue
is bounded by **payload bytes as well as item count** — a flag update carrying a
large keyword set is not the same cost as an expunge.

**Lifetime.** An `Updater` is valid from `Select` until `Unselect` returns. Pushes
after that return an error rather than panicking; a backend racing its own
teardown is a bug but must not take the process down.

**Coalescing.** No removal event may ever be *dropped*. Beyond that the rule
splits by wire form, and revision 2 got this wrong by forbidding coalescing
outright:

- `EXISTS` and flag updates may be coalesced.
- `EXPUNGE` stays individually ordered — each response renumbers the messages
  after it, so merging two is not expressible.
- `VANISHED` **may** be coalesced into a UID set. Carrying multiple UIDs is what
  the response is *for*; RFC 7162 names the bandwidth saving as a benefit. The
  one prohibition is that `VANISHED (EARLIER)` and plain `VANISHED` are never
  combined with each other — they answer different questions.

**Origin accounting, not blanket echo suppression.** Revision 2 said a change
made by this session is reported by its own command response and never through
the queue. That is right for STORE and wrong in general: an APPEND or COPY *into
the currently selected mailbox* still has to extend the sequence map, and
normally still owes the client an `EXISTS`.

So suppression is decided per operation, by whether the command's own response
fully describes the change:

| Operation | Handling |
|---|---|
| STORE | command response carries the FETCH data; suppress the queue event |
| EXPUNGE, MOVE out of the selected mailbox | command response carries the removals; suppress |
| APPEND / COPY / MOVE **into** the selected mailbox | the command result does not describe the new message's position — either synthesise the addition from APPENDUID/COPYUID, or let the queue event through. Never simply suppress |
| anything not fully described by the command result | **never suppressed** |

The mechanism is a change token carried on the update batch and matched against
the command that produced it, not "drop everything originating in this session".
Session-origin suppression is the shape that loses an `EXISTS` the client needed.

**The token has to reach the backend before the transaction starts**, or the
backend has no way to stamp asynchronously published batches with it. So it is an
input to every mutating call, carried by a shared embedded struct rather than
repeated field-by-field:

```go
type MutationOptions struct {
	Origin ChangeToken
	_      struct{}
}
```

`AppendOptions`, `StoreOptions`, `CopyOptions`, `MoveOptions` and
`ExpungeOptions` embed it, and `UpdateBatch.Origin` echoes it back.

Matching is exact, not merely non-zero:

- A **zero** token means no framework command supplied an origin. Never
  suppressed.
- A **non-zero** token is suppressed only when it matches *the command currently
  being accounted for*, and only per the per-operation table above.
- An **unrelated non-zero** token — a different command on this connection,
  including an earlier one — is treated like any external change. It is not
  "ours" merely because it originated on this connection, which is the
  distinction that keeps a pipelined second command from swallowing the first
  command's updates.

**Races.** A push arriving during `SELECT`, `CLOSE`, `UNSELECT` or logout is
dropped for the closing selection, not delivered to the next one — a new selection
starts from a fresh `Status`.

### Overflow: never drop, and BYE is best-effort

If the client stops reading, the queue fills. Dropping an EXPUNGE is not
available: it desynchronises the client's sequence numbers permanently and
silently, and the client cannot detect it. A dropped *connection* is detectable.

So on overflow the framework attempts `* BYE` **under a short write deadline** and
then force-closes regardless of whether the BYE was written. Revision 1 said "send
BYE and close", which assumes a write can complete — but the client that caused
the overflow by not reading is often exactly the client that cannot receive the
BYE. RFC 9051 §5.4 calls out the flow-control obligation directly.

**Overflow cannot be handled by the event loop alone, given synchronous
writing.** The two decisions interact: the event loop may be blocked mid-write on
a large FETCH to a client that has stopped reading, which is precisely when the
update queue overflows — so "the event loop notices and sends BYE" deadlocks
against the condition it is meant to resolve.

The push path therefore does not wait for the event loop. On overflow the
`Updater`:

1. marks the connection fatal,
2. cancels the connection context,
3. signals the event loop,
4. starts a short forced-close deadline.

The event loop sends `BYE` if it regains control before that deadline. Otherwise
the forced close tears down the connection and unblocks the stuck write. Step 2
is what makes step 4 terminate rather than merely expire.

---

## 5. Message analysis, and who owns SEARCH

Revision 1 contradicted itself: §2 delegated SEARCH entirely to the backend, while
§4 had the framework evaluating every criterion against stored messages. Both
cannot be the default.

The contradiction matters because forcing framework-side evaluation would make the
framework enumerate and parse every message in a mailbox, which is both the
performance bottleneck and an active obstacle to the backends most likely to be
written — anything with a database or a search index already answers these queries
natively and far better.

### The split

| Layer | Owns |
|---|---|
| Framework | parsing SEARCH syntax into `imap.SearchCriteria`, charset handling, returning ESEARCH/result options |
| Backend | **selecting and enumerating matches** — free to translate criteria into a native query |
| `internal/imapmessage` (reusable helper) | evaluating one criterion tree against one message plus its metadata |
| `imapserver/memory` and simple backends | use the helper |
| Indexed backends | ignore the helper, translate to SQL/index queries |

The helper is a package, not a policy. That is what makes both backend kinds
first-class.

### Two distinct subsystems, not one

Revision 1 bundled these; they need separating because their inputs differ.

**Message analysis** — pure, and a function of RFC 5322 bytes alone:

- **Generating BODYSTRUCTURE**, which needs exact octet counts and text line
  counts per part, agreeing with the raw bytes byte-for-byte including the stored
  CRLF convention.
- **Generating ENVELOPE**, which needs RFC 5322 address-list parsing including
  group syntax, and must *reproduce* malformed real-world headers rather than
  reject them — a server that refuses to describe a message a client already
  accepted is worse than one that describes it approximately.
- **Section extraction** for `BODY[HEADER.FIELDS (...)]<partial>`, byte-exact with
  correct offsets into the original.

**Search evaluation** — needs message bytes *and* mailbox metadata the bytes do not
contain: flags, UID, sequence position, INTERNALDATE, RFC822.SIZE, and MODSEQ under
CONDSTORE. So its input is a message *plus* a metadata struct, and it cannot be a
function of the raw bytes.

`net/mail`, `mime` and `mime/multipart` cover part of the first and none of the
byte-exactness. Zero dependencies still holds. Both get fuzz targets, and both are
independent of the backend abstraction, so T21 can start alongside T18.

---

## 6. The reference backend

**Ships as a supported package: `imapserver/memory`.** Not test-internal.

1. It is the only thing putting real pressure on the backend abstraction before it
   freezes. An abstraction validated only by its designer's mock is validated by
   nothing.
2. It gives the conformance suite a target drivable to any state on demand,
   including states real servers make hard to reach.
3. A framework nobody can run without first writing a storage layer gets no users,
   and therefore no bug reports, before its API freezes.

Constraints:

- Documented **not durable, not for production**, in the package doc.
- Minimal exported surface: a constructor, an options struct, and the methods the
  interfaces require.
- **It implements every optional interface the release itself claims to support** —
  not, as revision 1 promised, every optional interface forever. That open-ended
  promise would make each future extension substantially more expensive and turn
  the example into a second server product. The scoped version keeps it useful as
  the worked example of each capability we actually ship.
- **Test-state manipulation lives in `backendtest`, not in the production surface.**
  Revision 1 wanted both a minimal API and the ability to force a UIDVALIDITY
  change; those conflict. Pathological-state control belongs in an explicitly
  unstable testing controller, so the supported surface stays small.

Out of scope, confirmed: maildir or SQL storage, LMTP/delivery, a user database.

---

## 7. Testing strategy

**Loopback (`net.Pipe`, our client against our server) is the inner loop, not
validation.** Fast, hermetic, catches regressions — but a shared misreading of an
RFC passes both sides, which is the exact failure the client's interop matrix
exists to catch.

Validation is external, in descending order of value:

1. **Dovecot's `imaptest`** against our server in a container. The de-facto IMAP
   conformance and stress tool; the highest-value single external check available.
2. **Point the existing interop matrix at our own server.** T12 already starts
   servers, seeds fixtures over APPEND and reports a per-capability table. Adding
   `imapserver/memory` as an `interop/servers/` entry reuses all of it and makes
   our coverage directly comparable to Dovecot's and Stalwart's. It is the one
   entry where the profile assertion catches *our* bug rather than a broken
   container, and it needs no image — the harness must stop assuming every profile
   has a container.
3. **Real client software** — `mbsync`/`isync`, `offlineimap`. They exercise
   long-tail sequencing (UIDVALIDITY change mid-sync, resumed partial fetches,
   CONDSTORE replay) that no suite written alongside the server thinks to test.
4. **Server-side fuzzing** — the mirror of T13, non-optional. The command parser
   faces hostile input from *unauthenticated remote clients*: larger and more
   exposed than the client's hostile-server case. Bar unchanged: no panic, no
   hang, no unbounded allocation. Corpus from real client traffic and `imaptest`,
   not invention.
5. **`imapserver/backendtest`** — a conformance suite a third-party backend runs
   against itself. Also the mitigation for §2's runtime-discovery cost.

   It must check **snapshot self-consistency**, because a backend that gets this
   wrong corrupts the framework's sequence map rather than failing visibly:
   `len(Snapshot.UIDs) == Status.NumMessages`; UIDs strictly ascending and
   non-zero; `UIDNext` greater than every UID present; `NumRecent` no greater
   than the message count; and the batch chain starting at `Snapshot.Revision`.
   These are exactly the invariants §2 and §4 rely on and never re-derive.

### Stateful security tests

Parser fuzzing does not reach any of these, and each is a known historical
vulnerability class:

- **STARTTLS plaintext command injection** — bytes buffered after the STARTTLS
  command must be discarded, not executed. RFC 9051 §5.1 requires this explicitly
  because implementations got it wrong.
- Incomplete literal, then disconnect. Disconnect *during* APPEND.
- Slow reader during a large FETCH; slow writer during a large APPEND.
- Update-queue overflow under a non-reading client.
- Repeated failed authentication.
- Cancellation while the backend holds locks.
- SELECT/CLOSE/update races.
- Goroutine and temporary-file leak checks across all of the above.

---

## 8. Resource limits

The threat model inverts. T13 asked whether a hostile *server* could make the
client panic, hang or allocate without bound. The server faces the same parser
from **unauthenticated remote clients**, at connection rates a client never sees.

**Pre-authentication limits are separate and much tighter.** RFC 9051 §5.4 sets a
30-minute *minimum* on the post-authentication inactivity autologout, and
explicitly permits far shorter pre-authentication timeouts.

- Command line length; literal size — a pre-auth `APPEND {2147483647}` is refused
  *before* allocation. RFC 7888 permits refusing a non-synchronising literal that
  is too large, and APPENDLIMIT lets cooperative clients avoid the upload entirely.
- Commands per connection; short authentication deadline.
- Connection caps, total and per source address.

**Beyond parser inputs** — revision 1 stopped at the parser, which leaves the
expensive half unbounded:

| Bound | Why |
|---|---|
| queued pipelined commands | the command queue is an attacker-controlled buffer |
| total command execution time | a `SEARCH` over everything is cheap to ask for |
| LIST result count, SEARCH result cardinality | cheap request, unbounded response |
| FETCH response bytes; temporary spool bytes | `FETCH 1:* BODY[]` |
| decompressed COMPRESS=DEFLATE input | decompression bomb |
| authentication attempts per connection, account and source | credential stuffing |
| concurrent commands and connections per authenticated account | one account starving the server |
| backend work / messages scanned | the backend's own budget |
| total queued update payload bytes | §4's overflow bound |
| **selected-message count / sequence-map bytes** | §2's UID snapshot is a per-connection allocation sized by *mailbox*, not by request — a large mailbox selected on many connections is an amplification the client gets for free |

That last one is new in revision 4 and is the cost side of the UID-only decision:
buying correctness with a per-selection UID list means the list itself needs a
bound and a defined behaviour when a mailbox exceeds it (refuse the SELECT with a
`NO`, rather than allocate and hope).

Defaults are **safe**, not permissive-with-a-note. `internal/imapwire`'s limits and
deadline mechanism is reusable; the values are not.

---

## 9. What this costs v1.0, and how `imapserver` is versioned

### The exit criterion

Adding types to `package imap` after v1.0 is additive and always allowed.
**Reshaping an existing type is not.** A vocabulary exercised in only one
direction can hold a type a server can consume but not naturally produce, and no
client-side review surfaces it, because the client is the direction that works.

§0 shows this is not hypothetical: ~35 response data types are in the wrong
package, two cannot be constructed from outside `imapclient` at all, and at least
one field is meaningless server-side.

So **a bidirectional review of `package imap` is a v1.0 exit criterion** (T17,
milestone M4). Revision 1 guessed "no changes needed" was the likely outcome. That
guess is now known to be wrong, and T17 has concrete work.

### `imapserver` versioning — recommendation REVERSED from revision 1

Revision 1 recommended keeping `imapserver` in the same module with a carve-out
enforced by scoping the `apidiff` gate. **That was wrong, and the review's
argument against it is correct.**

The flaw: an `apidiff` scope is a gate on *us*. It changes nothing a user sees. A
user importing a package from a module tagged v1 reasonably expects it not to
break, and Go's compatibility guidance is built on exactly that expectation. No CI
configuration can reset it, and a doc comment saying "this package is exempt" is a
promise, which is the mechanism this project distrusts everywhere else.

Meanwhile revision 1 overstated the nested module's cost on two counts:

- **Development ergonomics.** Go workspaces (`go.work`) exist precisely to develop
  interdependent modules in one repository without committing `replace`
  directives. The objection is largely obsolete.
- **The `go.sum` entry.** The zero-dependency rule exists because "a `go.sum`
  entry is a stability liability we do not control". A self-referential entry on
  our own module is one we entirely control, so the exception is narrow and easy
  to justify rather than a hole in the policy.

**Recommendation: `imapserver` is a nested module with its own `go.mod`,
versioned v0.x independently while the root module is v1.x.**

Real remaining costs, accepted: two tags per release, explicit version
coordination between the modules, and a root-module dependency that must be bumped
deliberately. Those are operational. The same-module carve-out's cost is a
user-facing expectation problem no script can fix.

Fallback if the nested module proves unworkable in practice: same-module with a
documented stability exception — but only as a fallback, and it needs its own
written approval.

Either way this is an exception to API-STABILITY's versioning policy and **needs
explicit human approval**. It is recorded there as proposed and unapproved.

---

## 10. Task breakdown

Full specs exist for the tasks that do **not** depend on §2, because those can be
written honestly today. The rest get specs when §2 is approved.

| ID | Task | Milestone | Depends on | Spec |
|---|---|---|---|---|
| T16 | This document, and its approval | M5 | — | written |
| T17 | Bidirectional vocabulary audit of `package imap` | **M4 — blocks v1.0** | T16 | written |
| T18 | Server-direction codec: command parsing, response encoding, `internal/imapcodec` | M6 | T16 | written |
| T19 | Server core: reader/event-loop, state machine, dispatch, capability descriptors | M6 | T18, §2 approved | after approval |
| T20 | Backend contract + `imapserver/memory` + `backendtest` | M6 | T19 | after approval |
| T21 | Message analysis: bodystructure/envelope generation, section extraction, search evaluation helper | M6 | T18 | **written early — abstraction-independent** |
| T22 | Base command set, server side | M6 | T20, T21 | after approval |
| T23 | Server extensions, groups A–E (incl. NOTIFY's out-of-selection lifetime) | M6 | T22 | after approval |
| T24 | Conformance, interop, server-side fuzzing, stateful security tests | M6 | T22 | after approval |
| T25 | API review, docs, examples, `imapserver` release | M6 | T23, T24 | after approval |

T18 and T21 are the parallel pair, together the bulk of the work, and neither waits
on §2. Revision 1 called T21 abstraction-independent and then scheduled its spec
after approval; that was inconsistent and is fixed.

### A note on file ownership

T18 migrates `imapclient/{fetch,search,structure}.go` onto `internal/imapcodec`.
Those files belong to T06 under `BOARD.md`'s ownership rule.

That rule exists to make *concurrent* work safe — it is a lock, and T06 finished
long ago. A completed task's lock passes to the task that supersedes it. The
migration is also internal-only by construction: if it changes an exported
signature it has done something wrong, and `api_surface_test.go` plus the `apidiff`
gate both say so. Recorded here so the exception is deliberate rather than
discovered in review.
