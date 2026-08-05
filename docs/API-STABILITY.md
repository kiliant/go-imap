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
// Good — LIST is a command handle, so the context arrives with Collect/Next.
func (c *Client) List(ref, pattern string, opts *ListOptions) *ListCommand
// Bad — LIST-EXTENDED (5258), LIST-STATUS (5819), LIST-MYRIGHTS (8440),
// LIST-METADATA (9590) each want another parameter.
func (c *Client) List(ref, pattern string, selectOpts, returnOpts int) *ListCommand
```

A `nil` options pointer must always be valid and mean "defaults". That is what
lets us add *fields to an existing options struct* without breaking callers.

It does **not** rescue a method that shipped without an options parameter.
Adding a parameter to a Go signature breaks every call site regardless of
whether `nil` is an accepted value for it, so the options struct has to be
there from the first commit — even when it is empty. Every command entry point
therefore takes one, and dozens are empty structs today purely to keep that door
open. An earlier revision of this document claimed the opposite; three methods
shipped without options on the strength of it, and were only caught at the v1.0
freeze review.

This rule is enforced mechanically by `TestAPISurfaceOptionsStruct` in
`api_surface_test.go`, which walks every exported `Client` command entry point
and fails when one has no `*...Options` parameter.

There are **two** ways a method escapes that gate, and both need justification:

1. `optionsExemptClientMethods` — command entry points deliberately shipped
   without options. Each entry needs a written exception in this section. The
   map is empty today.
2. `nonBlockingClientMethods` — accessors that are not command entry points at
   all, shared with the context-first gate. A method belongs here only if it
   neither writes to the wire nor waits for a response: capability and session
   accessors reading cached state, plus `Close`, which matches `io.Closer` and
   so can never take an options parameter.

The second list is the looser one and therefore the easier to abuse: adding a
wire-writing method to it silences the rule-3 gate *and* the context-first gate
at once. Adding an entry there is an API decision, not a test fix — it needs the
same scrutiny as an entry in the first list.

**Exception — PARTIAL options (RFC 9394, approved at T10 / v1.0 freeze).**
`*PartialFetchOptions` and `*PartialSearchOptions` require a non-nil pointer
with `Range` set: the PARTIAL modifier has no sensible default range, so a nil
options pointer is invalid and is rejected locally. Optional companion fields
(`ReturnOptions`, future PARTIAL modifiers) remain on those structs once the
pointer is non-nil. Callers must pass `&PartialFetchOptions{Range: ...}` or
`&PartialSearchOptions{Range: ...}`; do not "fix" this by making nil mean a
magic default range.

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
    Exists   func(numMessages uint32)
    Expunge  func(seqNum uint32)
    Recent   func(numRecent uint32)
    Fetch    func(data *imap.FetchMessageData)
    Vanished func(data VanishedData) // QRESYNC; additive, not a break
    // A new unsolicited-response RFC adds a field here. Not a break.
}
```

### The hard case: a server backend — proposed, not yet approved

The server framework's backend is the worst instance of this rule in the
library, and `docs/SERVER-DESIGN.md` §2 proposes an answer that is *not* a
function struct. Summarised here because it is a proposed amendment to this rule
rather than an application of it:

- **A function struct is the wrong shape** for the primary abstraction. IMAP
  backends are stateful and hierarchical (`Backend` → `Session` → `SelectedMailbox`), so
  a flat struct forces every backend to reimplement session plumbing by closure;
  there is no compile-time completeness check; and the nine extensions that
  already want method groups take it to ~60 nilable fields.
- **The proposal is a small mandatory interface set plus optional capability
  interfaces discovered by type assertion.** This does not violate the rule's
  *reason*: a new RFC never adds a method to an existing interface, it adds a new
  single-purpose one. External implementers keep compiling.
- **It needs a gate, not a promise.** The proposed rule is *"a new extension may
  add a new optional interface; it may never add a method to an existing one"*,
  enforced by a golden test over `imapserver`'s interface method sets — the same
  mechanism rules 2, 3, 6 and 7 already have. §3 of this document is the standing
  reminder of what happens when a rule has no gate.
- **Backend→client updates stay a function struct**, exactly like
  `UnilateralDataHandler`, because that direction has the same growth pressure
  and the rule as written is right for it.

Nothing may be written against this until `SERVER-DESIGN.md` is approved.

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

An unexported `_ struct{}` field enforces mechanically what the doc comment
asks for. Adding one is itself a breaking change — it invalidates any unkeyed
literal already written — so it can only be done before v1.0, and after the tag
the doc comment is all we have. The structs a future RFC will most obviously
grow therefore carry the guard: T17 added it to `BodyStructureSinglePartExt`,
`BodyStructureMultiPartExt`, `Envelope`, `FetchDataLiteral`,
`FetchDataBodySection`, `FetchDataBinarySection` and `FetchDataRaw`.

## 8. Presence is data, not an inference

**Rule.** When the absence of a value is a legal protocol state, the type
carries an explicit exported presence field. Presence is not inferred from a
zero value, and is not exposed only through a derived accessor.

The reason is directional. A *decoder* may soundly infer absence from a zero,
because it knows what the wire cannot carry: UIDVALIDITY is never zero, a
message number is an `nz-number`. A *producer* holding a value it did not decode
has no such knowledge, and cannot distinguish "this item is absent", which is
frequently a legal and meaningful answer, from "I have not filled this in yet".
A vocabulary type shared by both directions must therefore state presence rather
than encode it in a sentinel.

A derived accessor — `func (d *T) Received() bool { return d.UIDValidity != 0 }`
— is the worse form of the same defect, because it makes the state readable by
anyone and settable only by the declaring package. It also cannot survive the
type being relocated behind an alias, since Go forbids declaring a method on a
non-local type.

Precedents: `imap.AppendData.HasUID` and `imap.CopyData.HasUIDs` (T17, replacing
a `Received()` accessor), `imap.MultiAppendData.HasUIDs`,
`imap.FetchDataBodySection.HasOrigin`, `imap.ESearchData.HasMin` /
`HasMax` / `HasCount` / `HasAll` / `HasModSeq`,
`imap.MessageLimitPartial.HasLowestUID`, `imap.InProgressData.HasProgress` /
`HasGoal`.

**Scope, stated precisely.** The rule is about states the protocol distinguishes
from a present zero, not about every numeric field. Where the wire itself cannot
carry the zero, a zero *is* the absence marker and no companion field is needed:
`MailboxStatus.UIDNext`, `MailboxStatus.Unseen`,
`MailboxStatus.HighestModSeq`, `ListData.Delimiter` and
`NamespaceDescriptor.Delimiter` are all correct as they stand, and each says so
in its doc comment. The test is whether a *producer* could ever need to mean
"present, and zero".

## 9. A field one direction cannot fill belongs to that direction

**Rule.** A field that only the client, or only the server, can meaningfully
produce does not belong on a type in `package imap`. Put the shared state in the
`imap` type and let the one-sided observation live on the consumer's own type,
which embeds it.

`package imap` is the vocabulary both directions speak. A field one of the two
can never fill is not vocabulary; it is evidence that the type has the wrong
semantic boundary. Documenting it as "the server always leaves this false" is
not a fix, because the field is then permanent: after v1.0 the only remedy is a
second type carried alongside the first for ever.

The shape is an embedded struct, which keeps every field read and method call
working through promotion and costs only the spelling of a keyed literal:

```go
// package imap — what both directions can express
type MailboxStatus struct { Mailbox string; UIDValidity uint32; /* ... */ }

// imapclient — plus what only a decoder can observe
type MailboxStatus struct {
    imap.MailboxStatus
    UIDValidityChanged bool // derived by comparing against this client's cache
    _                  struct{}
}
```

Applied four times by T17: `MailboxStatus.UIDValidityChanged`,
`SortData.Emulated`, `IDData.Received` and `ESearchData.Emulated`. The last was
initially argued to be exempt because "no server-facing contract names it"; that
was wrong on inspection, since `imap.MultiSearchResult.Data` holds an
`ESearchData` by value. **Reachability is transitive** — check what embeds the
type, not only what names it directly.

Where such a split would strand an exported helper that takes the type, move the
helper onto the `imap` type as a **value-receiver method** rather than choosing
between the two spellings: promotion then reaches it from the wrapper, and a
value receiver reaches it from a non-addressable field. That is how
`imapclient.ParsePartialSearchData` became `imap.ESearchData.Partial`.

## Versioning policy

- **v0.x** until every task in the board's M0–M4 milestones is complete and the
  required native interoperability acceptance matrices are green. Harness tiers
  describe execution expense, not release priority. Breaking changes allowed,
  documented in `CHANGELOG.md`.
- **v1.0** freezes the exported API. After it: additive changes only.
- Removal requires a deprecation notice for at least two minor releases, and
  never lands before v2.
- CI will run `golang.org/x/exp/cmd/apidiff` against the previous tag on every
  PR. Post-v1.0 a detected incompatible change fails the build; pre-v1.0 it
  posts the diff as a comment so the break is deliberate rather than accidental.
  The gate is not wired up yet — it is an exit criterion of M4, tracked by
  [T15](tasks/T15-release-engineering.md) — so until then the review in the next
  section is the only thing enforcing this document.

  **`apidiff` cannot see type-alias identity.** It identifies a named type by
  its defining package, so relocating a type to another package and leaving
  `type T = other.T` behind — which is source-compatible, and is the technique
  the standard library used for `context.Context` — is reported as
  `T: changed from T to other.T`, with both sides of the line naming the same
  type. A minimal two-package control reproduces it exactly. When the gate flags
  a move of that shape, discharge it by compiling a consumer written against the
  old spelling; do not trust the tool, and do not silently override it. T17 moved
  ~50 symbols this way and every one of them was reported.
- Go version floor: the two most recent major Go releases.

### Proposed exception: `imapserver` outside the v1 promise — NOT APPROVED

**Status: proposed by `docs/SERVER-DESIGN.md` §9. Not approved. Nothing enforces
it, and no `imapserver` code exists.**

The policy above says "v1.0 freezes the exported API" without qualifying by
package. Taken literally, `imapserver` inherits the freeze the moment it lands —
which would freeze the backend abstraction, the hardest API in this library, on
its first commit, before a single third-party backend has been written against
it. That is the failure this project exists to avoid, relocated one layer up.

**The proposal is a nested module:** `imapserver` gets its own `go.mod` and is
versioned v0.x independently while the root module is v1.x.

An earlier revision proposed the opposite — keeping `imapserver` in this module
with a carve-out enforced by scoping the `apidiff` gate — and that was wrong.
An `apidiff` scope is a gate on *us*; it changes nothing a user sees. Someone
importing a package from a module tagged v1 reasonably expects it not to break,
and Go's compatibility guidance is built on that expectation. No CI
configuration resets it, and a doc comment claiming the package is exempt is a
promise, which is the mechanism this document distrusts everywhere else.

The nested module's costs are real but operational: two tags per release,
explicit version coordination, and a deliberate bump of the root-module
dependency. Two objections raised against it do not survive contact:

- *Development ergonomics.* Go workspaces (`go.work`) exist to develop
  interdependent modules in one repository without committing `replace`
  directives.
- *The `go.sum` entry.* The zero-dependency rule exists because a `go.sum` entry
  is a stability liability **we do not control**. A self-referential entry on our
  own module is one we control entirely, so this is a narrow, well-founded
  exception rather than a hole in the policy.

Fallback, if the nested module proves unworkable in practice: same-module with a
documented stability exception — needing its own separate approval.

Per `CLAUDE.md`, this needs an explicit written approval from the human before it
becomes real. Until then it is a recorded open question, and the safe default is
the literal reading: do not land `imapserver` in a v1 module without deciding.

## Reviewing against this document

The `api-guardian` agent (`.claude/agents/api-guardian.md`) reviews every diff
that touches an exported symbol. Its single question is the one from CLAUDE.md:
*can the next RFC be added without breaking this?* It has authority to reject a
functionally correct change.
