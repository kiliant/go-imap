# T02 — Core types & error taxonomy

**Agent:** `client-core`, reviewed by `api-guardian` · **Milestone:** M0 ·
**Depends on:** nothing

**Owns:** `*.go` in the module root (`package imap`)

Runs in parallel with T01. Both must land before anything else starts.

## Goal

The shared vocabulary: every type that appears in the public API. This is **the
highest-leverage task in the project** — these signatures are what freeze at
v1.0, and the three open-ended sets below are precisely what has kept comparable
libraries in beta for years.

`package imap` performs no I/O and imports nothing from this module. That is what
lets the future server framework reuse it without an API break.

## The three open-ended sets — get these right

FETCH items, SEARCH criteria and STATUS items are extended by nearly every IMAP
RFC. Model each as an open marker interface, never a closed enum or a fixed
struct of bools:

```go
type FetchItem interface{ fetchItem() }   // unexported marker: open to us,
                                          // closed to external implementers
```

Before writing them, list from `docs/RFC-COVERAGE.md` every extension that adds
to each set — CONDSTORE adds `MODSEQ`, OBJECTID adds `EMAILID`/`THREADID`,
SAVEDATE adds `SAVEDATE`, PREVIEW adds `PREVIEW`, BINARY adds `BINARY[...]`,
RFC 8438 adds `STATUS SIZE`, 9208 adds `DELETED`/`DELETED-STORAGE`. Then verify
your design absorbs all of them **as new types, with no change to existing ones**.
That check is the deliverable, not a formality.

Returned FETCH data is keyed, not a fixed struct: a server may send items we do
not model, and dropping them is data loss. Unrecognised items are preserved raw.

## Types

- `Flag` — string-backed named type, **not** an enum. Custom keywords are
  ordinary values. Constants for the system flags.
- `MailboxAttr` — same shape; SPECIAL-USE (6154) and CHILDREN (3348) add values.
- `Envelope`, `Address` — with RFC 2047 header decoding and RFC 2231 parameter
  continuations.
- `BodyStructure` — recursive; single-part and multipart. Include the optional
  extension fields (disposition, language, location).
- `SearchCriteria` — composable, supporting `AND`/`OR`/`NOT` nesting.
- `SeqSet` / `UIDSet` — set arithmetic, `*` handling, range coalescing,
  iteration. They live in `package imap` itself: a sequence set is core
  vocabulary that both a client and a future server framework name in their
  exported signatures, so an internal package underneath it would be a wrapper
  around a type nobody else can see.
- `NumKind` — the UID-vs-sequence-number distinction must be visible in the type
  system, not a `bool` parameter. Confusing them is the classic IMAP client bug.

## Error taxonomy — one type, settled now

```go
type Error struct {
    Type ErrorType     // BAD, NO, BYE, protocol violation
    Code ResponseCode  // string-backed named type, NOT an enum
    Text string
    Tag  string
}
```

`ResponseCode` stays open: RFC 5530 alone defines ~20 codes and more arrive
continuously. Provide constants for known codes; pass unknown ones through
verbatim rather than flattening them to "unknown", which loses data callers may
need. Callers match with `errors.As` and compare `Code`.

No per-extension error types, ever. Extensions add codes, which is a data change.

## Constraints

- No I/O. No imports from this module.
- Every exported struct callers construct carries the keyed-literal doc note
  (`docs/API-STABILITY.md` §7).
- `nil` options pointers are always valid and mean defaults.

## Done when

`api-guardian` has reviewed and approved the three open-ended sets and the error
type — explicitly, in writing, in `.state/progress/T02.md`. This sign-off gates the
whole project; do not mark done without it.
