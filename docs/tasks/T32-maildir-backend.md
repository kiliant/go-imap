# T32 — Maildir backend: prove the backend contract from outside

**Issue:** [#53](https://github.com/kiliant/go-imap/issues/53)

**Milestone:** M7, `imapserver` v1 (`../ROADMAP.md`). It ships in the next
`imapserver` minor release after it lands.
**Depends on:** completed T20. It touches no file of T30 or T31 and may run
in parallel with them.

## Why this task exists

`imapserver` is feature-complete since M6 but deliberately stays on v0.x
(`../API-STABILITY.md`, the `imapserver` exception). Most of its API consists
of interfaces that a backend implements. Adding a method to one of them breaks
every backend, so the base interfaces have to be right before v1 freezes
them. So far they have been checked only from inside: the one backend is
`imapserver/memory`, written alongside the framework by the same tasks.

This task writes a second backend, persistent where the reference is not,
and uses only what any third party could use. Every place where the contract
is awkward or insufficient for real storage then shows up before the freeze.
That is M7's first exit criterion. The result is also a useful backend in
its own right.

## Ownership and scope

- Owns `imapserver/maildir/**`: a new package in the server module, together
  with its tests and examples. New parsers land with fuzz targets, which T24
  owns afterwards.
- From completed T24, narrowly: in `imapserver/interop/profile.go`, a second
  server configuration that runs the existing imaptest and mbsync suites
  against this backend. The suites themselves do not change.
- This task's entries in `CHANGELOG.md`, under the server module.

**Contract gaps** are the point of the task, but their fixes are not owned
here. Each gap goes on the issue and is fixed in its own PR, through the
owner of the affected file (T19 or T20), with that handover named on the
issue. A breaking fix is allowed on v0.x. It must be marked BREAKING in the
changelog, and it restarts M7's two-release count.

## Requirements

**Public API only.** The package may import the standard library, `imap`
and `imapserver`, and in tests `imapserver/backendtest`. It must not import
the root module's `internal/` packages (Go's path rules would allow it) or
`imapserver/memory`. A test parses the package's imports and enforces this.

**Its own surface.** A constructor with an options struct, keyed-only option
types and no exported interfaces. This API becomes part of `imapserver` v1,
so it is reviewed like the rest of the server's surface.

**Storage.**
- **Layout.** One directory per mailbox, nested like the hierarchy. Path
  segments are escaped the way `cmd/imapcli` already writes a backup (T28):
  `cur/ new/ tmp/`, and info flags `D F R S T` in the file name. Maildir++
  compatibility is not a goal. This code may not import `cmd/imapcli`, so it
  reimplements the escaping and documents it.
- **Per-mailbox state.** A versioned state file holds UIDVALIDITY, the UID
  map, UIDNEXT, keywords and MODSEQ. It is written atomically (temp file, then
  rename). After a crash a mailbox comes back consistent, or with a new
  UIDVALIDITY. It never assigns two messages the same UID.
- **Messages.** Bytes are stored exactly as received, CRLF included, and
  INTERNALDATE is the file's mtime. Delivery goes through `tmp/` and a rename.
- **Locking.** One server process owns a root directory, enforced by an
  exclusive lock file. Inside the process, sessions share mailbox state
  safely under `-race`.
- **External deliveries.** A message that an MTA places in `new/` reaches
  every session that has the mailbox selected, through that selection's
  `Updater`, within a bounded and configurable interval.
  - Use polling only, with no dependencies.
  - The contract has no NOOP-triggered refresh hook today: the backend
    publishes changes by itself. Whether a persistent backend needs such a
    hook is one of the contract questions this task answers, and goes on the
    issue.

**Contract coverage.** The base `Backend`, `Session` and `SelectedMailbox`
contract, plus these optional interfaces:
- `MoveMailbox`;
- `CondStoreMailbox` and `QResyncMailbox`, because MODSEQ that survives a
  restart is the hardest state a backend owns;
- `MetadataSession`.

The in-memory backend implements the same set, so this is a second
implementation, and the first persistent one. Others are optional.

## Done when

- `backendtest.Run` passes against the backend, and the race suite passes.
- **Persistence tests:** UIDs, flags, keywords, MODSEQ and metadata survive a
  restart. Simulated crashes leave a consistent store. An external delivery
  into `new/` reaches a selected session within the configured interval. A second process is refused the
  lock.
- The import-restriction test is in place and passes.
- The server interop suite (imaptest, mbsync) passes against this backend in
  CI, in addition to the reference backend. Findings are triaged as for the
  reference backend (`docs/INTEROP.md`).
- Every contract gap found is recorded on the issue, with its resolution. The
  CHANGELOG states whether any was breaking.
- The package has doc comments, a runnable example and a changelog entry.

## References

- [Maildir](https://cr.yp.to/proto/maildir.html), D. J. Bernstein: the
  `tmp`/`new`/`cur` delivery protocol and the info-flag convention.
- `T28-imapcli-backup.md`: the layout and escaping already used in this
  repository.
- `../SERVER-DESIGN.md`: the backend contract and its update path.
