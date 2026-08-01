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

**Status (2026-08-01):** T10 and T11 are merged; Groups C–E rows are `done`.
Exit is not yet met: the orphan `ID` (RFC 2971) capability remains `planned`
(never in T08's scope table; see the coverage footnote).

## M4 — Hardening and the freeze (T13, T14, T15)

Fuzzing corpus, API surface review, documentation and examples, release
engineering.

**Exit:** `apidiff` gate active in CI; API surface test passes; every exported
symbol has a doc comment; examples compile and run against the matrix.

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

Exit is still not met. T15 (CI jobs, apidiff gate, CHANGELOG, release-candidate
tag) has not started; a full 30-minute campaign over all 60 targets and an
interop re-run against the changed signatures are both outstanding.

## v1.0 — API freeze

After this tag, additive changes only. Removals require two minor releases of
deprecation and do not land before v2.

## M5 — Server framework (T16)

Design document first, then implementation. Deliberately after v1.0 of the
client: the shared `package imap` vocabulary already makes this additive, so
there is no cost to waiting and a real cost to rushing it.

## Sequencing

```
T01 ──┬── T03 ──┬── T04 ──┐
      │         ├── T05 ──┼── T07 ──┬── T08 ──┬── T10 ──┐
T02 ──┘         └── T06 ──┘         └── T09 ──┴── T11 ──┼── T14 ── T15 ── v1.0
                                                        │
                    T12 ──────────────────────── T13 ───┘
```

T12 (interop harness) has no dependency on the client beyond T03 and should start
early and in parallel — the matrix is worthless if it arrives after the code it
was supposed to validate.
