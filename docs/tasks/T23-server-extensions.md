# T23 — Server extensions, groups A–E

**Agent:** `extensions` · **Milestone:** M6 · **Depends on:** T22

**Owns:** `imapserver/ext_*.go`

## What this task is

The server side of every capability the client already implements in
`imapclient/ext_{a,b,c,d,e}_*.go`, mirroring the same grouping from
`docs/RFC-COVERAGE.md`, plus the extension-shaped pieces §3 called out as
needing their own optional interfaces: CONDSTORE/QRESYNC durable state, ACL,
QUOTA, METADATA, SORT/THREAD, SAVEDATE, PREVIEW, OBJECTID, NOTIFY, NAMESPACE,
and the SCRAM credential interface.

Each capability here is **one new optional interface, or one field on an
existing options struct** — never a method added to `Backend`, `Session` or
`SelectedMailbox`. That is the mechanical rule T19's golden test enforces; this
task is where it gets exercised nine-plus times over, which is the whole reason
§2 was designed the way it was.

## Deliverables, by group (mirrors `RFC-COVERAGE.md`)

- **`ext_a_*.go`** — UIDPLUS (APPENDUID/COPYUID — already covered by `imap.AppendData`/
  `imap.CopyData` per T17), ESEARCH, SEARCHRES, LIST-EXTENDED, LIST-STATUS,
  SPECIAL-USE, CREATE-SPECIAL-USE, CHILDREN, WITHIN. Mostly response-shape and
  LIST-selection-option work on top of T22's `cmd_list.go`/`cmd_search.go`; MOVE
  (RFC 6851) is T22's, not repeated here, since the design ties its interface
  and advertisement gate to the base command set.
- **`ext_b_*.go`** — `CondStoreMailbox` (per-message MODSEQ, mailbox
  HIGHESTMODSEQ, MODSEQ-filtered STORE/FETCH) and the QRESYNC extension of it
  (durable expunged-UID record to answer `VANISHED (EARLIER)`). Both need
  durable backend state no protocol layer can reconstruct (§3) — an in-memory
  shim loses exactly what QRESYNC exists to survive, so `imapserver/memory`'s
  implementation here should be honest about what it can and cannot persist
  across restart, and say so in its doc comment rather than silently
  under-delivering. Also OBJECTID (`MAILBOXID`/`EMAILID`/`THREADID`), SAVEDATE,
  STATUS=SIZE, APPENDLIMIT, PREVIEW, REPLACE.
- **`ext_c_*.go`** — BINARY (FETCH side only — see §1's `binary-fetch` vs
  `binary-append` feature split; APPEND-side binary literals require the
  backend to opt into the `BINARY` capability specifically, not merely rev2),
  CATENATE, MULTIAPPEND, COMPRESS=DEFLATE (framework-owned per §3, cross-check
  before duplicating), UTF8=* family, SORT/SORT=DISPLAY, THREAD, MULTISEARCH,
  PARTIAL, SEARCH=FUZZY.
- **`ext_d_*.go`** — QUOTA/QUOTA=/QUOTASET (quota roots, resource limits and
  usage), ACL/RIGHTS=/LIST-MYRIGHTS (a rights model per identifier), METADATA
  family (annotations at server *and* mailbox scope), **NOTIFY** (RFC 5465 —
  see the dedicated note below), UNAUTHENTICATE, UIDONLY, INPROGRESS,
  MESSAGELIMIT=/SAVELIMIT=, JMAPACCESS.
- **`ext_e_*.go`** — the legacy/niche group, best-effort per
  `RFC-COVERAGE.md`'s existing note (parse/respond, not necessarily full
  command support): LOGIN-REFERRALS, MAILBOX-REFERRALS, URLAUTH family,
  LANGUAGE/I18NLEVEL, CONTEXT=SEARCH/SORT, ESORT, FILTERS. CONVERT,
  IMAPSIEVE=, ANNOTATE-EXPERIMENT-1 stay `deferred` on the server side too,
  matching the client's verdict, unless a concrete server target needs one.
- **`namespace.go`** — optional `NamespaceSession` interface (§3): NAMESPACE is
  usually per-authenticated-user, not global config, so a backend that cares
  implements it; the framework supplies a configured default when absent.
- **`scram.go`** — the optional SCRAM credential interface AUTHENTICATE (T22)
  calls into when present: returns stored salt/iteration-count/StoredKey/
  ServerKey, since only the backend has that material. `AUTH=SCRAM-*` is
  advertised only when this interface is implemented. Channel binding
  (`-PLUS`) stays framework-side via `tls.ConnectionState.ExportKeyingMaterial`
  and is not this task's.

### NOTIFY's lifetime, specifically

§3 flags this explicitly: NOTIFY extends IDLE's push model to mailboxes that
are **not selected**, which is a different lifetime from the `Updater` T19/T20
built (scoped to one selection). Do not implement NOTIFY by widening
`Updater`'s scope — that was the trap the design called out by name. Add a
session-scoped update channel as a new, additive piece of framework surface
(coordinate with `server-core` since it touches `session.go`, which this task
does not own) rather than reshaping the selection-scoped one.

## Non-negotiables

- No PR in this task adds a method to `Backend`, `Session` or `SelectedMailbox`
  — full stop. If an extension seems to need one, stop and escalate per
  `BOARD.md`'s escalation table rather than making the change.
- Every extension-owned options field declares the `FeatureID` that activates
  it (T19's binding gate) — an unbound field fails CI, and that failure is the
  intended outcome of forgetting the binding, not a bug in the gate.
- `imapserver/memory` (T20) is updated to implement each optional interface
  this task ships, per §6's "implements every optional interface *the release
  itself* claims to support" — an extension without a `memory` implementation
  is untested by definition and should not be marked done.

## Done when

- Each capability's row in `RFC-COVERAGE.md` gets a server-side status
  alongside the existing client-side one (extend the table's shape rather than
  duplicating the file).
- `memory` implements every optional interface landed here; `backendtest`
  covers each one's required invariants.
- The golden interface-method-set test and the feature-binding gate (T19) stay
  green after every group.
- Full `-race` suite green; `api-guardian` reviews each new optional interface.
