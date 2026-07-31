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
| [T16](T16-server-framework.md) | Server framework design | M5 | v1.0 tagged | `docs/SERVER-DESIGN.md` | — |

## Critical path

```
T01 ──┬── T03 ──┬── T04 ──┐
      │         ├── T05 ──┼── T07 ──┬── T08 ──┬── T10 ──┐
T02 ──┘         └── T06 ──┘         └── T09 ──┴── T11 ──┼── T14 ── T15 ── v1.0
                                                        │
                    T12 ──────────────────────── T13 ───┘
```

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
