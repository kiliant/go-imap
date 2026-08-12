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
