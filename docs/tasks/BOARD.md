# Task board — dependency order and file ownership

The implementation plan. Task specs are in this directory, one file per task.

This file is **committed**: it is documentation, and it is static — the
dependency graph and file ownership do not change as work proceeds. Mutable
status lives in `.state/status.md`, which is gitignored. See "Plan vs state"
below.

## The two rules that make parallel work safe

1. **Only edit files your task owns.** The ownership column is the lock. If your
   task needs a change to a file it does not own, record the request in
   `.state/progress/<task>.md` and stop. Do not edit across the boundary.
2. **Do not start a task whose dependencies are unfinished.** The order exists
   because these tasks change shared type signatures; starting early produces
   rework, not speed.

Focused unit or integration tests colocated with the production file or file
prefix they exercise inherit that ownership: for example, T03 may add
`imapclient/conn_test.go` or `imapclient/conn_interop_test.go`. This does not
transfer ownership of fuzz tests or shared test infrastructure, which remain
explicitly assigned in the task table.

A **completed** task's lock passes to a later task that explicitly supersedes it,
named in both task specs. The rule exists to make *concurrent* work safe; once a
task is done there is no concurrent writer to protect against, and the
alternative is that finished work becomes permanently unmaintainable. T18 takes
`imapclient/{fetch,search,structure}.go` from T06 on exactly these terms. This
is a narrow exception and needs naming in both specs — not an invitation to edit
across boundaries because a task looks finished.

Where a task owns a directory tree and another task owns a filename pattern
inside it, **the pattern wins**: T13's `**/*_fuzz_test.go` covers
`internal/saslprep/saslprep_fuzz_test.go` even though T04 owns
`internal/saslprep/**`. The task that introduces a parser is expected to land its
fuzz target with it — a parser committed without one is not finished — but T13
owns that file afterwards, and a campaign result or a corpus addition is T13's
call, not the originating task's.

## Tasks

| ID | Task | Milestone | Depends on | Owns | Agent |
|---|---|---|---|---|---|
| [T01](T01-wire-codec.md) | Wire codec | M0 | — | `internal/imapwire/**` | wire-protocol |
| [T02](T02-core-types.md) | Core types & errors | M0 | — | `*.go` (root pkg) | client-core + api-guardian |
| [T03](T03-connection.md) | Connection & session | M1 | T01, T02 | `imapclient/{client,conn,state}.go` | client-core |
| [T04](T04-auth.md) | Authentication & SASL | M1 | T03 | `imapclient/auth.go`, `internal/imapsasl/**`, `internal/saslprep/**`, `internal/unicodenorm/**` | client-core |
| [T05](T05-mailbox-commands.md) | Mailbox commands | M1 | T03 | `imapclient/{select,list,status,namespace}.go` | client-core |
| [T06](T06-message-commands.md) | Message commands | M1 | T03 | `imapclient/{fetch,store,search,append,copy}.go` | client-core |
| [T07](T07-capability-enable-idle.md) | CAPABILITY, ENABLE, IDLE, rev2 | M2 | T05, T06 | `imapclient/{capability,enable,idle}.go` | client-core |
| [T08](T08-ext-group-a.md) | Extensions group A | M2 | T07 | `imapclient/ext_a_*.go` | extensions |
| [T09](T09-ext-group-b.md) | Extensions group B | M2 | T07 | `imapclient/ext_b_*.go` | extensions |
| [T10](T10-ext-group-c.md) | Extensions group C | M3 | T08 | `imapclient/ext_c_*.go` | extensions |
| [T11](T11-ext-group-de.md) | Extensions groups D+E | M3 | T08 | `imapclient/ext_d_*.go`, `ext_e_*.go` | extensions |
| [T12](T12-interop-harness.md) | Interop harness | M1 | T03 | `interop/**` | interop-harness |
| [T13](T13-fuzzing-hardening.md) | Fuzzing & hardening | M4 | T01, T12 | `**/*_fuzz_test.go`, `internal/imapwire/testdata/**` | fuzz-hardening |
| [T14](T14-api-review-docs.md) | API review & docs | M4 | T10, T11 | doc comments, `examples/**`, `api_surface_test.go` | docs-release + api-guardian |
| [T15](T15-release-engineering.md) | Release engineering | M4 | T13, T14 | `.github/**`, `CHANGELOG.md` | docs-release |
| [T17](T17-bidirectional-vocabulary-audit.md) | Bidirectional vocabulary audit | M4 — **blocks v1.0** | T16 | `*.go` (root pkg) | api-guardian + client-core |
| [T16](T16-server-framework.md) | Server framework design | M5 | — | `docs/SERVER-DESIGN.md` | — (human-led) |
| [T18](T18-server-direction-codec.md) | Server-direction codec | M6 | T16, v1.0 | `internal/imapwire/{command,respenc}.go`, `internal/imapcodec/**`, and `imapclient/{fetch,search,structure}.go` for the migration only (lock passes from T06) | wire-protocol |
| [T19](T19-server-core.md) | Server core: backend contract, reader/event-loop, state machine, dispatch, capability descriptors | M6 | T18, §2 approved, v1.0 | `imapserver/{backend,server,conn,session,state,dispatch,capability}.go` | server-core |
| [T20](T20-backend-contract.md) | Contract validation, `memory`, `backendtest` | M6 | T19 | `imapserver/{memory,backendtest}/**` and focused corrections to T19's `backend.go` exposed by those implementations | server-core |
| [T21](T21-message-analysis.md) | Message analysis: bodystructure/envelope generation, search evaluation helper | M6 | T16, v1.0 | `internal/imapmessage/**` | wire-protocol |
| [T22](T22-base-command-set.md) | Base command set, server side | M6 | T20, T21 | `imapserver/cmd_*.go` | server-core |
| [T23](T23-server-extensions.md) | Server extensions, groups A–E | M6 | T22 | `imapserver/ext_*.go` | extensions |
| [T24](T24-server-conformance.md) | Server conformance, interop and fuzzing | M6 | T22 | `imapserver/**/*_fuzz_test.go`, `interop/servers/goimap/**` | fuzz-hardening + interop-harness |
| [T25](T25-server-release.md) | Server API review, docs, release | M6 | T23, T24 | `imapserver` doc comments, `examples/server/**` | docs-release + api-guardian |

T16 is deliberately out of numeric order: it is the design task, it has no
dependencies, and T17 depends on it. See "Why T16 moved" below.

**T19, T20 and T22–T25 have spec files**, written after `../SERVER-DESIGN.md`
was approved by the human on 2026-08-05. T17 completed and v1.0 was tagged on
2026-08-06, opening the M6 implementation gate. T18, T19 and T21 completed on
2026-08-12; T20 is ready, while later tasks remain ordered by the dependencies
above.

T18 and T21 are the pair that never depended on the abstraction at all —
neither's spec changed with approval, and between them they are the bulk of the
server project. M6 starts with those two in parallel; T19 follows as soon as T18
does, since it needs the codec's command decoder.

## Critical path

```
T01 ──┬── T03 ──┬── T04 ──┐
      │         ├── T05 ──┼── T07 ──┬── T08 ──┬── T10 ──┐
T02 ──┘         └── T06 ──┘         └── T09 ──┴── T11 ──┼── T14 ── T15 ──┐
                                                        │                ├── v1.0
                    T12 ──────────────────────── T13 ───┘                │
                                                                         │
                    T16 ── T17 ──────────────────────────────────────────┘

                    v1.0 ──┬── T18 ── T19 ── T20 ──┬── T22 ──┬── T23 ──┐
                           └── T21 ─────────────────┘         │         ├── T25
                                                             └── T24 ──┘
```

## Why T16 moved ahead of v1.0

T16 used to be gated on "v1.0 tagged". It was moved on 2026-08-03, and the
reason is a specific risk rather than eagerness.

The argument for waiting was that the architecture had already paid for the
server — `package imap` holds the vocabulary and does no I/O, so the server
lands without touching an existing signature. That still holds, and it is still
why the *implementation* (M6) waits.

What it does not cover: adding types to `package imap` after v1.0 is additive
and always allowed, but **reshaping an existing type is not**. A vocabulary
exercised in only one direction can contain a type a server can consume but
cannot naturally produce, and no client-side review finds it, because the client
is the direction that works. `../SERVER-DESIGN.md` §0 shows the semantic codec
exists in exactly one direction for every one of these types.

So the design runs before the freeze and the implementation runs after it, with
T17 — the bidirectional audit the design makes possible — as a v1.0 exit
criterion. Design is what tells us the freeze is safe.

T01 and T02 may run in parallel. Both must complete before dependent work
begins — they fix the type signatures every later task consumes.

T12 should start as soon as T03 lands, in parallel with T04–T06. A matrix that
arrives after the code it was meant to validate has no value.

T08–T11 are the genuinely parallel phase: four extension agents, one owning file
prefix each, no shared files.

## Definition of done

A task is done when its tests pass, `api-guardian` has approved any exported
symbol it added, and its rows in `../RFC-COVERAGE.md` are updated.

## Escalation

| Situation | Do this |
|---|---|
| An extension seems to need a breaking change to a core type | Stop. Record it, flag `api-guardian`. Do not make the change. |
| A server response the parser rejects | Save the bytes to `internal/imapwire/testdata/`, note it for T01. |
| An RFC number in `../RFC-COVERAGE.md` looks wrong | Check the IANA registry, fix the doc. Never work from a recalled number. |
| Two servers disagree and both look RFC-compliant | Record both; the client accommodates both. Note it for the doc comment. |

## Plan vs state

| | Where | In git | Why |
|---|---|---|---|
| Task specs, dependency graph, ownership | `docs/tasks/` | yes | Documentation. A clone must be self-contained. |
| Current status, progress notes, scratch | `.state/` | no | Mutable coordination state, not project history. |

`.state/` contains a `.gitignore` holding `*` and `!.gitignore`: it ignores its own
contents, while the rule itself is tracked and therefore survives a fresh clone. Nothing in the repo depends on `.state/` existing:
delete it and the plan is still complete and readable.
