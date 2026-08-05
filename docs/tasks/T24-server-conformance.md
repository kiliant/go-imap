# T24 — Server conformance, interop and fuzzing

**Agent:** `fuzz-hardening` + `interop-harness` · **Milestone:** M6 · **Depends on:** T22

**Owns:** `imapserver/**/*_fuzz_test.go`, `interop/servers/goimap/**`

## What this task is

`SERVER-DESIGN.md` §7's validation strategy, executed. Loopback tests (ours
against ours) are the inner loop everywhere else in this project and stay that
way here — they catch regressions but not a shared misreading of the RFC, which
is exactly the failure the client's interop matrix exists to catch and the
reason this task exists as a distinct, external-facing check.

## Deliverables, in the design's descending order of value

### 1. Dovecot `imaptest` against our server, containerized

The highest-value single external check available — the de-facto IMAP
conformance and stress tool, run against `imapserver` (backed by `memory`) in
podman, matching this repo's existing container conventions
(`docs/INTEROP.md`).

### 2. `imapserver/memory` as an `interop/servers/` entry

Reuses T12's existing harness (fixture seeding over APPEND, per-capability
reporting) and makes our own coverage directly comparable to Dovecot's and
Stalwart's rows in the same matrix. This is the **one** entry where a profile
assertion failure means our bug, not a broken container — call that out in the
harness output rather than reporting it identically to a third-party server
failure.

**The harness must stop assuming every profile has a container.** `memory` runs
in-process; T12's server-startup path currently assumes a podman image per
profile and needs a native/in-process branch added. Coordinate with
`interop-harness` ownership boundaries in `BOARD.md` — this is exactly the kind
of harness-shape change that belongs to this task rather than being bolted onto
T12's existing files after the fact.

### 3. Real client software against our server

`mbsync`/`isync`, `offlineimap` at minimum. These exercise long-tail
sequencing no suite written alongside the server thinks to test on its own:
UIDVALIDITY change mid-sync, resumed partial fetches, CONDSTORE replay.

### 4. Server-side fuzzing — non-optional, mirrors T13

The command parser (T18) faces hostile input from **unauthenticated remote
clients** — a larger, more exposed threat surface than the client's
hostile-server case (§8). Same bar as T13: no panic, no hang, no unbounded
allocation. Corpus from captured real client traffic and `imaptest`, not
invention, matching this project's standing rule against synthetic-only fuzz
seeds.

Extend to `internal/imapcodec`'s command-decode and criteria-decode paths (T18)
and `internal/imapmessage` (T21) if either lacks coverage by the time this task
starts — do not assume "T18/T21 shipped a fuzz target" without checking; verify
against `docs/tasks/T13`'s discovery-based runner.

### 5. `imapserver/backendtest` run against `memory`, in CI

T20 built the suite; this task is responsible for it running continuously
against the reference backend as a conformance gate, not just being available
for third parties to opt into.

### 6. Stateful security tests

Each one is a named historical vulnerability class, per §7 — parser fuzzing
does not reach any of them:

- STARTTLS plaintext command injection (T19 built the defence; this task owns
  the regression test living permanently in the suite, if it is not already
  covered by T19's own acceptance criteria — de-duplicate rather than write it
  twice).
- Incomplete literal, then disconnect. Disconnect *during* APPEND.
- Slow reader during a large FETCH; slow writer during a large APPEND.
- Update-queue overflow under a non-reading client (T19 built the mechanism;
  same de-duplication note).
- Repeated failed authentication.
- Cancellation while the backend holds locks.
- SELECT/CLOSE/update races.
- Goroutine and temporary-file leak checks across all of the above — run under
  `-race` and with a leak detector at suite teardown, matching this project's
  existing bar for the client side.

## Non-negotiables

- Interop tests **skip** on absent capability, never fail — the standing rule
  from `CLAUDE.md` applies identically to server-side interop.
- Fuzz targets follow `docs/tasks/T13`'s policy: discovered, not hand-listed;
  campaigned, not merely present.
- No test in this task may weaken or route around the resource limits from §8
  — a slow-reader/slow-writer test that raises the limit to make the test pass
  faster defeats its own purpose.

## Done when

- `imaptest` runs green (or with documented, triaged exceptions — not silently
  skipped) against `imapserver`+`memory` in CI.
- `memory` appears in the interop matrix as a first-class entry with its own
  capability-coverage row, matching the client-side servers' format.
- At least one real third-party client (`mbsync` or `offlineimap`) completes a
  full sync/resync cycle against `imapserver`+`memory`.
- A recorded fuzz campaign (per T13's model — count, duration, discovered
  targets) is green with no crasher, covering command decoding at minimum.
- Every stateful security test in the list above exists, passes, and is
  wired into the same CI path as the client's `-race`/interop suites.
