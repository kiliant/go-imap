# T32 — Public message helpers and a persistent Maildir backend

**Issue:** [#53](https://github.com/kiliant/go-imap/issues/53)

**Milestone:** M7, `imapserver` v1 (`../ROADMAP.md`). It ships in the next
`imapserver` minor release after it lands.
**Depends on:** completed T20 and T21. Complete and review the public-helper
step below before starting storage. T30 and T31 are not dependencies; any
shared release-note edits must be coordinated with them.

## Why this task exists

`imapserver` has been feature-complete since M6 but deliberately stays on
v0.x (`../API-STABILITY.md`, the `imapserver` exception). Its three base
interfaces must work for a persistent backend built only from the exported
API and its documentation before v1 freezes them. So far the only backend
is `imapserver/memory`, written alongside the framework.

A known gap must be resolved first: `SelectedMailbox.Fetch` and `Search`
require completed FETCH data and SEARCH results, but MIME analysis, section
extraction, ENVELOPE/BODYSTRUCTURE generation and SEARCH evaluation exist
only in `internal/imapmessage`. The memory backend imports that package.
A third-party backend should be able to reuse these helpers through a
supported public API rather than have to implement them all again.

This task first supplies that API, then proves it and the backend contract
with persistent storage. The Maildir backend is also useful in its own right.

## Ownership and scope

- Owns new `imapserver/{message,maildir}/**`, including tests, doc comments
  and colocated runnable examples. New parsers land with fuzz targets; T24
  owns those targets afterwards.
- From completed T21: the public-helper boundary described below. The
  implementation stays in `internal/imapmessage`; changes there are limited
  to supporting that boundary, including cancellation. Its existing fuzz
  targets remain with T13.
- From completed T20: migrate the memory backend's message-analysis and
  SEARCH-evaluation uses, including extension files and associated tests,
  to the public helper. No unrelated memory or backendtest changes.
- From completed T24, in `imapserver/interop/`: backend selection in
  `profile.go` (`startOn` takes a backend factory), `serverForClients` in
  `client_test.go`, the profile list in `main_test.go`, and per-backend
  subtest loops in `imaptest_test.go` and `mbsync_test.go`. Suite assertions,
  triage records and testdata remain with T24.
- From completed T15: this task's server-module `CHANGELOG.md` entries and,
  only if measured runtime requires it, the server-stage timeout in
  `.github/scripts/run-interop.sh`. The current budget is `-timeout 75m`;
  record the measurement and reason before increasing it.

**Other contract gaps** are the point of the task, but their fixes are not
owned here. Each gets its own follow-up issue linked from #53 and its own
PR, with a handover from the affected owner: T19, T20, T21 or T23. The
CONDSTORE, QRESYNC and METADATA contracts belong to T23. A breaking fix is
allowed on v0.x, must be marked BREAKING in the changelog, and restarts
M7's two-release count. Record each gap's resolution or explicit deferral.
A gap that prevents the required contract from working cannot be deferred.

## Step 1 — Public message helpers, before storage

Add `imapserver/message`, backed by `internal/imapmessage`. It provides:

- analysis of a stored message from an `io.ReaderAt` and explicit byte size;
- ENVELOPE, BODYSTRUCTURE and message size;
- streaming BODY section extraction, with exact sizes, header-field filters
  and partials;
- SEARCH evaluation using a message, mailbox metadata and open
  `imap.SearchCriteria`, with explicit charset options.

The public message is a concrete type with unexported implementation fields.
Do not alias internal types or expose them in signatures, fields or errors.
Public options and metadata structs are guarded with `_ struct{}` and
constructed with keyed fields. Blocking operations take `context.Context`
first, options go in structs, and no new exported interface or protocol-error
type is introduced. Review the exact public signatures before implementation;
the helper is server-module API and will freeze at server v1.

Document source ownership and lifetime, cancellation, concurrent use,
unsupported criteria and charsets, malformed-message behaviour, and the
lifetime of section readers. Analysis and section extraction must preserve
streaming; a large FETCH cannot require buffering the message. Indexed
backends remain free to evaluate SEARCH themselves.

Migrate every memory-backend call to `Analyze`, `Match` and their associated
internal types to this package, including extension paths. A direct-import
test covers all memory source and test files and forbids
`internal/imapmessage`. Existing memory conformance and loopback tests must
still pass. Add external-package helper tests for exact sections, malformed
MIME, metadata-dependent SEARCH, cancellation and a large streaming source.
This step is a separately reviewable PR and must merge before storage work.
T33 reviews the helper with both memory and Maildir as consumers.

## Step 2 — Maildir backend through the public API

**Imports.** Production code may import the standard library, `imap`,
`imapserver` and `imapserver/message`. Tests may also import public
`imapclient` and `imapserver/backendtest`. An AST-based test checks every
source file, including `_test.go` and build-tagged files: no import path
with an `internal` component, and no `imapserver/memory`. The public helper
is the intentional internal boundary; Maildir cannot bypass it.

**Its own surface.** Use a constructor with an options struct, keyed-only
configuration types and no exported interfaces. Before storage, review its
credential source, account-to-directory mapping and shutdown API. Support
configured accounts through an extensible account options struct; document
supported credential kinds and refuse unsupported kinds. Isolate each
account beneath the root, reject traversal, and never expose passwords in
paths or diagnostics. Any credential callback is context-first with an
options struct. Construction and explicit shutdown may block and therefore
take context first; shutdown stops polling and releases the root lock.

**Storage.**

- **Layout.** One directory per mailbox, nested like the hierarchy, beneath
  its account. Use the path-segment escaping already used by `cmd/imapcli`
  (T28), with `cur/`, `new/`, `tmp/` and filename info flags `D F R S T`.
  Do not import `cmd/imapcli`; document and test the compatible escaping.
  Maildir++ compatibility is not a goal.
- **Mailbox state.** Persist UIDVALIDITY, message-to-UID mapping, UIDNEXT,
  flags, keywords, MODSEQ, mailbox metadata and expunge tombstones with
  their MODSEQ. QRESYNC must report removed UIDs across a restart; document
  any conservative superset or bounded-history policy and test it.
- **Account state.** Persist subscriptions and server-level METADATA
  (mailbox `""`) outside individual mailboxes.
- **Format evolution.** State files are versioned. Refuse newer versions
  without modifying them; migrate supported older versions forward and
  preserve unknown fields through updates. Document the migration policy as
  part of the public storage contract.
- **Crash consistency.** Write state through temporary files, flush data
  and state as required for the documented durability guarantee, rename
  atomically, and sync affected directories where the platform requires it.
  Specify commit/recovery ordering for APPEND, STORE, EXPUNGE, COPY, MOVE
  and mailbox RENAME. After interruption, recover a consistent state or
  change UIDVALIDITY; never reuse a UID under the same UIDVALIDITY, even if
  state is lost. Fault-injection tests cover the commit boundaries.
- **Message bytes.** Preserve APPEND bytes exactly, including CRLF, and use
  file mtime for INTERNALDATE. External delivery may use LF-only lines:
  normalise those to CRLF on ingestion into `cur/`, preserving the delivery
  mtime, then derive RFC822.SIZE, sections and structure from that same
  canonical stored form. Delivery uses `tmp/` and atomic rename.
- **Locking.** Only one backend process owns a root. Use an OS advisory
  lock released on process exit, with standard-library platform files for
  supported systems; unsupported platforms fail explicitly. Document and
  test the platform/filesystem support. A stale `O_EXCL` lock file alone
  is insufficient because a crash must permit restart. Sessions within the
  process share state safely under `-race`.
- **External deliveries.** Poll `new/` with a bounded, configurable interval,
  using no dependencies. Each selected session receives an external
  delivery through its `Updater`; ingestion and concurrent SELECT preserve
  the atomic snapshot/update boundary and revision chain. Failed polls are
  observable and shutdown stops the poller. The framework has no NOOP/CHECK
  refresh hook today; record on #53 whether polling suffices or such a hook
  is needed, through the normal contract-gap process.

**Contract coverage.** Implement `Backend`, `Session` and `SelectedMailbox`,
plus `MoveMailbox`, `CondStoreMailbox`, `QResyncMailbox` and
`MetadataSession`. These stress persistent state rather than only message
bytes. Advertise capabilities only with their required witnesses. The memory
backend implements all of these and many more; other optional interfaces
are not required here.

The base interfaces and shared `Updater`, `UpdateBatch`, `FetchWriter`,
`ExpungeWriter`, `Options` and `Limits` need persistent-backend evidence
because an existing contract cannot be replaced compatibly. Other optional
interfaces may freeze with memory-only evidence only if T33 confirms a
successor interface can be added and preferred without changing the old one
or silently withdrawing a capability.

**Interop.** Keep the memory profile and add a separate Maildir profile with
its own truthful capability floor, fresh root and cleanup. Register both
with the harness and run the existing imaptest and mbsync suites as named,
sequential subtests for each backend. Keep fixture and protocol assertions;
skip only absent capabilities or tooling under the existing policy. Run the
full suite in CI within the measured timeout, recording any skips and triage.

## Done when

- The helper step has merged, passed API review, and memory uses it without
  importing `internal/imapmessage`.
- `backendtest.Run` and race tests pass against Maildir; the import guards
  pass for both packages as described above.
- UIDs, UIDVALIDITY, UIDNEXT, flags, keywords, MODSEQ, tombstones,
  subscriptions, mailbox metadata and server metadata survive restart.
- Fault-injection tests prove recovery and UID non-reuse. Older state
  migrates, newer state is refused unchanged, and unknown fields survive.
- CRLF APPEND and LF external-delivery fixtures have correct bytes, size,
  sections and structure. External delivery reaches every selected session
  within the configured bound, including a concurrent SELECT.
- A second process is refused the root lock; after the owner exits or is
  killed, another process can open it. Shutdown leaves no poller or lock.
- imaptest and mbsync pass for both backends in CI, with findings triaged
  under `../INTEROP.md` and no unexplained skipped Maildir run.
- Every contract gap is linked from #53 with its resolution or explicit
  deferral; the changelog states whether any fix was breaking.
- Both new packages have doc comments, runnable examples and changelog
  entries, and T33 includes them in the v1 review.

## References

- [Maildir](https://cr.yp.to/proto/maildir.html), D. J. Bernstein: the
  `tmp`/`new`/`cur` delivery protocol and info flags.
- `T21-message-analysis.md`: the existing internal implementation.
- `T28-imapcli-backup.md`: the existing layout and escaping.
- `../SERVER-DESIGN.md`: the backend contract and its update path.
