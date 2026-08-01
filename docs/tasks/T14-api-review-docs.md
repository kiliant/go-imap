# T14 — API review, documentation & examples

**Agent:** `docs-release` with `api-guardian` · **Milestone:** M4 ·
**Depends on:** T10, T11

**Owns:** doc comments across `**/*.go`, `examples/**`, `api_surface_test.go`

## Goal

The last chance to change the public API before it freezes. Treat it as such:
this task's real output is a list of things fixed *now* rather than lived with
until v2.

## The full API review

`api-guardian` walks every exported symbol against the seven rules in
`docs/API-STABILITY.md`, asking the one question: *can the next RFC be added
without breaking this?* Use the `planned` and `deferred` rows of
`docs/RFC-COVERAGE.md` as the concrete list of pressures the API must already
survive — plus `UIDBATCHES` from the watch list, which is the nearest real test
of "an RFC nobody has written yet."

Anything found here gets fixed here. After v1.0 it cannot be.

## `api_surface_test.go`

A test that reflects over the exported API and asserts:

1. No `internal/` type is reachable from any exported signature — including as an
   embedded field, a map/slice element, or an opaque return.
2. Every exported struct that callers construct carries the keyed-literal doc
   note (`docs/API-STABILITY.md` §7).
3. Every exported symbol has a doc comment.
4. Every blocking method takes `ctx context.Context` first. Command-handle
   constructors do not block and correctly take none; their `Wait`, `Next` and
   `Collect` methods are the blocking boundary. See
   [API-STABILITY.md](../API-STABILITY.md) section 2.
5. Every command entry point takes a `*…Options` parameter
   ([API-STABILITY.md](../API-STABILITY.md) §3), even when the struct is empty
   today — a method that ships without one can never gain one without breaking
   callers.

Mechanical enforcement, so the rules survive contributors who have not read the
doc.

## Documentation

Every exported symbol documented. Protocol-related symbols must state:

- the RFC and capability, **verified against `docs/RFC-COVERAGE.md`, not recalled**
- behaviour when the capability is absent: fallback or sentinel error
- for emulated fallbacks, that they are emulated and how they differ — typically
  atomicity
- whether it blocks, and what cancellation does

## Examples

In `examples/` as runnable programs, and as `Example` functions in
`example_test.go` so `go test` compiles them and they cannot rot:

1. Connect, authenticate, list mailboxes
2. Fetch envelopes of the 10 most recent messages
3. Stream a large attachment to disk without buffering
4. IDLE for new mail
5. Incremental sync with CONDSTORE/QRESYNC
6. Append with flags and internal date
7. OAuth2 (XOAUTH2 / OAUTHBEARER) against a real provider
8. Search with a non-ASCII term

## Done when

`api_surface_test.go` passes. `go list ./... | xargs -n1 go doc -all` reads as
a coherent whole. Examples compile and run against the interop matrix.
`api-guardian` issues a
written `APPROVED` for the v1.0 freeze in `.state/progress/T14.md` — that sign-off is
the gate for tagging.
