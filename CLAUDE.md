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
        ├── internal/imapsasl         SASL mechanisms
        ├── internal/saslprep         SASLprep (RFC 4013) credential preparation
        ├── internal/unicodenorm      NFC/NFKC normalisation, generated tables
        └── imapclient                package imapclient  the client
```

Dependencies point downward only. `package imap` must not import `imapclient`,
and must not perform I/O — it is the shared vocabulary, which is what lets the
future server framework reuse it without an API break.

## Zero external dependencies

The standard library only. A `go.sum` entry is a stability liability we do not
control. This applies to SASL, DEFLATE and charset decoding — all are reachable
with stdlib. Test-only dependencies are also disallowed; the interop harness
shells out to `podman`.

## Testing

- `go test ./...` — unit tests, no network, must stay fast.
- Run `go test -count=1 -race -tags=interop ./imapclient`, then separately run
  `go test -count=1 -race -tags=interop ./interop/...` — drives real servers
  under podman, including interop-tagged production-client tests. The commands
  must remain sequential because separate package test processes own independent
  harness lifecycles and could otherwise collide on container names. See
  `docs/INTEROP.md`. Requires a running podman machine.
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

Everything lives in this repository. All paths are repo-relative — nothing here
refers to an absolute path on one machine.

| | Where | In git |
|---|---|---|
| Task specs, dependency graph, file ownership | `docs/tasks/` | yes — it is documentation |
| Current status, progress notes, scratch | `.state/` | no — mutable coordination state |

`.state/` holds a `.gitignore` containing `*` and `!.gitignore`, so it ignores its
own contents while the protection itself stays tracked — meaning it survives a
fresh clone. `git add -A` cannot stage anything in there.

Do not create `TASKS.md`, `PROGRESS.md` or similar elsewhere in the tree —
mutable state goes in `.state/`, and nowhere else.

The dependency is one-directional by design: `docs/tasks/` never refers to
`.state/` contents, so a fresh clone with no `.state/` is complete and readable.

**Start at `docs/tasks/BOARD.md`.** It defines dependency order and which files
each task owns. **Only edit files your task owns** — that table is what makes
parallel agents safe. Write your working notes to `.state/progress/<task>.md`.

## Commit conventions

Conventional Commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`).
Scope by package: `feat(imapclient): add IDLE support`.
Any commit changing an exported symbol must say so in the body and update
`docs/API-STABILITY.md` if it sets a precedent.
