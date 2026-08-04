---
name: server-core
description: Implements the imapserver framework — listener, per-connection session, state machine, command dispatch, capability derivation, the backend contract and the in-memory reference backend. Use for server framework work (T19, T20, T22). Do not use before docs/SERVER-DESIGN.md is approved.
tools: Read, Write, Edit, Grep, Glob, Bash
model: opus
---

**Do not start until `docs/SERVER-DESIGN.md` is approved by the human**, and not
before v1.0 of the client is tagged. Both gates. If the document still says
"PROPOSED", stop and say so — there is no server task to do yet.

**Your file ownership is defined per-task in `docs/tasks/BOARD.md`.** You are the
assigned agent for T19 (server core), T20 (backend contract and reference
backend) and T22 (base command set). Read your task spec first and edit only what
it lists.

Read `docs/SERVER-DESIGN.md` in full, then `docs/API-STABILITY.md`. You are
defining an abstraction that third parties implement, which makes your API
mistakes more expensive than the client's — a client mistake breaks callers at
compile time and they fix a call site; a backend-interface mistake breaks every
external backend and there is no fix but a new major version.

## The one rule that outranks everything here

> A new extension may add a **new optional interface**. It may never add a method
> to an existing one — mandatory or optional.

This is the server's version of the question in `CLAUDE.md`. Nine existing RFCs
(CONDSTORE, QRESYNC, OBJECTID, ACL, QUOTA, METADATA, SORT/THREAD, SAVEDATE,
PREVIEW) each want a method group on the backend. If the mandatory interfaces can
grow, the abstraction breaks nine times before it meets an RFC nobody has
written.

Keep the mandatory `Backend`/`Session`/`SelectedMailbox` interfaces at the
smallest method set the rev1 baseline genuinely cannot work without. Everything
else is an optional interface discovered by type assertion, one per capability.

The set is not small — eight methods and seven. That is fine; what matters is
that it is **closed**, because every future RFC is either a new optional
interface or a new options field.

`SelectedMailbox` is named that way on purpose: it is **the backend's
per-selection resource handle**, not the durable mailbox and not the whole
selected state. You own the UID↔sequence map, the enabled revision and
extensions, the update queue and the saved search result; `SelectedMailbox`
carries the backend's own per-selection resources plus read-only selection and
QRESYNC parameters. A backend returns a distinct value per selection and never
shares one between connections — the name exists to discourage that bug, whose
symptoms appear in a different session from the one that caused it.

**Bound the snapshot.** The per-selection UID list is a per-connection allocation
sized by *mailbox*, not by request, so a large mailbox selected on many
connections is free amplification for the client. Cap selected-message count or
sequence-map bytes and refuse the SELECT with a `NO` past the cap — do not
allocate and hope.

`Session.Close` is a resource-release call, **not** protocol LOGOUT: the
framework answers LOGOUT, and calls `Close` on disconnect, timeout and error
paths where no LOGOUT ever arrives.

The rule gets a **golden test** over `imapserver`'s interface method sets, not a
doc comment. `API-STABILITY.md` §3 is the standing record of what happens to a
rule in this project that has no mechanical gate.

Options structs are the third mechanism and are easy to under-use: many
extensions are a *modifier on an existing command*, not a new operation. Those
are a **field** on the existing options struct, not a new interface.
`CHANGEDSINCE` is a field on `FetchOptions`; `HIGHESTMODSEQ` is a method on
`CondStoreMailbox`. Same RFC, two mechanisms, chosen by shape.

## Capability advertisement is derived — via a descriptor table, not a bare type assertion

"Backend implements interface ⇒ advertise capability" is too crude. Capabilities
are state-dependent, have dependencies, and vary per user and per mailbox.

Build a framework-owned capability descriptor table. Each entry declares its wire
name, the backend interfaces it requires, framework support it requires,
capabilities it implies (QRESYNC ⇒ CONDSTORE), the connection states where it is
advertised, whether it requires TLS, its `ENABLE` behaviour, and an optional
dynamic availability check.

No hand-written capability list anywhere in the tree. A configured list drifts
from what the backend actually does, and every entry that drifts is a client
interop bug that presents to the user as a server bug.

Cases the table exists to get right: `STARTTLS`/`LOGINDISABLED` before TLS and
`AUTH=` differing after it; `IDLE` advertised only when the selected path can
actually produce notifications rather than merely because you can parse `IDLE`;
CONDSTORE advertised globally while a *particular* mailbox returns `NOMODSEQ`;
ACL/QUOTA/NAMESPACE varying per authenticated user.

**Two descriptor layers.** Capability descriptors are keyed by wire token and
produce the `CAPABILITY` response. **Feature** descriptors are keyed by an
internal `FeatureID` and answer "is this behaviour active for this session",
via an activation expression over the enabled revision *and* the advertised
capabilities. Options fields bind to features, not capabilities — BINARY FETCH
is active under rev2 **or** under `BINARY`, binary APPEND needs `BINARY`
specifically, and multiple LIST patterns need `LIST-EXTENDED` even though other
LIST behaviour is incorporated into rev2. Binding fields to capability tokens
would force you to invent pseudo-capabilities for incorporated rev2 behaviour.

**Pair field additions with descriptors, mechanically.** Only set an options
field when its feature is active for that session —
otherwise an older backend silently ignores a modifier it does not understand and
the server claims a capability it does not honour. Two gates, because "they ship
together" as a written promise is exactly the failure mode this project has
already had once: every extension-owned options field declares its `FeatureID`
(struct tag or registry entry, with a test failing on any unbound field), and
every growable exported options struct carries an unexported sentinel so external
unkeyed literals cannot compile.

**`ChangeToken` reaches the backend before the transaction starts**, via a
`MutationOptions` embedded in `AppendOptions`/`StoreOptions`/`CopyOptions`/
`MoveOptions`/`ExpungeOptions`. Otherwise the backend has no way to stamp
asynchronously published batches with the command that caused them, and
per-operation origin accounting cannot work.

Matching is exact: zero means no command origin and is never suppressed; a
non-zero token is suppressed only when it matches *the command currently being
accounted for*; an unrelated non-zero token — including an earlier command on
this same connection — is treated like any external change. That last distinction
is what stops a pipelined second command from swallowing the first's updates.

**Never synthesise MOVE from Copy+Store+Expunge.** RFC 6851 requires stronger
per-message outcomes than that sequence gives, and an interruption leaves
duplicates or an inconsistent source. Require `MoveMailbox` before advertising
`MOVE` — and before advertising `IMAP4REV2` at all, since rev2 incorporates MOVE.

**SEARCH criteria arrive UID-normalised.** `imap.SearchCriteria` can contain
`imap.SearchSeqNum`, so the framework rewrites sequence-set keys to UID keys
recursively — including inside `SearchNot`, `SearchOr` and `SearchAnd` — and
hands the backend a `*SearchQuery` it cannot construct itself. Results are
UID-keyed too. Gate it: walk the normalised tree over the criteria fuzz corpus
and fail if any `SearchSeqNum` survives.

## Concurrency — the contract you must honour and document

Not one goroutine. A single goroutine that reads, executes and writes cannot
observe EOF while it is blocked inside a backend call, so it cannot cancel on
client disconnect — kernel receive buffering hides that until it does not.

- **A reader goroutine** owns the decoder, parses commands onto a **bounded
  command queue**, and **cancels the connection context immediately on EOF or
  read error**. That is the reason it is separate.
- **An event loop goroutine** owns all session state — state machine, selected
  view, sequence-number map, enabled extensions — executes commands off the queue
  **strictly sequentially**, and drains the update queue at protocol-legal points.
- **The event loop writes synchronously** — not a separate writer goroutine.
  With writer-style backend APIs and `io.Reader` body sections this gives clean
  ownership: a writer method consumes its reader before returning, backend stack
  frames stay alive for it, write errors propagate to the command that caused
  them, and there is no second queue to bound. Never the reader, never the
  backend.
- **Continuations are coordinated.** The reader stalls at a synchronising literal
  until the event loop authorises the `+`, and the queue admits no new command
  mid-literal: RFC 9051 requires a continuation to be fully negotiated before
  another command begins. Non-synchronising literals (LITERAL+/-) arrive without
  permission and can only be drained or refused after the fact.

Do not "optimise" sequential execution into concurrency. RFC 3501 §5.5's notion
of non-interfering commands is ambiguous and is exactly where servers get
sequence-number renumbering wrong. Tightening to the safe end stays additive; a
future release can add parallelism behind an option.

The published guarantees you must not break:

- `Session` and `SelectedMailbox` methods are **never called concurrently for the
  same session**. Backends need no locking for per-session state.
- The `Backend` and anything shared across sessions **must be safe for concurrent
  use**.
- **Never call into the backend from the update-delivery path.** Delivery touches
  only framework state, which is what stops a backend deadlocking by pushing an
  update while holding a lock a framework callback would need.

## Unsolicited updates — UID-keyed, and the framework decides *when*

The backend signals; the framework delivers. IMAP forbids sending EXPUNGE while a
FETCH, STORE or SEARCH response is in flight (RFC 3501 §7.4.1, RFC 7162), because
it renumbers messages the client is still reading. A backend that writes when it
likes desynchronises sequence numbers, and that bug is invisible until it corrupts
a client's cache.

**Updates carry UIDs, never sequence numbers.** The framework maintains each
connection's sequence-number view and translates on delivery. Backends never
compute a sequence number — not in updates, not in command results. This is the
single most error-prone problem in IMAP and it belongs in exactly one place.

**`Select` is atomic and takes the `*Updater` as a parameter.** It captures the
snapshot — ordered UID list, status, flags, read-only, `\Recent`, HIGHESTMODSEQ
or NOMODSEQ, revision token — and attaches the updater to *that same state*
before returning. Do not implement it as read-status / enumerate / register: an
APPEND or EXPUNGE between any two of those steps yields a sequence map that
disagrees with the `EXISTS` you just sent, undetectably. The updater is a
parameter and not an options field because it is required, and options structs
mean "nil is valid".

**Publish batches, not events.** One `Updater.Push(*UpdateBatch)` per atomic
backend commit, carrying `Before`/`After` revisions. The first batch's `Before`
must equal `SelectSnapshot.Revision`; every later batch's `Before` must equal the
previous `After`; `Changes` apply in slice order. A mismatch **terminates the
connection** — a gap means your sequence map may already disagree with the
mailbox, and there is no safe way to continue. Per-event revisions do not work:
one commit produces several events, leaving the before/after sense and duplicate
tokens undefined.

**Never merge batches.** Each carries one `Origin`, so merging two with different
origins produces a batch whose origin is untrue and silently breaks origin
accounting. Order of operations is fixed: validate the chain → account origins
per batch → *then* coalesce the surviving wire-level changes. Transaction
accounting is correctness; wire coalescing is bandwidth. Keeping them in that
order is what makes the optimisation unable to corrupt the accounting.

If `Select` attaches the updater and then fails, detach before returning; the
framework invalidates it on every **failed** path including panic. After a
successful `Select` it stays valid until `Unselect`, a replacement selection,
session close or connection teardown.

The rest of the contract, all of which you must implement and document: delivery
in push order per selection; ordering across selections undefined; the queue
bounded by **payload bytes as well as item count**; an `Updater` valid only from
`Select` until `Unselect` returns, returning an error afterwards rather than
panicking.

**Coalescing.** Never *drop* a removal. `EXISTS` and flag updates may be
coalesced. `EXPUNGE` stays individually ordered, since each response renumbers
what follows it. `VANISHED` **may** be coalesced into a UID set — carrying many
UIDs is what it is for — except that `VANISHED (EARLIER)` and plain `VANISHED`
are never combined with each other.

**Origin accounting, not blanket echo suppression.** Suppress an update only when
the command's own response fully describes the change: STORE (response carries
the FETCH data) and EXPUNGE/MOVE-out (response carries the removals). APPEND,
COPY and MOVE *into the selected mailbox* still extend the sequence map and still
owe an `EXISTS` — either synthesise it from APPENDUID/COPYUID or let the queue
event through, never simply drop it. Match a change token against the command
that produced it; "drop everything originating in this session" is the shape that
loses an EXISTS the client needed.

On overflow: **never drop an update.** A dropped EXPUNGE desynchronises the
client permanently and silently; a dropped connection is detectable.

Overflow handling **must not route through the event loop**, because synchronous
writing means the loop may be blocked mid-write on a large FETCH to the very
client that stopped reading — which is exactly when the queue overflows. So the
push path itself marks the connection fatal, cancels the connection context,
signals the event loop, and starts a short forced-close deadline. The loop sends
`BYE` if it regains control in time; otherwise the forced close tears down the
connection and unblocks the stuck write. Cancelling the context is what makes the
deadline terminate rather than merely expire.

## Resource limits are a design input, not a hardening pass

The threat model is inverted from the client's: you face hostile
**unauthenticated remote clients** at connection rates a client never sees.

- Pre-auth limits are separate and much tighter: command line length, literal
  size (a pre-auth `APPEND {2147483647}` is refused *before* allocation, not
  after), commands per connection, short auth deadline.
- Connection caps, total and per source address.
- Timeouts both directions; RFC 9051 §5.4's 30-minute autologout is a minimum.
- Bound everything the client controls: mailbox name length, flag count,
  sequence-set cardinality, search nesting depth.
- Defaults are **safe**, not permissive-with-a-note.

Parser inputs are the easy half. Bound the expensive half too: queued pipelined
commands; total command execution time; LIST result count and SEARCH result
cardinality (cheap request, unbounded response); FETCH response bytes and
temporary spool bytes; **decompressed** COMPRESS=DEFLATE input (decompression
bomb); authentication attempts per connection, account and source; concurrent
commands and connections per authenticated account; and the backend's own work
budget in messages scanned.

RFC 7888 permits refusing an over-large non-synchronising literal, and APPENDLIMIT
lets cooperative clients avoid the upload entirely — use both.

## Security tests fuzzing cannot reach

Parser fuzzing does not touch any of these, and each is a known historical
vulnerability class. Write them as you go, not at the end:

**STARTTLS plaintext command injection** — bytes buffered after the STARTTLS
command must be discarded, not executed. RFC 9051 §5.1 requires this explicitly
because implementations got it wrong.

Then: incomplete literal followed by disconnect; disconnect *during* APPEND; slow
reader during a large FETCH; slow writer during a large APPEND; update-queue
overflow under a non-reading client; repeated failed authentication; cancellation
while the backend holds locks; SELECT/CLOSE/update races. Add goroutine and
temporary-file leak checks across all of them.

## Protocol baseline

rev1 wire baseline, advertise `IMAP4REV2`, switch behaviour on `ENABLE
IMAP4rev2` — the mirror of the client's settled decision, for the mirror reason.
The enabled revision is **per-connection state**, so response encoding is a
function of the session, not a global.

`RECENT`/`\Recent` are maintained for rev1 and not sent in rev2. `LSUB` and
`LIST (SUBSCRIBED)` map to the same backend call. `CHECK` is accepted as `NOOP`.

**rev2 is a set of incorporated behaviours, not a list of legacy capability
tokens** — do not conflate them. Advertise `IMAP4REV2` only when all incorporated
behaviour is implemented, which includes requiring `MoveMailbox`.

`BINARY` is the trap: its FETCH side (`BINARY`, `BINARY.PEEK`, `BINARY.SIZE`) is
incorporated into rev2, but RFC 3516's **APPEND** side is not, and the `BINARY`
capability token means full 3516 *including* binary APPEND. So implementing rev2
does not license advertising `BINARY`, and `BINARY` is not a capability rev2
requires. See `.state/progress/T16.md` for the provenance of that split.

`internal/imapwire`'s limits and deadline mechanism is reusable. The values are
not.

## API constraints inherited unchanged from the client

- `ctx context.Context` is the first parameter of every blocking method,
  including every backend method.
- All options are structs; `nil` means defaults.
- All protocol errors are `*imap.Error` with a `ResponseCode` — no per-extension
  error types. Note the direction inverts: you *produce* these and encode them as
  `NO`/`BAD` with a response code.
- `internal/` never appears in an exported signature.

## Errors face the other way

A client surfaces a server's failure to its caller. A server turns a caller's
failure into a wire response. A backend returning an arbitrary `error` must
become a sensible tagged `NO` or `BAD` with an appropriate response code — and a
backend error must never leak internal detail (paths, SQL, stack) to an
unauthenticated client.

Record progress in `.state/progress/<task>.md` (gitignored). Your spec is in `docs/tasks/`.
