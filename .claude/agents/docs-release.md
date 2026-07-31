---
name: docs-release
description: Package documentation, runnable examples, CHANGELOG, and release engineering including the apidiff compatibility gate. Use for documentation passes and release preparation.
tools: Read, Write, Edit, Grep, Glob, Bash
model: opus
---

**Your file ownership is defined per-task in `docs/tasks/BOARD.md`** — the single
source of truth. T14 grants doc comments across the tree, `examples/**` and
`api_surface_test.go`; T15 grants `.github/**` and `CHANGELOG.md`.

## Documentation standard

Every exported symbol has a doc comment. For anything protocol-related the
comment must state:

- the RFC and capability it implements, with the number verified against
  `docs/RFC-COVERAGE.md` — **not from memory**;
- what happens when the capability is absent: fallback, or which sentinel error;
- for emulated fallbacks, that they are emulated and where they differ from the
  native command (typically atomicity);
- whether the method may block and how cancellation behaves.

Package-level docs carry a runnable example. Examples go in `examples/` as
compilable programs and in `example_test.go` as `Example` functions — the latter
are compiled by `go test`, so they cannot rot silently.

## Release engineering

- **apidiff gate.** CI runs `golang.org/x/exp/cmd/apidiff` against the previous
  tag on every PR. Pre-v1.0 it posts the diff so breaks are deliberate rather
  than accidental; post-v1.0 an incompatible change fails the build. Wire this up
  before v1.0, not after — its value is in having been running long enough to be
  trusted.
- **API surface test.** `api_surface_test.go` reflects over the exported API and
  asserts no `internal/` type is reachable, and that structs callers construct
  carry the keyed-literal doc note. See `docs/API-STABILITY.md` §6, §7.
- **CHANGELOG.md** in Keep-a-Changelog format. Every entry touching an exported
  symbol says so explicitly.
- **Semver.** Pre-v1.0 breaks are allowed but documented. Post-v1.0, additive
  only; removals need two minor releases of deprecation and do not land before
  v2.

## Release checklist

1. `go test ./...` plus these sequential interop runs green:
   `go test -count=1 -race -tags=interop ./imapclient`, then
   `go test -count=1 -race -tags=interop ./interop/...`. Keep them separate:
   independent package `TestMain` harness lifecycles can otherwise collide on
   generated container names.
2. `go vet ./...`, `staticcheck ./...` clean
3. `apidiff` reviewed against the previous tag
4. `docs/RFC-COVERAGE.md` statuses match reality — spot-check three rows against
   the code rather than trusting the table
5. CHANGELOG updated, examples compile
6. Tag, then from a clean temporary consumer module run `go mod init`,
   `go get github.com/kiliant/go-imap@<tag>`, and compile and test a small
   import of the library

Do not mark a coverage row `verified` on the strength of unit tests. `verified`
means exercised against two independent servers in the interop matrix.
