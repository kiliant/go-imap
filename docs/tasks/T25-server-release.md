# T25 — Server API review, docs, examples, `imapserver` release

**Agent:** `docs-release` + `api-guardian` · **Milestone:** M6 · **Depends on:** T23, T24

**Owns:** `imapserver` doc comments, `examples/server/**`

## What this task is

The mirror of T14/T15 for the server: the final exported-surface review, the
documentation and runnable examples, and shipping `imapserver` under the
versioning decision `SERVER-DESIGN.md` §9 makes and that human approval of the
design document covers.

## The versioning decision this task executes

**`imapserver` is a nested module with its own `go.mod`, versioned v0.x
independently while the root module is v1.x** (§9, reversed from revision 1's
same-module carve-out recommendation — the reasoning is in the design doc, not
repeated here).

Concretely:

- Set up `imapserver/go.mod` as a nested module. Use a `go.work` at the repo
  root for development ergonomics (interdependent-module development without
  committing `replace` directives) — this is explicitly what makes the nested
  module workable in practice, per §9.
- `imapserver`'s `go.mod` requires the root module (`github.com/kiliant/go-imap`)
  at a real released version, bumped deliberately on each root release the
  server wants to pick up — not a `replace` directive left in place for
  production consumption.
- This is the **one** sanctioned exception to the zero-dependency rule
  (`CLAUDE.md`) — a self-referential `go.sum` entry on our own root module,
  which §9 argues is a narrow, fully-controlled exception rather than a hole in
  the policy. Do not read it as license for any other dependency.
- Two tags per release going forward when both modules move together: e.g.
  `v1.x.y` for the root module and `imapserver/v0.a.b` for the nested one.
  Document the coordination procedure in `docs/tasks/T15`'s release process (or
  its successor doc) rather than leaving it tribal knowledge.
- `imapserver` starts at v0.x deliberately — it does not inherit the root
  module's v1 compatibility promise on day one. State that explicitly in the
  package doc so users don't assume parity with the root module's stability
  guarantee.

`apidiff` (or whatever the root module's compatibility gate is at this point,
per T15) is extended to run against `imapserver`'s nested module too, scoped to
*its* v0.x expectations (a v0.x module may break between minors; the gate here
is about catching *unintended* breaks, not enforcing v1 semantics prematurely).

## Deliverables

- **Doc comments** on every exported `imapserver` symbol: `Backend`, `Session`,
  `SelectedMailbox` and friends get the most scrutiny, since they are the
  contract every third-party backend author reads first. Cross-reference
  `imapserver/backendtest` from `backend.go`'s package doc — a backend author's
  first stop should be "here is how you check you got this right", not just the
  interface list.
- **`examples/server/`** — at minimum: a minimal server backed by `memory`
  (the "hello world" a new backend author starts from), and one example per
  shipped optional interface (CONDSTORE/QRESYNC, ACL, QUOTA, at minimum),
  showing the type-assertion discovery pattern in context. Compile-gated, same
  as the client's `examples/**` (T14 precedent).
- **`CHANGELOG.md`** entry for the `imapserver` v0.x line, separate from the
  root module's entries, given the nested-module split.
- **Final `api-guardian` sign-off** on the complete `imapserver` exported
  surface — this is the actual freeze point for backend authors, parallel to
  T14's for `imapclient`/`package imap`.

## Non-negotiables

- Nothing here reopens `package imap` — that froze at v1.0, before T18 ever
  started. If documenting the server surface surfaces a wish to change a root
  package type, that is a new finding against a frozen API and needs the same
  written-exception process `CLAUDE.md` requires generally, not a quiet fix
  folded into this task.
- No example or doc comment may imply `imapserver`'s v0.x carries the root
  module's v1 stability guarantee — say the opposite, explicitly, since it is
  the one place in this codebase where two different promises sit side by side.

## Done when

- `go doc` over `imapserver` reads as a complete, coherent package surface —
  the same bar T14 applied to the root module and `imapclient`.
- Every example in `examples/server/**` builds and runs against `memory`.
- `imapserver/go.mod` + root `go.work` are committed and a clean checkout can
  build and test both modules per the documented procedure.
- `api-guardian` issues written sign-off on the frozen `imapserver` v0.x
  surface, and the human approves the versioning execution (the design's §9
  recommendation was approved in principle with the rest of the design; this
  is where it becomes a real tag).
