# Roadmap

Target: **v1.0 with a frozen public API**, not a permanent beta. The exit
criteria below are objective — a milestone is done when they pass, not when it
feels finished.

Task IDs refer to `docs/tasks/BOARD.md`.

## M0 — Foundation (T01, T02)

The wire codec and the core vocabulary. No network/session layer, no client.

This milestone is foundational and blocks everything else: it fixes the types
that appear in every later signature, and those are exactly the types that are
expensive to change later. T01 and T02 can proceed in parallel because their
ownership boundaries do not overlap.

**Exit:** fuzz targets run clean for 5 minutes; `package imap` imports nothing
from the module; the three open-ended sets (FETCH/SEARCH/STATUS) are reviewed and
signed off by `api-guardian`.

## M1 — A client that works (T03, T04, T05, T06, T12)

Connection, TLS/STARTTLS, SASL, mailbox commands, message commands, and the
interoperability harness.

**Exit:** the full Dovecot interop suite passes; a real account can be listed,
selected, searched, fetched and appended to.

## M2 — Negotiation and the extensions that matter (T07, T08, T09)

ENABLE, CAPABILITY handling, IDLE, IMAP4rev2 activation, then extension groups A
and B.

**Exit:** the M2 acceptance matrix (Dovecot, Stalwart, GreenMail) is green;
CONDSTORE/QRESYNC resynchronisation is verified against two independent servers.
This acceptance set is independent of the harness expense tiers in
`docs/INTEROP.md`.

**Status (2026-08-01):** exit criteria met. T07–T09 are done; Group A is fully
`verified`; Group B is `verified` except APPENDLIMIT/REPLACE (`done`, single
advertising server each).

## M3 — Full coverage (T10, T11)

Extension groups C, D and E. Tier-2 servers (Cyrus, Courier) join the matrix.

**Exit:** `docs/RFC-COVERAGE.md` has no `planned` rows outside the explicitly
`deferred` set.

**Status (2026-08-01): complete.** T10 and T11 are merged; Groups C–E rows are
`done`. The orphan `ID` (RFC 2971) capability is implemented (`Client.ID`), and
coverage has no `planned` rows outside the deferred set.

## M4 — Hardening and the freeze (T13, T14, T15, T17)

Fuzzing corpus, API surface review, documentation and examples, release
engineering, and the bidirectional vocabulary audit.

**Exit:** `apidiff` gate active in CI; API surface test passes; every exported
symbol has a doc comment; examples compile and run against the matrix; **and
`package imap` has been reviewed from the server direction (T17), with any
reshaping of an existing type landed before the tag.**

The last criterion was added 2026-08-03 and is the one thing v1.0 gained from
scoping the server early. Rationale: adding types after the freeze is additive
and always allowed, but reshaping one is not, and a vocabulary exercised in only
one direction can hold a type a server can consume but cannot naturally produce.
A client-side review cannot find that, because the client is the direction that
works. See `docs/tasks/T17-bidirectional-vocabulary-audit.md` and
`docs/SERVER-DESIGN.md` §9.

A verdict of "nothing needs to change" would have been legitimate, but the audit
found and fixed several one-directional shapes before the freeze. The full
per-symbol record lives in `.state/progress/T17.md`.

**Status (2026-08-01, after audit):** T13 and T14 were marked done and both were
reopened. An audit of that claim found seven issues, all since fixed:

- 28 exported `Client` methods took no options struct, violating rule 3. This is
  the irreversible class — it cannot be repaired after v1.0.
- `docs/API-STABILITY.md` §3 asserted, incorrectly, that a method without an
  options parameter can gain one without breaking callers. That false premise is
  what licensed the above. Rule 3 now has a mechanical gate, as rules 2, 6 and 7
  already did.
- Extension groups C–E shipped with **no** fuzz targets, against the standing
  rule that every parser gets one. Targets went 26 → 60, and the campaign runner
  now discovers them rather than reading a hand-maintained list.
- `staticcheck` had 21 findings, `gofmt` was dirty, and nothing compiled
  `examples/**` at all.

Both previously-outstanding items are since closed: a full 10-minute campaign
over all 61 discovered fuzz targets completed 2026-08-03 (61/61 pass, no
crasher — three `context deadline exceeded` hits at the fuzztime boundary
confirmed as a `FUZZ_PARALLEL=4` scheduling artifact, not a real failure), and
the native interop matrix re-ran green against the changed signatures.

**Status (2026-08-06): complete.** T15 and T17 were pushed and merged through PR
#2. GitHub CI was green, including the platform/version test matrix,
vet/staticcheck/gofmt, `apidiff`, the zero-dependency gate and all four
fuzz-smoke shards. The annotated `v1.0` tag was cut at `47a6958` and pushed to
both remotes. M4's exit criteria are met.

## M5 — Server design (T16) — runs *before* v1.0

Design document only. No `imapserver` code.

This milestone was moved ahead of the v1.0 tag on 2026-08-03. The implementation
still waits; the design does not, because the design is what tells us the freeze
is safe (see M4's added exit criterion). Running it during the T15 hold costs
nothing on the critical path.

**Exit:** `docs/SERVER-DESIGN.md` approved by the human, including its
recommendation on the `imapserver` versioning carve-out (§8); T19–T25 specs
written against the approved abstraction.

**Status (2026-08-05): complete.** The human approved revision 5, including the
backend abstraction, supported in-memory backend, resource-limit model and the
nested-module versioning decision. T19–T25 are fully specified.

## v1.0 — API freeze

After this tag, additive changes only. Removals require two minor releases of
deprecation and do not land before v2.

**Status (2026-08-06): released.** The annotated `v1.0` tag freezes the exported
API of `package imap` and `package imapclient`. The approved exception places
`imapserver` in a nested module versioned v0.x independently; execution of that
release structure is T25's responsibility and is recorded in
`docs/API-STABILITY.md`.

### v2 candidates

Incompatible changes deliberately deferred to v2. Each ships in v1 as an
additive opt-in, so in v2 the change is a new default, not a new feature.

- **Password-only SASL selection by default** (T29). With
  `AuthenticateOptions.CredentialKind` unset, `Client.Authenticate` can send a
  password as a bearer token to a server that offers OAUTHBEARER or XOAUTH2
  beside PLAIN, and the server refuses it. v1 keeps that, because callers
  who pass a token as the password rely on it, and changing it would break
  their logins without a compile error. In v2 an unset `CredentialKind`
  could mean `CredentialPassword`.

## M6 — Server implementation (T18–T25) — after v1.0

Server-direction codec and message analysis first — they are the bulk of the
work and neither depends on the backend abstraction. Then the server core,
backend contract and reference backend, the base command set, extensions,
conformance testing, and the `imapserver` release.

**Exit:** the reference server passes Dovecot's `imaptest`; it appears in the
interop matrix as a first-class entry with a capability table comparable to
Dovecot's and Stalwart's; server-side fuzz targets are green over a recorded
campaign; `imapserver` has a documented stability status.

**Status (2026-08-12):** T18 and T21 are complete. The bidirectional shared
codec, command-literal interlock, response encoder, message analysis, streaming
section extraction and metadata-bearing SEARCH evaluator are green under the
race suite, native interoperability matrix and recorded parser campaigns. T19
completed with independent API approval and green PR CI. T20 completed with the
supported memory backend, reusable backend conformance suite, atomic selection
race gate, UID-normalised SEARCH boundary and independent API approval. T22 is
ready; T23–T25 remain dependency-blocked.

**Status (2026-08-14): T24's validation work is in.** Against the exit criteria
above:

- *Interop matrix entry:* done. `goimap` reports a capability table in the same
  units as Dovecot's and Stalwart's, and the profile assertion holds.
- *Recorded campaign:* done. 70 discovered targets at 60 s each, 70/70 pass, no
  crasher, completed 2026-08-14 — including the whole-connection server targets,
  which cover command decoding end to end rather than only the parser. The seed
  corpus now contains captured `mbsync` and `imaptest` traffic rather than only
  hand-written seeds; a 45 s campaign over the enriched corpus turned up 84 new
  interesting inputs.
- *Third-party client:* done. `mbsync` completes a full sync and a clean resync.
- *`imaptest`:* runs, with two recorded findings and one triaged tool
  limitation — see `INTEROP.md`. It found a real bug on its first contact with
  the server (`STORE` rejecting the unparenthesised flag list, which every test
  in this repository passed because none of them sent that form), now fixed.

The one exit criterion **not** met is "passes Dovecot's `imaptest`" without
qualification. Two conformance findings are open, the more serious being that
untagged `EXPUNGE` is delivered while a pipelined `FETCH`/`STORE`/`SEARCH` is
still in progress, which RFC 3501 §7.4.1 forbids precisely because it
desynchronises message sequence numbers. Fixing it means changing when
`drainUpdates` flushes, which is shared server-core machinery and wants its own
regression tests rather than a change landed at the tail of T24.

Turning on `IMAP4REV2` was part of the same pass: it had been gated off behind a
hardcoded `false`, and `SERVER-DESIGN.md` §1 sets the bar at every incorporated
behaviour being implemented. Three gaps had to close first — UIDPLUS, a bare
`SEARCH` answering in the rev1 shape, and `SELECT` sending no untagged `LIST`.

**Status (2026-08-21): T18–T25 and both releases are complete.** T25 fixed the
pipelined-EXPUNGE defect above, delivered the documented v0.x API and examples,
and executed the nested-module release sequence. GitHub-verified SSH-signed
`v1.1.0` and `imapserver/v0.1.0` tags are published; standalone server builds,
the resolved dependency gate, all 70 fuzz targets and a clean external import
are green.

**Status (2026-08-21): M6 is closed.** The last imaptest finding was fixed by
publishing mailbox-wide `FLAGS` updates before a newly created keyword appears
in FETCH and by keeping the memory backend's applicable-keyword registry
persistent after the last message reference is removed. The triage entry is
gone, a raw loopback regression pins the wire ordering, and the unfiltered
`TestImaptestStress` workload passes. All numbered tasks and every M6 exit
criterion are complete; the scripted-corpus skip remains the documented
imaptest `LITERAL+` runner limitation, not a server finding. See `INTEROP.md`.

## Post-M6 — reliability and event delivery

The original milestones are complete. Root `v1.2.0` and server
`imapserver/v0.3.0` are published; the two module lines remain independent.
Current execution status lives on the issue tracker, not in this plan.

The next work is ordered deliberately:

1. Root `v1.2.1`: release the imapcli UID-summary race fix and UNSEEN-label
   correction, with the persistent-backup-directory warning from
   [#41](https://github.com/kiliant/go-imap/issues/41). Keep optional timestamp
   refinements separate from this patch.
2. [#34](https://github.com/kiliant/go-imap/issues/34): strengthen backend
   conformance for RECURSIVEMATCH combined with REMOTE and SPECIAL-USE.
   These checks belong to the independently versioned server module.
3. [T30](tasks/T30-unsolicited-events.md), tracked by
   [#44](https://github.com/kiliant/go-imap/issues/44): decode and route
   interleaved responses safely, deliver CONTEXT updates and metadata-change
   notifications, and document event/command cases indistinguishable on the
   wire. Public API additions make this a candidate for root `v1.3.0`.
4. [T31](tasks/T31-uidbatches.md), tracked by
   [#51](https://github.com/kiliant/go-imap/issues/51): UIDBATCHES, published
   as RFC 10022 on 2026-07-31, the day of the coverage table's registry
   snapshot. Apart from the deliberately deferred capabilities, it is the only
   registered capability that neither direction implements. Both directions
   ship together, tagged the same day with the root first: the root minor
   after T30's `v1.3.0`, and the next `imapserver` minor. T30 does not wait
   for it.

**Exit:** the patch is fetchable and builds outside the workspace; backend
conformance catches faulty option combinations; T30's scripted, race, fuzz,
API-compatibility and supporting-server interop gates pass, and so do T31's.
Coverage must name remaining limitations instead of treating command
serialization as a fix for lost events.

The authentication refusal code in
[#21](https://github.com/kiliant/go-imap/issues/21) stays lower priority until
another consumer needs the distinction. The password-only SASL default remains
a v2 candidate, not a prerequisite for this phase.

## M7 — `imapserver` v1

`imapserver` has been feature-complete since M6, but it stays on v0.x by the
approved exception in `docs/API-STABILITY.md`. Most of its API consists of
interfaces that backends implement, and adding a method to one breaks every
backend. So far the only backend is `imapserver/memory`, written by the same
tasks as the framework, and the server broke compatibility in
`imapserver/v0.2.0` (signatures) and `v0.3.0` (behaviour).

**Exit.** The human approved the three criteria on 2026-10-15. Their
operational definition is `API-STABILITY.md`, "When the exception ends":

1. **A second backend.** T32's persistent Maildir backend, built only from
   the exported API and its documentation, passes `backendtest` and server
   interop.
2. **Two quiet releases.** Two consecutive `imapserver` minor releases, the
   first at or after the one containing T32 and the initial full review's
   fixes. Every release from the first through the second minor, patches
   included, has no incompatible change in `apidiff` and no BREAKING entry
   in `CHANGELOG.md`, including behaviour changes. Each counted minor must
   contain a server-module change beyond a root dependency bump; a release
   cut only to advance the count does not count. Any break restarts the
   window.
3. **A full review.** An `api-guardian` review of the whole server surface,
   with its findings resolved: T33.

[T32](tasks/T32-maildir-backend.md), tracked by
[#53](https://github.com/kiliant/go-imap/issues/53), first adds the public
`imapserver/message` helpers and migrates memory to them. Its persistent
Maildir backend then uses only public API. The interop matrix and real-client
suites run against both backends. T30 and T31 are not dependencies; shared
release-note edits need coordination.

[T33](tasks/T33-imapserver-v1.md), tracked by
[#54](https://github.com/kiliant/go-imap/issues/54), starts the full review
once T32 is merged and its contract gaps are resolved or explicitly deferred.
Breaking review fixes land before the release window. It reviews the final
diff before tagging v1. Both modules then carry the same compatibility
promise. Whether their version numbers align is T33's decision, including
what happens if their major versions later diverge.


## Sequencing

```
T01 ──┬── T03 ──┬── T04 ──┐
      │         ├── T05 ──┼── T07 ──┬── T08 ──┬── T10 ──┐
T02 ──┘         └── T06 ──┘         └── T09 ──┴── T11 ──┼── T14 ── T15 ──┐
                                                        │                ├── v1.0
                    T12 ──────────────────────── T13 ───┘                │
                                                                         │
                    T16 ── T17 ──────────────────────────────────────────┘

                    v1.0 ──┬── T18 ── T19 ──┐
                           └── T21 ──────────┴── T20 ── T22 ──┬── T23 ──┐
                                                             │         ├── T25
                                                             └── T24 ──┘
```

T12 (interop harness) has no dependency on the client beyond T03 and should start
early and in parallel — the matrix is worthless if it arrives after the code it
was supposed to validate.

T18 and T21 are M6's parallel pair, and between them they are most of the server
project. Neither waits on the backend abstraction, so if M6 needs to start
quickly, it starts with those two.
