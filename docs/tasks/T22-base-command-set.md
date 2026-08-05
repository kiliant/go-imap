# T22 — Base command set, server side

**Agent:** `server-core` · **Milestone:** M6 · **Depends on:** T20, T21

**Owns:** `imapserver/cmd_*.go`

## What this task is

The rev1-baseline commands that are **backend-delegated** or **cooperative**
per §3's table, wired from T19's dispatch table to T20's `Session` /
`SelectedMailbox` methods and T21's message-analysis helpers. An IMAP server
without these is not an IMAP server (§2) — this is the mandatory-interface set
made to actually speak the protocol.

Framework-owned commands (CAPABILITY, ENABLE, NOOP, LOGOUT, ID, CHECK,
STARTTLS, COMPRESS=DEFLATE, UNSELECT) are T19's, not this task's — they never
call a `Session`/`SelectedMailbox` method and have no backend-facing shape
decision to make.

## Deliverables, grouped by backend method

- **`cmd_list.go`** — `LIST`, `LSUB` (rev1) → `Session.List` with a selection
  option; both map to one backend call per §1. rev2's `LIST (SUBSCRIBED)` goes
  through the same path.
- **`cmd_mailbox.go`** — `STATUS`, `CREATE`, `DELETE`, `RENAME`, `SUBSCRIBE`,
  `UNSUBSCRIBE` → the matching `Session` methods.
- **`cmd_select.go`** — `SELECT`, `EXAMINE` → `Session.Select`, exercising the
  atomic select boundary (T20 §2) end to end: build the framework's UID↔seqnum
  map from `SelectSnapshot.UIDs`, attach the connection's update queue, resolve
  `\Recent`/`NOMODSEQ`/read-only from the one snapshot rather than separate
  lookups. This is the command where getting the ordering wrong reintroduces
  the three-step race §2 exists to prevent — do not decompose it back into
  separate status/enumerate/register calls at the command-handling layer either.
- **`cmd_append.go`** — `APPEND` → `Session.Append`, streaming the announced
  literal via `io.Reader` per §2 ("never materialise an unbounded result"), with
  the pre-auth and post-auth size limits from §8 enforced *before* the backend
  call, not after.
- **`cmd_fetch.go`**, **`cmd_store.go`**, **`cmd_search.go`**, **`cmd_copy.go`**,
  **`cmd_expunge.go`** → `SelectedMailbox.Fetch` / `.Store` / `.Search` /
  `.Copy` / `.Expunge`, all writer-style (T20) so a large `FETCH 1:* BODY[]`
  streams rather than buffers. `SEARCH` criteria are parsed by T18's codec, then
  UID-normalised into `*SearchQuery` (T20) before reaching the backend — verify
  this task does not construct one by hand anywhere; only the framework wrapper
  does.
- **`cmd_move.go`** — `MOVE`, when the backend implements the optional
  `MoveMailbox` interface (§2). **Never synthesise MOVE from
  Copy+Store+Expunge** — refuse with a clear error when the interface is
  absent, matching the capability table's advertisement gate from T19. This is
  a named non-negotiable in the design, not a style preference.
- **`cmd_authenticate.go`** — AUTHENTICATE's per-mechanism credential exchange:
  the SASL state machine itself is framework-owned (T19, `internal/imapsasl`),
  but the extracted-credential → `Backend.Authenticate` call and the
  PLAIN/LOGIN/XOAUTH2/OAUTHBEARER wiring are this task's. SCRAM is **not**
  covered here — it needs backend-held key material and is an optional
  interface, deferred to T23.
- **`cmd_idle.go`** — `IDLE`/`DONE` and the `+ idling` timing are framework-owned
  (T19), but this file is where the selected-mailbox update path actually gets
  exercised end to end against a live `SelectedMailbox`/`Updater` pair; land it
  here since it has no meaning before `cmd_select.go` exists.

## Non-negotiables

- No command handler bypasses the UID-normalisation wrapper for SEARCH, or
  performs its own seqnum→UID resolution outside the framework's map — both
  are the exact centralization §2 exists to buy.
- `imap.SearchCriteria`, `imap.FetchData`, `imap.Envelope`, `imap.BodyStructure`
  etc. flow through T18's `internal/imapcodec` in both directions; no command
  handler hand-rolls wire encoding for a type the codec already covers.
- Every command handler respects the origin-accounting table from §4 (T19
  implements the mechanism; this task is responsible for calling it correctly
  — e.g. STORE must carry its `ChangeToken` via `MutationOptions.Origin` so its
  own FETCH response can suppress the redundant queue event).
- Resource limits from §8 (FETCH response bytes, SEARCH result cardinality,
  LIST result count, total command execution time, backend work per command)
  are enforced in this layer, not assumed to be the backend's problem alone.

## Done when

- A loopback client (ours) exercises the full rev1 command surface against
  `imapserver/memory` (T20): LOGIN through AUTHENTICATE, SELECT, APPEND, FETCH,
  STORE, SEARCH, COPY, MOVE (when advertised), EXPUNGE, IDLE with a concurrent
  writer connection, LOGOUT.
- MOVE's refusal path (backend without `MoveMailbox`) is tested explicitly, not
  just its happy path.
- Full `-race` suite green; `imapserver/backendtest` still passes for `memory`
  after this task's changes.
- `api-guardian` reviews any exported symbol added (command-adjacent option
  structs, mainly) for the sentinel-guard and options-struct rules.
