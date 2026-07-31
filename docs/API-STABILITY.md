# API stability rules

This document is the contract that makes v1.0 possible. It exists because the
failure mode we are avoiding is specific and well-documented: a library that
works, but whose public API must break every time a new RFC is implemented, and
therefore can never leave beta.

Each rule below states the extension pressure it absorbs. If you cannot name the
future RFC a design accommodates, the design is not finished.

## 1. The three sets that always grow

FETCH items, SEARCH criteria and STATUS items are extended by nearly every IMAP
RFC. They are the classic v1.0 blockers. History: `BODYSTRUCTURE` (3501),
`BINARY` (3516), `MODSEQ` (7162), `EMAILID`/`THREADID` (8474), `SAVEDATE` (8514),
`PREVIEW` (8970) all added FETCH items. `SIZE` (8438), `MAILBOXID` (8474),
`DELETED`/`DELETED-STORAGE` (9208) all added STATUS items.

**Rule.** Model each as an open type, not a closed enum:

```go
// Good — a new RFC adds a constant; no existing code breaks.
type FetchItem interface{ fetchItem() }

type FetchItemBodySection struct { /* ... */ }
func (*FetchItemBodySection) fetchItem() {}
```

The unexported marker method keeps the set closed to *external* implementers
(so we can add methods later without breaking anyone) while leaving it open to
*us*. This is the only sanctioned use of an exported interface.

**Anti-pattern — do not do this:**

```go
type FetchOptions struct {
    Envelope bool
    Flags    bool
    // ...adding ModSeq bool here is fine, but callers cannot express
    // BODY[HEADER.FIELDS (From To)] at all, so this shape dead-ends.
}
```

Returned FETCH data must likewise be keyed, not a fixed struct: a server may send
items we do not model yet, and dropping them silently is data loss. Unrecognised
items are preserved in raw form.

## 2. context.Context from commit one

Every operation that waits for a server response or transfers caller-sized data
exposes a `context.Context`. Synchronous methods take `ctx context.Context` as
their first parameter. The asynchronous command-handle API is the deliberate
exception: its initiating method may write a bounded command prelude and return
a handle without waiting for the response; each blocking `Wait` or `Next`
method takes the context. Operations that stream caller data, such as APPEND,
take a context on the initiating method as well.

Adding the context at a blocking boundary later is a breaking change — it is
the most expensive retrofit in Go and a frequent cause of permanent v0. New
commands must therefore follow one of the two established shapes rather than
inventing an uninterruptible blocking call.

Cancellation semantics are documented once, centrally: cancelling a command
that is already on the wire invalidates the connection (IMAP has no command
abort except for `IDLE`), so the client marks the connection unusable rather
than desynchronising the stream. `IDLE` cancels cleanly with `DONE` after the
server has accepted it; cancellation before the continuation follows the
general command rule.

## 3. Options structs, never positional parameters

```go
// Good
func (c *Client) List(ctx context.Context, ref, pattern string, opts *ListOptions) ...
// Bad — LIST-EXTENDED (5258), LIST-STATUS (5819), LIST-MYRIGHTS (8440),
// LIST-METADATA (9590) each want another parameter.
func (c *Client) List(ctx context.Context, ref, pattern string, selectOpts, returnOpts int) ...
```

A `nil` options pointer must always be valid and mean "defaults". This lets us
add an options struct to a method that has none without breaking callers.

## 4. Exported interfaces are a liability

Adding a method to an exported interface breaks every external implementer.
Permitted exported interfaces are exactly:

- marker interfaces with an unexported method (rule 1), which external code
  cannot implement, so they are safe to extend;
- interfaces the standard library already defines (`io.Reader`, etc.).

Everything else — notably "backend" and "handler" abstractions — is expressed as
a struct of function fields:

```go
type UnilateralDataHandler struct {
    Exists  func(numMessages uint32)
    Expunge func(seqNum uint32)
    Recent  func(numRecent uint32)
    Fetch   func(data *imap.FetchMessageData)
    // A new unsolicited-response RFC adds a field here. Not a break.
}
```

## 5. A single error type

```go
type Error struct {
    Type     ErrorType    // NO, BAD, BYE, protocol violation, ...
    Code     ResponseCode // e.g. "AUTHENTICATIONFAILED", "OVERQUOTA", "TRYCREATE"
    CodeArgs string       // raw response-code arguments
    Text     string
    Tag      string
    Err      error        // optional underlying protocol/parser cause
}
```

`ResponseCode` is a `string`-backed named type, **not** an enum — RFC 5530 alone
added ~20 codes and more arrive continuously. Named constants are provided for
known codes; unknown codes pass through as-is rather than being flattened to
"unknown", which would be data loss.

Callers match with `errors.As` and compare `Code`. No per-extension error types.

## 6. internal/ never leaks

The wire codec must not appear in any exported signature — not as a parameter,
return value, embedded field, or opaque handle. The moment it does, the parser is
frozen forever. This is enforced by a test that reflects over the public API
(`api_surface_test.go`, task T14).

## 7. Struct literal safety

Public structs that callers construct are documented as keyed-literal-only, and
the API-surface test rejects adding a field to a struct that lacks that doc
comment. Public structs that only *we* construct are safe.

## Versioning policy

- **v0.x** until every task in the board's M0–M4 milestones is complete and the
  interop matrix is green on Tier 1. Breaking changes allowed, documented in
  `CHANGELOG.md`.
- **v1.0** freezes the exported API. After it: additive changes only.
- Removal requires a deprecation notice for at least two minor releases, and
  never lands before v2.
- CI runs `golang.org/x/exp/cmd/apidiff` against the previous tag on every PR.
  Post-v1.0 a detected incompatible change fails the build; pre-v1.0 it posts the
  diff as a comment so the break is deliberate rather than accidental.
- Go version floor: the two most recent major Go releases.

## Reviewing against this document

The `api-guardian` agent (`.claude/agents/api-guardian.md`) reviews every diff
that touches an exported symbol. Its single question is the one from CLAUDE.md:
*can the next RFC be added without breaking this?* It has authority to reject a
functionally correct change.
