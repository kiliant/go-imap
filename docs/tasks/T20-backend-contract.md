# T20 — Contract validation, `imapserver/memory`, `backendtest`

**Agent:** `server-core` · **Milestone:** M6 · **Depends on:** T19, T21

**Owns:** `imapserver/{memory,backendtest}/**`, plus focused corrections to
T19's `imapserver/backend.go` that the two independent implementations expose.

## What this task is

The exported backend abstraction introduced by T19 (`SERVER-DESIGN.md` §2), plus
the two things that put real pressure on it before it freezes: a working
reference implementation and a conformance suite a third party can run against
their own. Per §6, an abstraction validated only by its designer's mock is
validated by nothing — this task exists to make that not true here. Focused
contract corrections discovered here are expected; wholesale redesign is not.

## Deliverables

### 1. Validate `backend.go` — the mandatory interfaces

Verify the three landed by T19 against §2's sketch (reviewed and build-clean,
but non-binding on naming details — correct any name that reads wrong in real
use and record why):

```go
type Backend interface {
	Authenticate(ctx context.Context, conn *ConnInfo, cred *Credentials,
		opts *AuthenticateOptions) (Session, error)
}

type Session interface {
	List(...) error
	Status(...) (*imap.StatusData, error)
	Create(...) error
	Delete(...) error
	Rename(...) error
	Subscribe(...) error
	Unsubscribe(...) error
	Append(...) (*imap.AppendData, error)
	Select(ctx context.Context, mailbox string, updater *Updater, opts *SelectOptions) (*SelectResult, error)
	Close(ctx context.Context) error
}

type SelectedMailbox interface {
	Status(...) (*imap.MailboxStatus, error)
	Fetch(...) error
	Search(...) (*SearchResult, error)
	Store(...) error
	Copy(...) (*imap.CopyData, error)
	Expunge(...) error
	Unselect(ctx context.Context) error
}
```

Ten methods on `Session`, seven on `SelectedMailbox` — the rev1 baseline, and
per §2 the count is not the point: **the set is closed**. No future RFC may add
a method here; it adds a new optional interface or a new options-struct field.
Land the golden method-set test from T19 against these interfaces specifically,
if it is not already exercising them.

**Backends see UIDs, never sequence numbers**, everywhere in this file. That is
the framework's single most load-bearing correctness decision (§2) — do not let
a convenience overload reintroduce a sequence-number parameter anywhere in this
surface.

### 2. The atomic select boundary

`Select` takes `*Updater` and returns `*SelectResult{Mailbox, Snapshot}` as one
call, per §2's contract:

> `Snapshot` describes one atomic mailbox state. `updater` is attached to that
> same state before `Select` returns. Every change committed after that state is
> delivered exactly once, or the connection is terminated.

`SelectSnapshot` carries the ordered UID list (which *is* the sequence-number
map), status, flags, permanent flags, read-only bit, `NumRecent`,
`HighestModSeq`/`NoModSeq`, and `Revision`. Get the three-step race described in
§2 into a test even though it is a backend author's bug to make, not the
framework's — `backendtest` (below) is where that test lives permanently.

If `Select` attaches the updater and then fails, the backend must detach before
returning; document this obligation directly on the method, not only in this
spec.

### 3. Optional capability interfaces

Land only an optional interface whose operation is already exact enough to
freeze here. T20 validates `MoveMailbox` and its pre-selection `MoveSupport`
witness because §2 settles both signatures and T22 consumes them directly.
CONDSTORE/QRESYNC, SCRAM, NOTIFY and NAMESPACE are named design requirements but
their method signatures are deliberately left to T23, which owns both their
shape and first implementation. New interfaces are additive and cheap to add
there; a speculative declaration shipped here would not be free to remove.

### 4. `SearchQuery` — the UID-normalisation wrapper

A framework-only type, not constructible outside the framework, whose invariant
(no `imap.SearchSeqNum` survives anywhere in the tree, including recursively
inside `SearchNot`/`SearchOr`/`SearchAnd`) is established at construction. Land
the gate test described in §2: walk the normalised tree over the *fuzz corpus*
for `imap.SearchCriteria`, not a handful of hand-written examples, and fail if
any `SearchSeqNum` survives.

### 5. `imapserver/memory` — the reference backend

Ships as a **supported** package, not test-internal (§6):

- Implements every optional interface *this release* claims to support — not
  every one that will ever exist. Document that scope explicitly so it reads as
  a decision, not an oversight, each time a new extension lands without a
  matching `memory` update.
- Package doc states plainly: not durable, not for production.
- Minimal exported surface — constructor, options struct (sentinel-guarded per
  API-STABILITY rule 7), and the interface methods. No pathological-state
  control here.
- Uses `internal/imapmessage` (T21) for search evaluation and structure
  generation — it is exactly the "simple backend" §5 describes, as opposed to
  an indexed backend that would translate criteria into a native query instead.

### 6. `imapserver/backendtest` — the conformance suite

A suite any backend (including `memory`) runs against itself. Per §7, this is
the direct mitigation for the runtime cost of type-assertion-discovered
capabilities: a backend author who gets a method signature subtly wrong gets a
silently-unadvertised capability rather than a compile error, and this package
is where that gets caught instead.

Required checks, verbatim from §7 — these are the invariants §2 and §4 rely on
and never re-derive, so a backend that violates one corrupts the framework's
state silently rather than failing visibly:

- `len(Snapshot.UIDs) == Status.NumMessages`
- UIDs strictly ascending and non-zero
- `UIDNext` greater than every UID present
- `NumRecent` no greater than the message count
- the batch chain starts at `Snapshot.Revision`

Also: pathological-state control (forcing a UIDVALIDITY change, forcing an
error path) lives here, not in `memory`'s production surface — the §6
constraint this package exists to satisfy without compromising `memory`'s
minimalism.

## Non-negotiables

- No method may be added to `Backend`, `Session` or `SelectedMailbox` by this
  task or any later one. A new requirement is a new optional interface.
- `imapserver/memory` must not become a second production server product —
  scope creep here is exactly the open-ended promise §6 rejected in revision 1.
- Every exported options struct is sentinel-guarded from its first commit.

## Done when

- `memory` passes `backendtest` end to end, including every required
  self-consistency check.
- Two independently authenticated sessions can select and mutate the same
  `memory` mailbox through the backend contract, with the observing selection
  receiving a gap-free revision chain. The permanent race gate proves a change
  concurrent with `Select` appears exactly once: in the snapshot or its first
  update.
- T22's loopback suite exercises SELECT, FETCH, STORE, APPEND, EXPUNGE and IDLE
  against `memory`, including a concurrent second connection. Those wire
  commands do not exist until T22, so making their test a T20 prerequisite would
  create a T20→T22→T20 dependency cycle; T20 validates the same boundary
  directly and T22 owns the end-to-end wire gate.
- `api-guardian` signs off on `backend.go`'s exported surface specifically —
  this is the interface every third-party backend implements, and it is the
  single most consequential freeze in the server project.
