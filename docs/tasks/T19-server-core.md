# T19 — Server core: reader/event-loop, state machine, dispatch, capability descriptors

**Agent:** `server-core` · **Milestone:** M6 · **Depends on:** T18, `docs/SERVER-DESIGN.md`
§2 approved, v1.0 tagged

**Owns:** `imapserver/{backend,server,conn,session,state,dispatch,capability}.go`

## Why this waits for §2 and v1.0

Everything here is built around the three mandatory backend interfaces
(`Backend`, `Session`, `SelectedMailbox`) that §2 defines, and around
`package imap` staying frozen underneath it. T18 and T21 could be specified and
built without either; this cannot.

## What this task is

The connection lifecycle and the concurrency model, exactly as decided in
`SERVER-DESIGN.md` §4, plus the state machine and capability derivation from
§1 and §3. This is framework machinery — no command bodies live here beyond
what the framework owns outright (see the table in §3: CAPABILITY, ENABLE,
NOOP, LOGOUT, ID, CHECK, STARTTLS, LITERAL+/-, SASL-IR, UTF8=ACCEPT framing,
COMPRESS=DEFLATE, UNSELECT). AUTHENTICATE's SASL state machine is framework-owned
too (`internal/imapsasl`, mirroring T04), but the credential exchange with the
backend and the base command set proper are T22's.

## Deliverables

### 0. The approved backend contract — `backend.go`

Land the three mandatory interfaces and the framework-owned collaborating types
from `SERVER-DESIGN.md` §2 before the connection core that consumes them. T20
then puts this surface under pressure with `memory` and `backendtest` and may
make focused corrections before its API review. This ordering avoids the stale
circular boundary where T19 required the interfaces but T20, which depends on
T19, was assigned their first declaration.

The contract remains exactly the approved shape: `Backend`, `Session`, and
`SelectedMailbox`; UID-only backend operations; atomic `Select` snapshot plus
updater attachment; writer-style unbounded results; optional interfaces for new
capabilities; sentinel-guarded options structs. The method-set golden gate in
this task records these declarations from their first commit.

### 1. Connection lifecycle — `server.go`, `conn.go`

- `Server` accepts connections, owns TLS configuration and STARTTLS, and applies
  the pre-authentication resource limits from §8 (command line length, literal
  size, commands-per-connection, short auth deadline, connection caps — total
  and per source address) *before* handing a connection to a session.
- **STARTTLS plaintext-command-injection defence** (§7, "Stateful security
  tests"; RFC 9051 §5.1): bytes buffered ahead of the STARTTLS response must be
  discarded, never executed as post-TLS commands. This is a named historical
  vulnerability class — get a test for it, not just an implementation.
- One connection = one reader goroutine + one event-loop goroutine, per §4's
  structure. Do not collapse them; the reader's independence from the event
  loop is what makes prompt disconnect-cancellation possible at all, and
  revision 1 of the design shipped exactly that bug.

### 2. The reader goroutine — `conn.go`

- Owns the decoder (T18's `internal/imapwire/command.go`). Parses command lines
  into typed commands and pushes them onto a **bounded command queue** (bound by
  count *and* the payload-bytes rule from §4/§8 — a queue is an
  attacker-controlled buffer).
- On EOF or read error, cancels the connection context immediately. That
  cancellation is the entire reason this is a separate goroutine from command
  execution — an event loop blocked inside a backend `Fetch` call cannot observe
  EOF itself.
- Coordinates continuations: stalls at a synchronising literal until the event
  loop authorises the `+`; does not admit a new command mid-literal. Non-
  synchronising literals (LITERAL+/-) arrive without permission and can only be
  drained or refused after the fact — the asymmetry T18's `command.go` already
  had to solve at the decode level; this is where it is scheduled.

### 3. The event loop — `session.go`, `dispatch.go`

- Owns all session state: state machine, selected view, UID↔sequence map,
  enabled-extension set. Takes commands off the queue and executes them
  **strictly sequentially**, calling the backend. RFC 3501 §5.5's permission for
  concurrent execution of non-interfering commands is **not** taken — §4
  explains why (the ambiguity is exactly where servers get renumbering wrong),
  and that tightening is deliberately the frozen behaviour, not a placeholder:
  loosening it later is additive, taking it back would not be.
- **Writes synchronously**, from the event loop, never the reader, never the
  backend. This is what gives a writer-style backend method (§2) a live stack
  frame for the duration of its write and lets write errors propagate directly
  to the command that caused them.
- Drains the update queue only at protocol-legal points (never mid-FETCH,
  mid-STORE or mid-SEARCH response — RFC 3501 §7.4.1 / RFC 7162).
- `dispatch.go` is the command-name → handler table. Base command handlers
  themselves are T22's; this task provides the dispatch mechanism and the
  handlers for the framework-owned command set enumerated above.

### 4. The update contract — `session.go`

Implements the receiving side of §4's `Updater`/`UpdateBatch` contract:

- Validates each batch's `Before`/`After` chain against the previous state (and
  against `SelectSnapshot.Revision` for the first batch); a mismatch
  **terminates the connection** — no attempt to continue from a state the
  framework can no longer trust.
- Origin accounting per the table in §4 (STORE / EXPUNGE / MOVE-out suppressed
  when the batch's origin matches the *currently accounted* command; APPEND/COPY/
  MOVE-in never simply suppressed; unrelated origins — including earlier
  commands on the same connection — always delivered).
- Wire-level coalescing **after** accounting, never before: `EXISTS` and flag
  updates may merge; `EXPUNGE` never does; `VANISHED` may merge but
  `VANISHED (EARLIER)` and plain `VANISHED` never merge with each other.
  Batches themselves are never merged (§4 — a merged batch's `Origin` would be a
  lie).
- Overflow handling exactly as specified: the push path does not wait for the
  event loop. On overflow it marks the connection fatal, cancels the context,
  signals the event loop, and starts a short forced-close deadline; the event
  loop attempts `* BYE` under a short write deadline if it regains control in
  time, otherwise the forced close wins. Get the ordering right — §4 spells out
  why "event loop notices and sends BYE" alone deadlocks against a stuck write.

### 5. State machine — `state.go`

Not-authenticated → authenticated → selected, per RFC 3501/9051, including the
STARTTLS-required and LOGINDISABLED gates. The enabled IMAP revision
(rev1/rev2 per §1) and the enabled-extension set live here, per-connection —
never global.

### 6. Capability descriptor table and derivation — `capability.go`

Implements §3's table exactly: `Name`, `RequiresBackend`, `RequiresFramework`,
`Depends`, `States`, `RequiresTLS`, `Enable`, `Available`. `CAPABILITY` output
(and the required re-issue after STARTTLS and after authentication, RFC 9051
§7.2.1) is computed from this table against live connection state — no
hand-written capability list anywhere else in the tree.

Also implements the **feature descriptor** layer (§3): `FeatureID`s with an
activation expression over enabled revision + advertised capabilities (e.g.
`binary-fetch = IMAP4rev2 enabled OR BINARY advertised`). Options-struct fields
that extensions own bind to a `FeatureID`, not directly to a capability token —
this is the mechanism, not merely documented policy.

**Mechanical gates required, not optional:**
- A golden test recording every exported backend interface's method set, so a
  future extension that tries to add a method to an *existing* interface (rather
  than a new one) fails CI instead of getting reviewed in by hand. Same
  mechanism as API-STABILITY's existing rules 2/3/6/7.
- A test failing on any extension-owned options field with no `FeatureID`
  binding — the gate that makes "capability and options field ship together" an
  executed rule instead of a remembered one (§3).
- `MOVE` / `IMAP4REV2` advertisement gated specifically on a backend/session
  `MoveSupport` witness plus `MoveMailbox` on an existing selected handle (§2)
  — the framework must not synthesise MOVE from Copy+Store+Expunge, ever, and
  the capability table is where that refusal is enforced rather than merely
  documented.

## Non-negotiables

- `Session` and `SelectedMailbox` backend methods are never called concurrently
  for the same session (§4's re-entrancy contract) — this task is what makes
  that guarantee true, by construction of the event loop, not by locking inside
  handlers.
- The framework never calls into the backend from the update-delivery path.
- `internal/imapwire`, `internal/imapcodec` stay internal — no leak into any
  `imapserver` exported signature.
- Every growable exported options/config struct here (e.g. `Server`'s options)
  blocks external unkeyed literals via the unexported-sentinel-field pattern
  from day one (API-STABILITY rule 7, applied to the server surface).

## Done when

- A loopback client (ours) can complete the framework-owned connection lifecycle:
  greeting, CAPABILITY, STARTTLS, NOOP and LOGOUT, with correct capability sets
  before and after TLS. Authenticated/selected state transitions are covered at
  the state-machine layer here; end-to-end AUTHENTICATE calls the backend and is
  therefore completed with `cmd_authenticate.go` in T22, as that task's ownership
  already specifies.
- The golden interface-method-set test and the feature-binding gate both exist
  and are wired into CI.
- Stateful security tests for STARTTLS plaintext injection and update-queue
  overflow-under-non-reading-client pass (the rest of §7's stateful list is
  T24's, but these two are load-bearing for this task's own correctness and
  should not wait).
- Full `-race` suite green.
- `api-guardian` has reviewed the exported surface of `imapserver` introduced
  here.
