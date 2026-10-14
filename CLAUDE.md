# go-imap — agent working rules

Goal: a complete, correct, **stable** IMAP client library for Go.
Module path: `github.com/kiliant/go-imap`.

## The one goal that shapes every decision

The reference implementation in this ecosystem has been in beta for years. It did
not stall because of missing features or missing CI. It stalled because **each new
extension forced a breaking change to the public API**, so v1.0 was never safe to
declare.

Therefore the acceptance criterion on every public API decision is:

> **Can RFC N+1 — an extension nobody has written yet — be added to this API
> without a breaking change?**

If the answer is no, the design is wrong. This outranks brevity, elegance, and
resemblance to other libraries. An agent that ships a working feature behind an
unextensible API has failed the task, not completed it.

See `docs/API-STABILITY.md` for the concrete rules. They are not style
preferences; they are the deliverable.

## Non-negotiable API rules

1. **Open-ended sets get open-ended types.** FETCH items, SEARCH criteria, STATUS
   items and capability names are extended by nearly every new RFC. None of them
   may be a closed `enum`-style constant list that callers `switch` on
   exhaustively, and none may be a fixed struct of `bool` fields.
2. **`context.Context` is the first parameter of every blocking call**, from the
   first commit. Retrofitting it is breaking and is the single most common reason
   Go network libraries never reach v1.
3. **Options go in structs, never in positional parameters.** A new RFC adds a
   field; a new parameter breaks every caller.
4. **Exported interfaces are a last resort.** Adding a method to an exported
   interface is a breaking change. Prefer concrete types with unexported fields,
   or function-typed callbacks in an options struct.
5. **One error type.** All protocol failures surface as `*imap.Error` carrying the
   response code, tag and text. Do not add per-extension error types; extensions
   add response *codes*, which is a data change, not a type change.
6. **`internal/` stays internal.** The wire codec must never appear in an exported
   signature, not even as an opaque return value. Once it leaks, the parser can
   never be rewritten.
7. **No struct literals across the API boundary without `_ struct{}` guards or
   documented "always construct via constructor" contracts** — adding a field to a
   struct callers build with unkeyed literals is breaking.

Anything that violates these needs an explicit, written exception in
`docs/API-STABILITY.md` — approved by the human, not by an agent.

## Protocol baseline

- Wire compatibility target is **RFC 3501 (IMAP4rev1)**, with **RFC 9051
  (IMAP4rev2)** behaviour once `ENABLE`d. Most deployed servers are still rev1;
  a rev2-only client cannot be tested against them. This is a settled decision —
  see `docs/ARCHITECTURE.md`. Do not "simplify" it away.
- The authoritative capability→RFC mapping is `docs/RFC-COVERAGE.md`, generated
  from the IANA IMAP Capabilities registry. **Do not invent RFC numbers from
  memory.** If a capability is not in that file, check the IANA registry and add
  it there first.

## Layering

```
github.com/kiliant/go-imap            package imap        core types, errors (no I/O)
        ├── internal/imapwire         lexer, decoder, encoder — NEVER exported
        ├── internal/imapcodec        bidirectional semantic codec
        ├── internal/imapmessage      message analysis and SEARCH evaluation
        ├── internal/imapsasl         SASL mechanisms
        ├── internal/saslprep         SASLprep (RFC 4013) credential preparation
        ├── internal/unicodenorm      NFC/NFKC normalisation, generated tables
        └── imapclient                package imapclient  the client
```

Dependencies point downward only. `package imap` must not import `imapclient`,
and must not perform I/O — it is the shared vocabulary, which is what lets the
future server framework reuse it without an API break.

The server framework is **implemented**: `docs/SERVER-DESIGN.md` (milestone M5
design, M6 implementation). v1.0 was tagged on 2026-08-06 after T17's
bidirectional audit; T18–T24 built the codec, the framework, the base command
set, the extensions and the conformance/interop coverage between 2026-08-12 and
2026-08-14. The root `imap` and `imapclient` APIs are frozen; `imapserver` is a
nested module on the approved v0.x line — see "Two modules" below.

## Zero external dependencies

The standard library only. A `go.sum` entry is a stability liability we do not
control. This applies to SASL, DEFLATE and charset decoding — all are reachable
with stdlib. Test-only dependencies are also disallowed; the interop harness
shells out to `podman`.

## Two modules

Since T25 the repository holds two modules with independent version lines:

| Module | Path | Line | Promise |
|---|---|---|---|
| root | `github.com/kiliant/go-imap` | `v1.x.y` | frozen; an incompatible change fails CI |
| server | `github.com/kiliant/go-imap/imapserver` | `imapserver/v0.a.b` | may break between minors, deliberately |

A committed `go.work` covers both, so ordinary work needs no `replace`
directive. **`./...` stops at the module boundary**, which is the one thing to
remember: a command run at the repository root does not touch `imapserver`.
`.github/scripts/modules.sh` prints the module directories, and every CI job
loops over it rather than naming them.

`imapserver/go.mod` requiring the root module is the single sanctioned
exception to the zero-dependency rule below. It is not licence for another one.
See `docs/RELEASING.md`, which also documents why the root module must be
tagged before the server module can be.

## Testing

- `go test ./...` in **each module** — unit tests, no network, must stay fast.
  `for dir in $(.github/scripts/modules.sh); do (cd "$dir" && go test ./...); done`
  is the whole of it, and is what CI runs.
- Run `go test -count=1 -race -tags=interop ./imapclient`, then separately run
  `go test -count=1 -race -tags=interop ./interop/...`, then separately run
  `go test -count=1 -race -timeout 75m -tags=interop ./imapserver/interop/...` — the first two
  drive real servers under podman, including interop-tagged production-client
  tests; the third is the inverse, measuring our own server as a matrix entry and
  driving real third-party clients (`imaptest`, `mbsync`) against it. The
  commands must remain sequential because separate package test processes own
  independent harness lifecycles and could otherwise collide on container names.
  `.github/scripts/run-interop.sh` runs all three in order, which is what CI
  uses. See `docs/INTEROP.md`. Requires a running podman machine.
- **Full interop runs go to the CI runner**, not the development machine: push
  the branch, then `gh workflow run interop.yml --ref <branch>`. `interop.yml`
  does not run on pull requests, so a branch needs the dispatch. Targeted
  interop tests (`-run`, `GO_IMAP_INTEROP_SERVERS`) are fine locally while
  developing.
- Interop tests **skip** on absent server capabilities, never fail. A permanently
  red matrix is a matrix nobody reads.
- Every parser gets a fuzz target. Malformed input from a hostile server must not
  panic; internal codecs return an error and the public client boundary returns
  an `*imap.Error`.

Host note: development is on darwin/arm64 with podman. The M2 acceptance servers
(Dovecot, Stalwart, GreenMail) are arm64-native; their harness expense tiers are
documented separately in `docs/INTEROP.md`. Apache James is amd64-only and runs
emulated behind `-tags=interop_emulated`.

## Plan vs state

Plans live in this repository; status lives on its GitHub issue tracker. All
paths are repo-relative — nothing here refers to an absolute path on one
machine.

| | Where | Versioned by |
|---|---|---|
| Task specs, dependency graph, file ownership | `docs/tasks/` | git — it is documentation |
| Task status, progress, decisions, review verdicts | one GitHub issue per task, label `task` | the issue |
| Work found during a task and left for later | its own issue, label `follow-up` | the issue |
| Scratch for one session | `.state/` | nothing — ignored, never relied on |

**Issues are public.** Post what a reader of the project should know, at real
milestones: implementation done, review verdict, CI result, hand-over, a
decision and its reason. Notes that matter only to one session stay out of
them.

A task's spec names its issue, and the PR that completes the task closes it
(`Closes #N`). Something found during a task and deliberately left becomes a
`follow-up` issue, not a paragraph buried in a spec; the spec may name the
issue number. Label by module too: `module:root`, `module:imapserver`,
`module:imapcli`.

A spec never depends on its issue to be understood. A fresh clone is a complete
plan; the issues record what happened to it.

`.state/` holds a `.gitignore` containing `*` and `!.gitignore`, so it ignores its
own contents while the protection itself stays tracked. `git add -A` cannot
stage anything in there. Do not create `TASKS.md`, `PROGRESS.md` or similar in
the tree.

**Start at `docs/tasks/BOARD.md`.** It defines dependency order and which files
each task owns. **Only edit files your task owns** — that table is what makes
parallel agents safe. Report progress on the task's issue.

## Commit conventions

Conventional Commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`).
Scope by package: `feat(imapclient): add IDLE support`.
Any commit changing an exported symbol must say so in the body and update
`docs/API-STABILITY.md` if it sets a precedent.
