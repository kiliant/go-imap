# T18 — Server-direction codec

**Agent:** `wire-protocol` · **Milestone:** M6 · **Depends on:** T16 approved,
v1.0 tagged

**Owns:** new files in `internal/imapwire/` (`command.go`, `respenc.go` and
their tests), the new `internal/imapcodec/**`, and — for the migration in step 3
only — `imapclient/{fetch,search,structure}.go`.

That last one crosses T06's ownership. It is deliberate and is justified in
`docs/SERVER-DESIGN.md` §10: the ownership rule is a lock that makes *concurrent*
work safe, T06 is complete, and a completed task's lock passes to the task that
supersedes it.

## Why this is startable before the abstraction is settled

Nothing here depends on the backend abstraction in `SERVER-DESIGN.md` §2. This is
the wire, and the wire is fixed by the RFCs. Together with T21 it is the bulk of
the server project, and both can proceed while §2 is still under review.

## The finding this task exists to fix

`SERVER-DESIGN.md` §0, in short: the grammar primitives in `internal/imapwire`
are direction-agnostic and reusable as-is, but the *semantic* codec is not. About
1 400 lines across `imapclient/{fetch,search,structure}.go` exist in exactly one
direction, and the server needs the other one for the same types.

| Exists | Missing, and needed by the server |
|---|---|
| `BeginResponse`, `ExpectRespCond`, `ExpectRespText` — decode responses | decode **commands**; encode resp-text and response codes |
| `writeSearchCriteria` — encode criteria | **decode** criteria into an evaluable tree |
| `readFetchResponse` — decode fetch data | **encode** fetch data |
| `readEnvelope`, `readBodyStructure` — decode | **encode** (generation from message bytes is T21, not this task) |

## Deliverables

### 1. Command decoding — `internal/imapwire/command.go`

The mirror of `resp.go`. `BeginCommand` returning tag and command name, then
per-command argument decoding built on the existing primitives.

- Reuse `Atom`, `Astring`, `String`, `Number`, `List`, `Literal`, `Mailbox`,
  `ListMailbox`, `Flag` unchanged. If a primitive needs a change to serve the
  command direction, that is a finding — record it, do not fork the primitive.
- **Literal handling inverts.** The client announces a literal and waits for `+`;
  the server *receives* the announcement and must decide whether to send `+`,
  reject with `BAD`, or accept silently under LITERAL+ / LITERAL-. Non-
  synchronising literals mean the payload is already in flight and cannot be
  refused — only drained. That asymmetry is the delicate part of this file.
- Command syntax errors produce `BAD` with the offending tag where the tag was
  parseable, and `BAD` untagged where it was not. Recovering the stream to the
  next command line is part of the contract, not an optimisation: a server that
  drops the connection on every syntax error is a denial-of-service against
  itself.

### 2. Response encoding — `internal/imapwire/respenc.go`

Untagged and tagged response construction, `resp-text-code` with arguments,
continuation requests. Thin over the existing `Encoder`.

### 3. `internal/imapcodec` — both directions for the shared vocabulary

Envelope, body structure, fetch data, search criteria, status items. Both encode
and decode, for the types in `package imap`.

**Migrate the client's existing code; do not rewrite it.** Two implementations of
BODYSTRUCTURE that must agree byte-for-byte is a bug generator, and the client's
is the one with fixtures, fuzz corpus and five servers' worth of interop behind
it. Move it, then add the opposite direction alongside.

The migration must be exported-surface-neutral. `api_surface_test.go` and the
`apidiff` gate both assert this; if either fires, the migration leaked
`internal/` into a signature and violates API-STABILITY §6.

### 4. Fuzz targets

Standing rule — every parser gets one. The command decoder's threat model is
worse than the response decoder's: hostile input arrives from *unauthenticated
remote clients*, per `SERVER-DESIGN.md` §8. Targets cover command decoding and
criteria decoding, with the bar unchanged: no panic, no hang, no unbounded
allocation.

The corpus starts from real client traffic, not invention — capture from the
interop matrix's clients and from `imaptest`.

## Non-negotiables

- `internal/imapwire` and `internal/imapcodec` stay internal. They must not
  appear in any exported signature — API-STABILITY §6, no exceptions, and the
  reason the parser can still be rewritten.
- Zero dependencies, including test code.
- The client's behaviour does not change. Full `-race` suite and the native
  interop matrix must be green after the migration, and that is the acceptance
  gate for step 3 — not the unit tests alone.

## Done when

- A command line can be decoded to a typed command, and every response form the
  client can parse can be encoded by the server side.
- `internal/imapcodec` round-trips every type in `package imap`: encode → decode
  → equal, as a property test over the existing fixtures.
- `imapclient` uses `internal/imapcodec` and holds no private copy.
- Fuzz targets are green over a recorded campaign, per `docs/tasks/T13`'s policy.
- Full `-race` suite green, native interop matrix green, exported surface
  unchanged (`apidiff` reports no diff).
