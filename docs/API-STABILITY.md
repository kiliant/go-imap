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

**A raw-preservation field keeps an item when a later release starts decoding
it** — added by T29, 2026-10-14. The callers who read an item raw are exactly
those using one the library does not decode yet. If decoding it moved it out of
the raw field, they would break with no compile error. So where a type keeps a
raw field apart from its decoded fields, the raw field holds every item
received, and a decoded field is an additional view of one of them. Precedents:
`imap.ESearchData.Values` ("modelled items appear here too") and
`imap.ListData.Extended`, which keeps OLDNAME and CHILDINFO beside `OldName` and
`ChildInfo`.

FETCH predates this and is the exception. `FetchMessageData.Items` holds one
value per item received, and `FetchData` documents that a value is
`*FetchDataRaw` only while the library does not yet model its item. When a later
release models one, that key's value changes type. RFC 5257's ANNOTATION, named
under "The next one" below, is the candidate. That is documented v1 behaviour
and stays. Do not bring FETCH into line by appending a raw duplicate beside the
typed value: that would misreport what the server sent. The rule above applies
to a raw field kept apart from its decoded views.

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

**A test-support package inside a v1 module is still public API — T24,
2026-08-14.** `interop/definition` is scaffolding: nobody outside this repository
has a reason to import it, and `.github/scripts/apidiff.sh` excludes `/interop`
from the compatibility gate. Neither fact changes the rule. `Profile.Native`
shipped as `func(context.Context) (*NativeServer, error)`, and the pressure was
already recorded in the file that assigns it — a native profile cannot exercise
STARTTLS without a `TLSConfig` it has nowhere to put. It now takes a
`*NativeOptions`, empty today, exactly as the paragraph above prescribes. The
apidiff scope is a gate on us; it changes nothing a user of a module tagged v1
sees, and the versioning policy below already rules that an exclusion is not a
licence.

Separate options types when commands already have different policy. The server's
`GetMetadataOptions` carries depth and size limits; `SetMetadataOptions` is
independent and empty. `ACLOptions`, `QuotaOptions`, `LanguageOptions` and
`ComparatorOptions` remain shared, deliberately documented on each type: a
future command-specific field must name its command and be ignored by the
other methods. URLAUTH similarly has separate `GenURLAuthOptions`,
`URLFetchOptions` and `ResetURLAuthKeyOptions` after its pre-v1 correction.

Stored-message helpers follow the same rule. In particular
`message.Message.OpenBodySection` takes `*message.SectionOptions` for helper
policy. Its shared `imap.FetchItemBodySection` describes a wire request and
cannot carry server-only transformation policy. This replaces the T32 helper
exemption before the server freeze; its reader, size and error results remain.

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

Server diagnostics follow the same rule: `imapserver.Options.OnError` is a
function field receiving a guarded `ServerEvent`, with an open string
`ServerEventKind`. New diagnostic kinds and event fields are additive. The
callback runs serially outside connection loops and locks; delivery is bounded
and best effort, with overflow counts on subsequent events. Server shutdown
does not wait for callbacks, so callback code may initiate shutdown. This is an
application of rules 1, 4 and 7, with no new interface or error type. (#139)

### The hard case: a server backend — approved by the human, 2026-08-05

The server framework's backend is the worst instance of this rule in the
library, and `docs/SERVER-DESIGN.md` §2 answers it with something that is *not*
a function struct. Summarised here because it is an approved amendment to this
rule rather than a plain application of it:

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

`SERVER-DESIGN.md` was approved by the human on 2026-08-05. v1.0 was tagged on
2026-08-06 after T17 completed, so `imapserver` implementation tasks T18–T25
may proceed — see `docs/tasks/BOARD.md`.

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

### Client-local response codes

`imapclient.CodeNoCompatibleAuthMechanism` is a synthetic local code, carried
by the existing `*imap.Error` with `ErrorTypeProtocol` when automatic SASL
selection exhausts the advertised compatible mechanisms before writing. Its
constant belongs to the client because it describes a client observation,
not shared wire vocabulary. Callers match both Type and Code; a remote NO/BAD
with the same token must not authorize a fallback. Future SASL mechanisms can
then be added without copying their registry into each caller. This adds no
error type or closed set, and does not relax transport or credential checks.

## 6. internal/ never leaks

The wire codec must not appear in any exported signature — not as a parameter,
return value, embedded field, or opaque handle. The moment it does, the parser is
frozen forever. This is enforced by a test that reflects over the public API
(`api_surface_test.go`, task T14).

## 7. Struct literal safety

Public structs that callers construct are documented as keyed-literal-only, and
the API-surface test rejects adding a field to a struct that lacks that doc
comment. Public structs that only *we* construct are safe.

An unexported `_ struct{}` field enforces mechanically what the doc comment only
asks for: it makes an unkeyed literal fail to compile. Adding one is itself a
breaking change — it invalidates any unkeyed literal already written — so it can
only be done before v1.0. After the tag, the doc comment is all we have.

Comparability is also part of the compatibility promise. An exported struct
that is comparable today must remain usable as a map key and with `==`.
Do not add a slice, map or function field directly to it. A pointer to a richer
policy or result value can carry those fields while preserving comparability.
The struct-literal guard does not protect this property; apidiff checks it.

**Rule.** Every exported struct in `package imap` carries the `_ struct{}`
guard. Exceptions are listed here, each with a reason.

Stating it as a rule rather than as a list of structs is deliberate. T17 first
guarded a hand-picked seven and got the selection backwards on every axis that
matters: it guarded `FetchDataRaw`, which callers only ever receive, and left
unguarded the thirteen `Search*` criteria structs — the smallest and most
caller-constructed types in the library, where `imap.SearchString{imap.SearchKeyFrom, "x"}`
is the literal someone actually writes — along with the five `FetchItem*`
request structs, one of which had already grown a field from RFC 8970's `LAZY`
modifier, and the five `BodyStructure` concrete types that the message-analysis
generator constructs. A judgement call per struct is a judgement call that will
be got wrong; the rule is not.

**Exceptions.**

- `NumRange[N]` — a range is exactly a start and a stop. A third field would
  change what "range" means rather than extend it, so the growth this rule
  guards against cannot occur, and `NumRange[SeqNum]{1, 5}` stays idiomatic.
  Its doc comment records the exception at the declaration.

`SectionPartial` was considered as a second exception, on the argument that
RFC 3501 §6.4.5 fixes its `Offset`/`Size` shape, and rejected: its own doc
comment already promised that fields may be added, which is incompatible with
inviting unkeyed literals.

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
anyone and settable only by the declaring package. It also constrains where the
type can live: Go forbids declaring a method on a non-local type, so when a type
moves to `package imap` and leaves an alias behind, the accessor must move with
it. It cannot stay in the package the callers are in. That is fine for a
genuinely shared predicate and wrong for one that hides state, which is why the
field is the answer and the accessor is not.

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

**Scope: wire data the framework declines to write is still vocabulary** —
recorded by T29, 2026-10-14. Rule 9 is about observations with no wire form:
comparisons against a cache, emulation, receipt markers. A field for wire data
that a correct server could send belongs in `package imap`, even when the
`imapserver` framework declines to write it from backend results. That refusal
is framework policy and can change without touching the root type. Precedents:

- `FetchDataRaw`, refused because it cannot be bounded;
- `ListData.OldName`, written only where what was negotiated gives it meaning;
- `ListData.Extended`, never written, because unknown items cannot be gated on
  what the client asked for.

The test is whether a correct server, given the data, could put it on the wire.

## Versioning policy

- **v0.x** until every task in the board's M0–M4 milestones is complete and the
  required native interoperability acceptance matrices are green. Harness tiers
  describe execution expense, not release priority. Breaking changes allowed,
  documented in `CHANGELOG.md`.
- **v1.0** freezes the exported API. After it: additive changes only.
- **Documented behaviour is frozen too** — recorded by T29, 2026-10-14.
  `apidiff` sees signatures, not behaviour, and a default documented in v1 is
  part of the promise. If one is judged wrong after v1.0, it gets an additive
  opt-in on the existing options struct, and the flip is recorded under "v2
  candidates" in `docs/ROADMAP.md`. The server resource-limit exception below
  is deliberately narrower than this general rule. First exercised by
  `imapclient.AuthenticateOptions.CredentialKind`: unset, `Authenticate` can
  still send a password as a bearer token, as v1 documents.
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

  The alias is permanent. Deleting `type T = other.T` breaks every caller still
  holding the old spelling, and the removal policy above applies to an alias
  exactly as it applies to any other exported symbol — a deprecation notice for
  at least two minor releases, never before v2.
- Go version floor: the two most recent major Go releases.

### Exception: `imapserver` outside the v1 promise — APPROVED

**Status: approved by the human on 2026-08-05, as part of approving
`docs/SERVER-DESIGN.md` §9. The v1.0 prerequisite was satisfied on 2026-08-06;
execution of the nested-module release model remains T25's.**

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

Per `CLAUDE.md`, this needed explicit written approval from the human before it
became real, and it has that approval now. `imapserver/go.mod` and the root
`go.work` are T25's to create, at v1.0.

#### When the exception ends — APPROVED

**Status: approved by the human on 2026-10-15** as milestone M7
(`docs/ROADMAP.md`).

`imapserver` may be tagged v1, and so come under the v1 promise, only when
all three of these hold:

1. **A second backend.** T32's persistent Maildir backend, built only from
   the exported API and its documentation, passes `backendtest` and server
   interop.
2. **Two quiet releases.** Two consecutive `imapserver` minor releases, the
   first at or after the one containing T32 and the fixes approved by the
   second full review. Every release from the first through the second minor,
   patches included, has no incompatible change in `apidiff` and no BREAKING entry
   in `CHANGELOG.md`, including behaviour changes. Each counted minor must
   contain a server-module change beyond a root dependency bump; a release
   cut only to advance the count does not count. Any break restarts the
   window.
3. **Full reviews.** An `api-guardian` review of the whole server surface,
   with its findings resolved, followed by a second full review of the
   integrated corrections before the release window: T33. The human
   explicitly requested this second pass on 2026-10-19. It covers every
   exported symbol and documented behavioural contract in all five public
   server packages on one candidate commit, including unchanged API. Record
   that commit, coverage, evidence and verdict on #54; a diff review alone
   does not satisfy this gate.

Feature completeness is deliberately not a criterion. The server was
feature-complete at M6 and still broke compatibility twice afterwards. The
exception waits for evidence that the contract holds for a persistent
backend built only from the exported API and its documentation. T32 first
adds public message helpers, then uses them alongside the memory backend;
T33 reviews both new packages before the release window.

## 10. The `imapserver` enforcement gates — added by T23, 2026-08-13

Three mechanical gates in `imapserver/capability_test.go` enforce the rules above
for the server framework. They are recorded here because a gate that lives only
in a test file is a gate the next task deletes as a test failure rather than
recognising as an API decision — which is exactly the history section 3 records
about the options-struct rule.

**The interface method-set gate.** `TestBackendInterfaceMethodSets` compares a
hand-written map of every exported interface against an AST scan of the package.
Adding a method to an existing interface, adding an interface, or removing one
all fail it. The map is meant to be edited deliberately: the diff to that literal
is the record of what the API grew.

**The struct guard.** `TestGrowableConfigurationStructsAreGuarded` requires
`_ struct{}` on every exported struct in `imapserver`, walking the package rather
than iterating a list — section 7's point is that the rule is not a judgement
call, and a list makes it one again. Framework-constructed types are exempt
through `unguardedByDesign`, which carries a one-line reason each and is checked
in both directions so a stale exemption fails too.

**The feature-binding gate.** Every field on an `imapserver` options struct that
is not part of the rev1 baseline carries an `imapfeature:"<id>"` tag naming the
feature that activates it, and `TestExtensionOptionFieldsHaveFeatureBinding`
fails on a missing or unknown binding. The framework populates such a field only
when its feature is active for the session, so the pairing between "capability
advertised" and "option field set" is executed rather than remembered. Without
it, a field added for a new RFC is silently ignored by an older backend and the
server claims a capability it does not honour.

### Capability witnesses

A capability whose behaviour the backend must implement is advertised only when
the backend witnesses it. `imapserver` has two witness styles and the choice
between them is not stylistic:

- **`CapabilitySupport`** — `SupportsCapability(ctx, name, options) bool`, keyed
  by the wire token. Use it where support is spread across data the backend
  returns and no type can see it: CHILDREN, SAVEDATE, WITHIN, CONDSTORE. A
  future RFC is then a new *token*, which is a data change rather than a type
  change — rule 1 applied to the witness layer. The context is the connection
  or command lifetime; nil options select the defaults.
- **A structural check** that the session or selected mailbox implements the
  optional interface. Use it where the interface *is* the whole of the support:
  QUOTA, ACL, METADATA. A forwarding wrapper or a partial implementation
  must additionally withdraw any unsupported tokens through the opt-in filter
  below.

Every extension command handler calls `requireCapability` before doing any work,
whichever witness its capability uses. Holding an optional interface is not
consent to advertise it.

### Resolved content carries size and ownership — added 2026-10-19

`URLAuthSession.FetchURLAuth` returns guarded `URLFetchResult`, so later URLFETCH
metadata can grow without replacing its return signature. A nil result means
unresolved. A non-nil result supplies an owned Body that the framework closes
exactly once, including when returned with an error. Size is exact when HasSize
is set, including a known empty body; otherwise bounded disk staging measures
it. A known-size stream failure after literal framing invalidates the connection
rather than attempting to continue with a mismatched byte count.

`Limits.MaxLiteralBytes` bounds individual input literals and aggregate APPEND
and URLFETCH payloads per command. CATENATE stages parts on disk with this bound;
URLFETCH streams known sizes and bounds staging of unknown ones. Refusal drains
remaining client literals without resolving more URLs. These pre-v1 changes
correct the previous unbounded buffering and are recorded as breaking behaviour.
The owner-approved limits policy (#124) permits a minor release to retune
`imapserver.Limits` default values. Every such change must be called out in the
changelog. An application needing stable limits must set positive values
explicitly. This exception covers resource-limit defaults only: the meaning of
explicit values and sentinel values, other options defaults, and backend
obligations remain under the general documented-behaviour promise.
`MaxLiteralBytes` defaults to 32 MiB; non-positive `MaxCommands` means unlimited.
`MaxConnections` and `MaxConnectionsPerIP` use zero for the documented default
and negative values for unlimited. All remaining non-positive fields select
their documented defaults; retained queues, sizes and timeouts remain bounded.

### Maildir format compatibility — approved 2026-10-20 (#180)

A v1 minor release may introduce a forward-only Maildir format migration.
Migration must preserve mailbox/message identities and unknown fields at the
supported record boundaries, recover prepared older journals, and retain
refusal of unsupported newer versions rather than risk stale or corrupt data.
Every format bump must identify the new version and migration/rollback effects
in the changelog. These documented migrations are compatible for the stability
window; downgrade compatibility is not promised. To roll back to an older
reader, restore a complete backup made before migration with the backend closed.
This exception does not permit identity resets, data loss or silently dropping
unknown fields.

### Atomic batches require an explicit backend contract — added 2026-10-19

`MultiAppendSession.MultiAppend` receives the whole command's messages through
borrowed readers and returns the shared `imap.MultiAppendData`. Each guarded
`AppendMessage` carries per-message flags, date and declared-size presence;
`MultiAppendOptions` carries command-wide mutation origin and future settings.
The optional interface promises all-or-nothing storage and one atomic update
batch. Its absence withdraws MULTIAPPEND; the framework never simulates the
operation with repeated `Session.Append` calls. This corrects the pre-v1
advertisement, deliberately recorded as breaking behaviour in CHANGELOG.md.

The framework stages a complete APPEND command to a temporary file before any
backend call, even for one message: the command's shape and later failures are
not known until every literal and its terminator are consumed. Backends still
read streams, but are called after staging rather than while the client sends.
Cancellation and parse/zero-length failures discard staged data. Memory
implements the atomic contract. Maildir's existing journal recovers by rolling
forward after a prepared commit failure; it does not implement this stricter
rollback contract and therefore does not advertise MULTIAPPEND. This interface
adds no obligations to the mandatory Session method set.

### Capability withdrawal is a separate opt-in policy — added 2026-10-19

`CapabilityFilter.AllowCapability(ctx, name, options)` can withdraw a
backend-backed capability after its ordinary witnesses succeed; true never
grants support. The Session's filter answers alone when present, otherwise the
Backend's filter is used. Without either, the existing witness rules are
unchanged. The name is the upper-case descriptor token before a dynamic value
is appended, such as `APPENDLIMIT`; nil options select the defaults. A filter
that only suppresses specific tokens should return true for all other names.

Do not reinterpret false from `CapabilitySupport` as a veto of structural
support: existing implementations correctly return false for unfamiliar names,
including structural tokens. The new interface is what makes withdrawal an
explicit, additive opt-in. In particular a server-only metadata backend can
withdraw `METADATA`, retain `METADATA-SERVER`, and avoid promising mailbox
annotations or `LIST-METADATA`. Command gates enforce the remaining scope.

Withdrawal prunes dependent capabilities transitively. IMAP4REV2 also consults
the filter for its backend-backed incorporated members, even when a member is
not individually advertised in the current connection state. Framework-only
capabilities, including the routing token LIST-EXTENDED, are not filterable.
The filter does not change who evaluates a routing witness. Backend conformance
skips explicitly withdrawn optional behaviours, and still checks retained
server-scope metadata. Implementing a filter never changes the mandatory rev1
backend contract.

### A routing witness chooses who evaluates, not whether to advertise — added 2026-10-15

Some behaviour can neither be refused nor left to an unwitnessed backend. The
first case is RECURSIVEMATCH (RFC 5258 section 3.1). LIST-EXTENDED enables it,
RFC 5258 section 3 allows BAD only for options the server does not recognise,
and IMAP4rev2's LIST incorporates it. A server advertising either therefore
cannot answer it BAD. Before this rule, the framework handed it to every
backend, so one that ignored it answered wrongly.

**The rule: a token may *route* instead of gate. A routing token's true says
the backend evaluates a behaviour itself. Without it, the framework evaluates
the behaviour and the backend never sees the option.** It applies only where
all of these hold:

- The capability's advertisement is already decided by the framework alone.
  A routing token therefore never appears in `rev2Incorporated` or in any
  `RequiresBackend` witness. `TestRoutingTokensGateNothing` checks the
  token's own descriptor and `rev2Incorporated`. A gate on the token in some
  other descriptor is a closure the test cannot see, so review has to catch it.
- The behaviour cannot be refused once the capability is advertised.
- The framework's evaluation includes everything the backend itself returns
  for the same command without the option, and only adds to it. That
  condition, not "the framework could work it out", is what makes a backend
  without the witness correct rather than wrong. Where the evaluation has to
  decide something the backend would decide differently, such as how the
  reference is read, it says so, and only for what it adds.

**Whether a token gates or routes is fixed when the token is first released
and never changes.** Flipping it changes what false means for every released
backend, which is the silent break "Witness tokens are API" below describes.
CHILDREN, for instance, could be worked out from `List "*"`, but it has gated
since v0.1.0 and stays a gate.

For RECURSIVEMATCH the token is `"LIST-EXTENDED"`, the capability that enables
it. A backend witnessing it receives `ListOptions.SelectRecursiveMatch`. For
any other backend, `imapserver/ext_a_list_recursive.go` starts from the
backend's answer to the command without the option, then adds the parents of
the descendants that meet the base options. A base selection option the
evaluation does not handle is refused for such a backend, never passed on.
`TestListRecursiveMatchCoversBaseOptions` keeps the two lists level when RFC
5258 section 9.3 registers a new one. `backendtest` holds a backend that
claims the witness to the native semantics, and skips the case for one that
does not.

### An umbrella capability is witnessed by its members — added by T24, 2026-08-14

`IMAP4REV2` is not a capability a backend implements. It is a claim that every
behaviour RFC 9051 §1 folds in is implemented, which `SERVER-DESIGN.md` §1 calls
"a lie the client cannot detect" when it is not true — the client has no way to
ask which half it got.

T24 shipped it gated on atomic MOVE alone. A backend witnessing MOVE and nothing
else therefore advertised rev2 and was then held to `UID EXPUNGE`, `APPENDUID`,
`COPYUID`, `NAMESPACE` and an untagged `LIST` on `SELECT` it had never agreed to
produce. The api-guardian review of PR #7 demonstrated it against a backend built
to model a T23 third party, and it is the reason this section exists.

**The rule: an umbrella capability's witness is the conjunction of its members'
own witnesses, and the membership is a list, not a predicate.** `rev2Incorporated`
in `imapserver/capability.go` names the members; `witnessesRev2` asks each one's
own descriptor witness rather than repeating it, so a capability cannot be
witnessed one way for its own token and another way for the umbrella.

The list is what makes this survive rule 1. A future revision that incorporates
more extensions adds a token to a slice — a data change. A hand-written
conjunction would make it a code change, and the one thing we know about the next
revision is that nobody will remember to edit it. `TestRev2IncorporatedNamesResolve`
gates the list against the descriptor table, because an unresolvable name reads as
"needs no backend support" and so fails in the direction that advertises *more*.

**There is no pre-authentication special case, and adding one would undo this.**
The first version of the fix had one: it checked atomic MOVE alone before a
session existed, on the reasoning that a structural witness has nothing to assert
against yet. That reinstated the hand-written conjunction one path down, naming
exactly one member — so adding a token to `rev2Incorporated` would have changed
the greeting not at all, silently, which is the failure this section exists to
prevent.

Instead each witness decides for itself what it can say. The spoken ones answer
from the backend, which they can do in any state; the structural ones abstain,
matching `selectedImplements` and `supportsAtomicMove`, which already abstain
before a mailbox is selected for the same reason. So the greeting reflects every
witness that can answer, and the remainder is deferred rather than assumed. That
the deferral is safe rests on `ENABLE` consulting the derived set too: a backend
that loses `IMAP4REV2` on authentication can never have rev2 enabled against it,
which is the only place the advertisement has consequences.

The general rule: **a witness that cannot answer yet abstains; it does not answer
no.** A witness that answers no is believed in every state.

### Search keys and fetch items are gated too — added by T25, 2026-08-14

§10 says every extension *command* handler calls `requireCapability` before
doing any work. That is true, and it does not cover the two lists this document
spends the most words on. **A search key is not a command and a fetch item is
not a command**, so both classes reached the backend with no gate at all: a
backend witnessing nothing still received `FUZZY` and `MODSEQ`, from a server
that advertised neither `SEARCH=FUZZY` nor `CONDSTORE`.

While this repository owned both halves that was invisible and harmless. It
stops being either the moment `imapserver/v0.1.0` exists, because then there is
a population of backends compiled against a fixed set of criteria and items.
Branch (b) of the rule above explicitly does not apply to `imap.SearchCriteria`
or `imap.FetchItem`, so branch (a) is the only justification available — and
branch (a) requires the framework to *guarantee* a new member never reaches a
consumer that predates it, with a test enforcing it. No such mechanism existed;
the framework had no notion of which capability a criterion belonged to.

**The rule: the framework classifies every criterion and every fetch item, and
refuses what it cannot classify.** The tables live in
`imapserver/capability_keys.go` and are data, so RFC N+1 is a new row.
`TestEverySearchKeyAndFetchItemIsClassified` reads the marker-method
declarations in `package imap` and fails when one is missing from them, because
forgetting fails *open* — an unclassified key that nobody notices is one the
framework would hand to a backend ungated, which is the state this rule ends.

Classification is by value, not only by type. `imap.SearchDateKey` and the
other key types inside a criterion are open string sets: RFC 8514 added
SAVEDBEFORE, SAVEDON and SAVEDSINCE to `imap.SearchDate` without a new type.
Classifying `SearchDate` wholesale as baseline let those keys reach backends
that never witnessed SAVEDATE (third T33 review, B1). Every key constant is
therefore a row, unknown values are refused, and the same test discovers the
key types used by criterion fields and fails on any constant without a gate.
MVS makes this matter even without a server release: a newer root module can
add a key under a pinned `imapserver`.

Refusing rather than forwarding is the deliberate half. A key the framework
cannot name is one it cannot gate, and RFC 5257's `ANNOTATION` — the candidate
§10 nominates — is exactly the case: when `package imap` learns it, backends
compiled today start receiving it having witnessed nothing, and the realistic
failure is a permissive `default:` branch returning a silently empty result
rather than anything anyone would notice.

This also closed an advertisement gap of the T24 kind: `imapserver/memory`
evaluated `FUZZY` through the shared evaluator but never witnessed
`SEARCH=FUZZY`, so the key parsed, evaluated and returned results from a server
that never offered it.

### Witness tokens are API — the THREAD rename, recorded 2026-08-14

§10's witness rule makes the *token string* part of `CapabilitySupport`'s
contract: a backend witnesses `"CHILDREN"`, and the framework advertises
CHILDREN. Changing which token the framework asks for is therefore a breaking
change to every backend that spells the old one, and it breaks silently — the
capability simply stops being advertised.

T24 made one: THREAD moved from a bare `"THREAD"` witness to per-algorithm
`"THREAD=ORDEREDSUBJECT"` and `"THREAD=REFERENCES"`. **The change is correct** —
RFC 5256 defines no bare `THREAD` token, and per-algorithm witnesses are what let
a backend implement one algorithm and refuse the other rather than being forced to
claim both. It is recorded here because it is a token *removal*, which is the
breaking direction, and because the window in which it is free is exactly now:
`imapserver` has never been tagged, so the affected population is zero.

After `imapserver` v1.0 a rename of this kind needs the old token to keep working
alongside the new one. Before it, they are free and should be made deliberately.

### Resolved: `CapabilitySupport` context/options and MOVE witness — 2026-08-21

The two pre-v1 decisions recorded here were made immediately after v0.1.0,
while the compatibility window remained open:

- `SupportsCapability(name string) bool` became
  `SupportsCapability(ctx context.Context, name string, options
  *CapabilitySupportOptions) bool`. Capability derivation now threads the
  connection or command context through greetings, CAPABILITY, ENABLE,
  feature gates and extension-command gates. The empty guarded options struct
  gives future RFCs room to extend the query without another method break.
- `MoveSupport` was removed. `CapabilitySupport("MOVE")` is now the sole spoken
  witness for atomic MOVE, while `MoveMailbox` remains the structural witness
  once a mailbox is selected. `witnessesRev2` still consults MOVE's own
  descriptor, so the umbrella and standalone capability cannot drift.

Both changes are deliberately breaking in the `imapserver` v0.x module and are
named in `CHANGELOG.md`. A backend migrating from v0.1.0 adds the context and
options parameters to its capability witness, moves its `SupportsMove` answer
to the `"MOVE"` token, and retains `MoveMailbox` on selected handles.

### Additive root-package growth after v1.0 — exercised 2026-08-13

`imap.SearchFilter` was added to the frozen root package by T23, and it is worth
recording as the first exercise of the rule rather than leaving the next person
to wonder whether it was allowed.

**Adding a type to `package imap` is source-compatible and always permitted.
That is not the same as being safe, and the first draft of this section confused
the two.** Reshaping an existing type is breaking and still needs approval; that
half was never in doubt.

The half that was wrong: the original text argued a new `SearchCriteria`
implementation "adds a case that consumers may ignore — the interface is closed
by an unexported method, so no external type asserts exhaustively over it." The
premise is false. The interface being closed to *implementers* says nothing about
*consumers*, and this library has consumers that must switch over it
exhaustively: every `imapserver` backend receives an `imap.SearchCriteria` and
its only possible implementation is a type switch. A backend compiled before the
new type exists does not fail to compile. It falls to `default` and returns a
wrong answer — for SEARCH, a silently empty result indistinguishable from a
correct search that matched nothing.

That was not hypothetical. FILTER substitution was wired into SEARCH alone while
SORT, THREAD and ESEARCH passed the raw tree through, so the guarantee this
section rested on was already untrue when it was written.

#### The rule, restated

A new implementation of an open marker interface in `package imap` is permitted
when **either**:

- **(a) the framework guarantees it never reaches a consumer that predates it** —
  and the guarantee is written on the consumer-facing declaration, not only in
  the code that upholds it, *and* a test enforces it for every path that reaches
  a consumer; **or**
- **(b) no consumer switches exhaustively over the interface**, and the interface
  documents what a consumer does with an unrecognised case.

Adding an ordinary named type — a string-backed vocabulary, a struct nothing
type-switches over — is unconditionally additive. The condition applies to
implementations of open marker interfaces, which are the ones consumers discover
by type assertion.

**Branch (b) does not apply to `imap.SearchCriteria`, `imap.FetchItem`, or
STATUS items.** Each has exhaustive in-repo consumers — every `imapserver`
backend, and for search keys `internal/imapmessage` too — so a new
implementation of one may only be justified under (a). `SearchCriteria` is the
one the framework narrows: no `SearchFilter` and no `SearchSeqNum` reaches a
backend, at any nesting depth, on any command. `FetchItem` gets no such
narrowing, because a fetch item requests data only the backend holds; its
`# Consumers` paragraph therefore carries the whole contract, and an
unrecognised item must be an error rather than a silently omitted field.

This is stated by name because (b) is otherwise self-satisfying: the
`# Consumers` paragraph on `SearchCriteria` already documents the unrecognised
case, so an agent could cite (b), write no framework code and no test, and pass
the rule that was written to stop exactly that.

RFC 5257 (ANNOTATE) is the live test of this: it adds an `ANNOTATION` search key
*and* an `ANNOTATION` fetch item, so it stresses both lists at once.

Where (b) does apply, the documented fallback must have shipped in the release
the consumer surface froze in. A fallback documented after the fact protects
nobody already compiled — which is the population the rule exists for — so
retroactive documentation does not satisfy it.

Branch (a) names a test. Branch (b) cannot, which is a reason to prefer (a)
wherever both are available.

#### How `imap.SearchFilter` satisfies (a)

- The framework substitutes every `SearchFilter` for the criteria it names before
  any backend sees the tree, and refuses an undefined name with
  `UNDEFINED-FILTER` rather than matching nothing.
- The guarantee is stated on `SearchQuery.Criteria`, on
  `MultiSearchSession.MultiSearch` — the two places a backend receives criteria —
  and on `imap.SearchCriteria` itself, where a consumer of the shared vocabulary
  would look first. All three say the same thing, including where the
  `SearchSeqNum` half does *not* hold: MULTISEARCH searches several mailboxes, so
  there is no single selection to resolve sequence numbers against.
- `TestSearchQueryNormalisationGuarantee` drives SEARCH, SORT, THREAD and ESEARCH
  with a FILTER key, at the top level and nested under `FUZZY`, `NOT` and `OR`,
  and asserts the substituted answer. It was verified to fail before each fix,
  not merely to pass after it.
- `TestSearchCriteriaContainersAreTraversed` reads the type declarations in
  `package imap` and fails if a search key that holds other search keys is not
  handled by the framework's single traversal helper.

That last gate exists because the first version of this section was wrong in
practice as well as in theory. Substitution was a hand-maintained list of
container node types that omitted `imap.SearchFuzzy` — a container this library
already shipped, parsed and advertised — so `SEARCH FUZZY FILTER "x"` delivered
an unsubstituted `SearchFilter` to the backend and skipped the FILTERS
capability check with it. A second, separate traversal for UID normalisation did
handle it, and nothing compared the two. **A guarantee maintained by hand in two
places is not a guarantee**; branch (a) is only worth anything when a test reads
the declarations rather than trusting a list.

Also true, and still worth recording: it is a new type rather than a change to
one; it satisfies an existing interface, so no signature moved; it closed a gap
the client's FILTERS work had already escalated; and `internal/imapmessage`'s
criterion-coverage gate was extended to express "this criterion must fail to
evaluate" rather than exempted.

#### The next one

RFC 5257 (ANNOTATE) adds an `ANNOTATION` search key — the identical shape, on the
deferred rows of `docs/RFC-COVERAGE.md`. It will arrive with the same reasoning
available to it, so the conditions above are what it must be held to, not the
precedent that "T23 added one, so this is fine."

A future extension needing a root-package *reshape* remains a different question
and still needs the human's approval.

### Shared client/server vocabulary belongs in `package imap` — decided 2026-08-13

T23 gave `imapserver` its own NOTIFY event and specifier vocabulary while
`imapclient` already had one. The two disagreed: the client canonicalised events
as `MessageNew`, the server upper-cased them to `MESSAGENEW`, and both declared a
`NotifyMailboxSpecifier` with the same seven constants. `imapclient.NotifyMailboxes`
was the MAILBOXES *constant*; `imapserver.NotifyMailboxes` was a watch-group
*struct*.

Nothing failed. Both packages compiled, both test suites were green, and a backend
author comparing an event against the constant from the wrong package would simply
have matched nothing — a NOTIFY registration that silently never fires, which the
client reads as "nothing has changed".

**The vocabulary moved to `package imap`** (`notify.go`) and the server dropped
its copies. This is the layering rule in CLAUDE.md applied rather than restated:
the root package is "the shared vocabulary, which is what lets the future server
framework reuse it without an API break."

The spelling adopted is the client's, because `imapclient` is frozen at v1.0 and
its constant values can never move, while `imapserver` is pre-1.0 and its still
can. Deferring would have meant changing released values later or keeping both
spellings permanently.

#### Values were unified; type identity was not

The obvious move — alias `imapclient.NotifyEventName` to `imap.NotifyEventName` —
was tried and backed out. It is invisible to realistic callers: an alias is the
same type, so assignment, constant comparison, `NotifyFilter` construction and
dynamic type assertion through an interface all behave identically, verified
against a consumer written to the v1.0 surface. But it changes type identity, and
`apidiff` correctly reports sixteen symbols as incompatible.

Overriding the gate was available — the policy allows it with a human decision
and a CHANGELOG entry — and was declined. **The gate's value is that it is not
argued with.** This would have been the first post-v1.0 enforcement, and
establishing at the first opportunity that a sufficiently good argument moves it
would have cost more than the duplication saves.

Instead `imapclient` keeps its own defined types and derives their constant
*values* from the root constants by constant conversion:

```go
const NotifyEventMessageNew NotifyEventName = NotifyEventName(imap.NotifyEventMessageNew)
```

This is a compile-time constant, so the two definitions cannot drift in *value*.
`TestNotifyVocabularyMirrorsRootPackage` covers the other axis — a later RFC
registering an event would otherwise add a constant to `package imap` and leave
`imapclient` silently without one, with a green build. And
`apidiff` reports no change to `imapclient` at all. The divergence that caused the bug is gone; only the redundant type identity
remains.

An earlier draft justified that by claiming a value never crosses from one
package's type to the other's in a single program. That is false, and worth
correcting rather than quietly deleting: a proxy or a migration tool built on
both halves of this module — a shape the nested-module design invites — parses a
client's NOTIFY with `imapserver` and reissues it upstream with `imapclient`,
crossing the two types in one call chain. The real justification is smaller and
holds: both are string-backed, so the crossing costs one conversion and loses
nothing.

Collapsing the identities is an `imapclient` v2 change. It is not urgent, because
the values can no longer diverge.

#### Registry accessors — a precedent, with a limit

`imap.NotifyEventNames()` and `imap.NotifyMailboxSpecifiers()` were added
alongside the vocabulary. They are the first package-level "known values"
accessors in the root package, and the first new *functions* added to it after
v1.0, so the precedent needs stating rather than inferring.

They earn their place: three consumers each kept a hand-written copy of the same
list, which is how the two vocabularies drifted apart to begin with. A later RFC
registering an event adds it once.

**The limit is that a registry is not a validity test.** These sets are open by
declaration, so absence from the list means "not known to this release", never
"invalid". A consumer that rejects a name for being absent starts *accepting* it
the day the library is upgraded, before anything implements it — which for NOTIFY
is a watch that never fires and reads to the client as a quiet mailbox. Reject a
name because you cannot serve it, which is a statement about your own code.
`imapserver/memory` shows the split: it consults the registry to decide whether a
specifier is in the grammar at all (a syntax error if not) and its own list to
decide whether it serves it (`NO [BADEVENT]` if not).

An accessor is therefore justified only where a *shared spelling* is the thing
being protected. It is not a licence for `imap.FetchItems()` or
`imap.CapabilityNames()`: those sets are open precisely so that callers may name
what this library does not model, and a list of them invites the membership test
that rule 1 exists to prevent.

The general rule: **a string-backed name set that both the client and the server
must spell identically goes in `package imap` from the start**, not in whichever
package implements it first. Retrofitting the values is cheap; retrofitting the
identity is not.

## Reviewing against this document

The `api-guardian` agent (`.claude/agents/api-guardian.md`) reviews every diff
that touches an exported symbol. Its single question is the one from CLAUDE.md:
*can the next RFC be added without breaking this?* It has authority to reject a
functionally correct change.

### BINARY fetch is an independent backend witness — added 2026-10-19

`CapabilitySupport` recognises the backend-only token `BINARY-FETCH`. It is
never advertised on the wire and is separate from full `BINARY`, which also
claims literal8 APPEND. IMAP4REV2 requires the fetch-only witness alongside its
other incorporated behaviours. The same session-over-backend precedence applies.
`CapabilityFilter` filters wire capabilities, not this backend-only witness;
withdrawing full BINARY therefore does not withdraw the incorporated fetch half.

`message.Message.OpenBinarySection` and guarded `message.BinarySectionOptions`
provide a shared, context-bound decoder for both reference backends. Exact
length measurement scans the body before opening the returned stream, using
bounded buffers. Partial ranges apply after CTE decoding and text CRLF
conversion; an empty section retains the message headers. Containers are
rejected, unknown or malformed CTEs return UNKNOWN-CTE, and source I/O errors
retain their identity. Source ownership and lifetime match OpenBodySection.
The helper adds no backend interface requirement. (#126)

### STATUS and quota witnesses — added 2026-10-20 (#176–#179)

`STATUS-DELETED` is a backend-only witness required by IMAP4REV2, like
`BINARY-FETCH`: it promises STATUS DELETED for selected and unselected mailboxes
and is never advertised or passed to CapabilityFilter. RFC 9051 Appendix E
items 2–3 are covered by the incorporated witnesses plus framework handling;
SIZE and DELETED each have an explicit backend witness.

STATUS and LIST-STATUS requests are classified before reaching a backend.
Unknown items and items outside the enabled revision's baseline and advertised
capabilities receive BAD, including RECENT in rev2. NOTIFY has no client STATUS-item list; its outgoing
STATUS data is projected onto the same negotiated item set.

Registered `QUOTA=RES-*` resources use the existing spoken CapabilitySupport
mechanism, with one descriptor row per resource. New registrations add rows;
no session interface changes. CapabilityFilter applies to each wire token.
Bare QUOTA remains RFC 2087 for a QuotaSession declaring no resources.
QUOTASET additionally requires at least one unfiltered resource token.
RES-STORAGE promises STATUS DELETED-STORAGE; RES-MESSAGE promises DELETED.
GetMetadataOptions carries only depth: the framework always applies MAXSIZE
and computes LONGENTRIES from unfiltered values returned by the backend.

### RECENT selection ownership and source-route preservation (#160, #165)

`UpdateRecent{NumRecent}` extends the existing closed-to-external-types update
vocabulary without adding a method to any exported interface. Its absolute count
belongs to the receiving selection and follows additions/removals in the same
revision batch. Reference backends persist pending state until a read-write
selection claims it; EXAMINE does not consume it. A backend must tailor FETCH,
STORE and update flags to each selection, and must not store `\Recent` as a
client-controlled permanent flag. The framework suppresses RECENT counts and
flags in rev2. Pending claims in old Maildir records without explicit recent
state are conservatively treated as recent on first read.

`imap.Address.SourceRoute` is an additive field on an already guarded root
struct, preserving the fourth ENVELOPE address component. The root module must
be released before a standalone server release consumes these codec/helper
corrections, following the existing root-before-server release gate. No root
signature or existing backend interface changes.

For `message.Metadata`, nil private and shared per-entry clock maps select
RFC 7162's single-clock model for `/flags/` MODSEQ queries. Non-nil maps preserve
entry-specific behavior, including zero for an absent entry.

### T33 publication and resource contracts — 2026-10-21 (#54, #196)

Before the release window, `UpdateBatch` and `Updater.Push` explicitly require
synchronous publication to every attached updater before a mutation returns,
including commits followed by errors. STATUS/SELECT visibility is fenced behind
publication, including external commits. This is a **BREAKING pre-v1 backend
duty**, previously relied on by the framework but absent from its written
contract. A backend using a late change feed must add a publication fence.
`backendtest.Run` checks the call boundary for APPEND, COPY, STORE, EXPUNGE and MOVE
against both the mutating selection and a bystander. Both reference backends
already serialize publication with commit visibility. `WriteExpunge` reports a
UID; the ordered update batch supplies its later wire response.

`Limits.MaxQueuedUpdateBytes` bounds undrained backlog and permits the budget
plus one atomic commit. `MaxQueuedUpdates` bounds undrained batches. Applied,
protocol-deferred removals are compact UIDs bounded by the selected client view
(`MaxSelectedMessages`), separately from backlog. These are pre-v1 changes to
the bounds' meanings. `Limits.MaxStoreResponseBytes` is an additive guarded
field, defaulting to 32 MiB, charging encoded bytes plus 1 KiB per retained STORE
response. Exhaustion switches to a streaming FLAGS/MODSEQ refetch after mutation
completion, preserving OK and MODIFIED. A failed post-commit stream sends BYE.

FETCH item keys are case-insensitive in both framework and conformance suite;
no canonical-spelling duty is imposed. RENAME INBOX requires strict removal-clock
advancement only with QRESYNC, retaining CONDSTORE's monotonic-clock contract.
UNAUTHENTICATE errors send BYE and close, as RFC 8437 requires; success removes
COMPRESS after the OK's CRLF and gives Session.Close a fresh cleanup deadline.
URLAUTH returns nil/nil for an unresolved URL; errors fail URLFETCH through its
ordinary error mapping after completing the untagged response framing.

The publication fence is also stated directly on `Session.Status` and
`Session.Select`. Conformance exercises concurrent STATUS/SELECT reads while
publication is held open, including writes without a framework origin. Truly
external delivery and durable-commit error paths still need backend-specific
tests; the generic suite does not manufacture storage events or failures.

SORT keys join the explicit capability classification: unknown keys and repeated
REVERSE are BAD, DISPLAYFROM/DISPLAYTO require SORT=DISPLAY, and RELEVANCY requires
SEARCH=FUZZY and a FUZZY criterion after filter expansion. This tightening is
marked BREAKING behaviour before the window. FILTERS and URL-PARTIAL withdrawals
are enforced before backend calls. Duplicate case variants cannot bypass
FetchWriter's existing exactly-one-UID rule.

The URL-PARTIAL gate includes CATENATE: range URLs receive NO [BADURL] before
resolution when the token is absent or filtered out. Later parts are drained
without further resolution or a commit. This tightening is marked BREAKING
behaviour before the release window. URLFETCH treats an unavailable range as
an individually invalid URL (NIL), preserving other URLs and tagged OK.

The compatible root client correction ends COMPRESS at successful UNAUTHENTICATE
completion, preserving TLS and prefetched plaintext. The shared internal boundary
reader accepts sync flush, final block, and sync flush followed by a final block.
Ship the root correction before the server's compression teardown correction.

### T33 fixed sweep corrections — 2026-10-21 (#54)

Before the stability window, three **BREAKING behaviour** changes close S-02,
S-03 and S-06: UIDONLY prohibits all sequence-number command forms, sequence-set
search criteria at any depth and SELECT QRESYNC sequence-match data, returning
BAD [UIDREQUIRED]; the framework rejects 8-bit headers in staged APPEND-family
messages until UTF8=ACCEPT or IMAP4rev2 is enabled; and the obsolete UTF8=APPEND
token and memory witness are removed. RFC 9755 does not extend IMAP4rev2, which
already incorporates UTF-8 headers, so ENABLE IMAP4rev2 also permits them.
CATENATE TEXT's grammar is an ordinary literal (RFC 4469); refusing the previously
accepted literal8 TEXT syntax is also recorded as BREAKING behaviour. The BINARY
witness permits whole-message literal8 APPEND, MULTIAPPEND and REPLACE.

Compatible corrections share APPEND staging with REPLACE, prevent mutation
before its command terminator, send BYE on its read timeouts, and repair
REPLACE/LIST/LSUB framing. UID FETCH PARTIAL windows resolved, existing UIDs
before CHANGEDSINCE (RFC 9394 section 3.4). Oversized LITERAL- APPEND-family
literals return BAD [TOOBIG]. NO on a supported SAVE search empties the saved
result; BAD preserves it. SORT's unsupported RETURN (SAVE) still receives BAD.

Memory's CATENATE URL resolver now honors relative URLs, sections, UIDVALIDITY
and byte ranges under the existing CatenateSession contract (#199). The
backendtest relative-section and BINARY APPEND checks enforce existing witness
obligations, adding no backend duty. BINARY mailboxes that cannot store binary
content may return NO [UNKNOWN-CTE], as RFC 3516 allows. No exported API changes.

A refused CATENATE URL produces NO [BADURL <url>]. BADURL must echo the URL, and
url-resp-text cannot carry "]", CR, LF or NUL (RFC 4469 section 5), so a URL
containing one of them is refused with a plain NO. The response code is optional
(section 3). The refusal is the client's, not the backend's, so it is not
reported to OnError (#204, follow-up S-15). The response encoder's SERVERBUG
substitution remains for backend errors whose code cannot be written.

### T33 QRESYNC and UIDONLY follow-up — 2026-10-21 (#54)

**BREAKING pre-v1 contract:** `QResyncMailbox.Resync` is no longer limited to
one call for each QRESYNC SELECT/EXAMINE. The framework also calls it for each
UID FETCH with CHANGEDSINCE and VANISHED for nonempty requested UID sets,
permitting repeated history requests within one selection. `QResyncSelect`
carries the selection's UIDVALIDITY,
CHANGEDSINCE as ModSeq and the full requested UID set as KnownUIDs, with no
sequence-match data. Only Vanished is used; the ordinary Fetch supplies changed
messages. This expands the documented backend call contract without changing
any exported method signature or adding an interface requirement.

RFC 7162 section 3.2.6 requires VANISHED (EARLIER) before FETCH responses.
Requested UIDs include messages no longer present; PARTIAL windows only the
surviving FETCH results. Empty saved results never become unrestricted history
requests. Invalid command forms, disabled/withdrawn QRESYNC and NOMODSEQ are
refused before Resync. A failed UID FETCH retains its selection; the existing
selection-release rule applies only to failed SELECT/EXAMINE resynchronisation.
The QRESYNC-gated backendtest history check covers repeatable retained removals
and KnownUIDs restriction, under this explicit pre-v1 contract correction.

**BREAKING behaviour (pre-v1):** VANISHED now receives BAD outside UID FETCH
or without advertised, enabled QRESYNC. Previously these forms were accepted
and the modifier was ignored.

**BREAKING behaviour (pre-v1):** UIDONLY SELECT/EXAMINE suppress the rev1
UNSEEN response code because its value is a sequence number. STORE's UIDONLY
check now precedes read-only refusal, preserving BAD [UIDREQUIRED] on EXAMINE;
UID STORE still receives NO [READ-ONLY]. The latter correction rejects no new
input. These changes close G-1–G-3 before the stability window.

### T33 H follow-up corrections and compatibility decisions — 2026-10-21 (#54)

The compatible H-1 correction retains UID FETCH VANISHED wildcard ranges on an
empty selection. UIDNEXT has not been assigned; using the UID maximum as that
endpoint selects the same historical removals, while a lone `*` matches none.
The existing Resync contract already accepts these static UID ranges. The root
codec's H-2 correction maps `UID $` to the existing saved-result criterion at
any search depth. The SEARCHRES gate and UID normalization remain unchanged.
Neither correction changes exported declarations or backend obligations; the
root codec correction must ship before the server release.

Preserve and document the accepted SEARCH return-option combinations in #206:
repeated ALL is treated once; PARTIAL together with ALL reports both the full
result and its requested window. Preserve memory's CATENATE authority aliases
in #207: absolute hostnames and ports identify the same authenticated account.
Absolute URLs without userinfo inherit that account; mechanism-only userinfo is
refused. Other-user and URLAUTH-only URLs remain refused.
No stricter parsing or authority rejection is introduced. A future opt-in policy
may restrict these forms while retaining existing default acceptance.

The comparability rule in section 7 applies to server types too. SERVER-DESIGN
section 8 now distinguishes implemented Limits from application/backend budgets
and outstanding design goals; it does not make every listed budget an implicit
Server guarantee. These documentation corrections alter no limit or default.

### T33 pre-window conformance corrections — 2026-10-21 (#204, #205)

imaptest's scripted corpus now runs against memory (#205), and the corrections
it prompted land before the stability window opens.

**Additive:** `message.Message.EncodeNULParts` and `message.EncodeOptions`.
The method follows the package's shape: a context, a caller-owned writer and
a guarded options struct for future policy. It is a method on a concrete
type, not an interface. It re-encodes NUL-bearing parts as base64 for backends
that accept BINARY APPEND (RFC 3516 section 4.4). Memory uses it; no backend
is required to.

**BREAKING pre-v1 contract:** `message.Message.OpenBinarySection` accepts
multipart and encapsulated message sections and returns their stored octets.
RFC 3516 allows these sections; RFC 9051 lets an IMAP4rev2 client request only
leaf parts, and serving them anyway is the lenient choice. Base64 decoding
ignores characters outside the alphabet (RFC 2045 section 6.8), so malformed
base64 no longer returns CodeUnknownCTE. Unknown transfer encodings and
quoted-printable decoding failures still return that code.

**Backend-observable call:** when APPEND or REPLACE is refused while staging,
for example over a CATENATE URL, the framework calls `Session.Status` with no
items for the destination. A NONEXISTENT or TRYCREATE answer turns the
refusal into NO [TRYCREATE], as RFC 3501 section 6.3.11 requires; any other
answer keeps the original response. Status already answers for any mailbox,
and with no items requested it need populate nothing, so this adds no backend
obligation.

**Framework wire corrections:** THREAD responses follow the RFC 5256 section
5 grammar. SORT accepts the PARTIAL return option under CONTEXT=SORT, as
SEARCH accepts it (#206). ESORT sets write ascending runs as ranges, which
RFC 5267 section 3.2 permits.

**Memory behaviour (reference backend):** Subscriptions belong to names and
survive DELETE and RENAME. LIST returns missing hierarchy levels as
`\NonExistent \HasChildren`. SORT and THREAD keys follow RFC 5256 and RFC
5957, using the session's active comparator (i;unicode-casemap by default).
CATENATE escapes the login
inherited into URL userinfo. A BINARY APPEND is stored with NUL-bearing parts
re-encoded.

**Root, for the next root release:** `internal/imapmessage` keeps
domainless addresses in the ENVELOPE with host `MISSING_DOMAIN`, the
placeholder Dovecot uses, because a NIL host would mean group syntax.

**Left open:** a BODY section that a backend already stores with NUL still
closes the connection (#210).

`message.MatchOptions.Comparator` is an additive field on a guarded options
struct. Empty selects i;unicode-casemap; i;octet selects exact octets. Unknown
names return NO [BADCOMPARATOR]. Memory passes its active comparator to SEARCH,
SORT/THREAD criteria and MULTISEARCH; textual sort keys and ORDEREDSUBJECT use
the same RFC 5051 preparation. Text that cannot be converted (invalid UTF-8,
unknown stored charsets, malformed transfer encodings) compares as i;octet
under a named comparator and sorts after valid keys (RFC 5255 section 4.6).
An empty comparator keeps RFC 3501's ASCII case-insensitive matching for that
text, which is what a backend without I18NLEVEL, such as Maildir, gets. No
backend interface or obligation is added.

SORT UPDATE now registers its complete ordered result, including outside a
PARTIAL window. Refreshes use the existing SortMailbox operation, and explicit
positions preserve the requested order across removals and additions. Context
failure sends untagged NO [NOUPDATE "tag"]. Reusing an active UPDATE tag is
refused before backend access across SEARCH, SORT and MULTISEARCH; this is a
BREAKING pre-v1 behavior correction. Cancellation or selection teardown frees
the tag.

**Backend-observable deadline:** context refresh calls (`Search`, `Sort`) and
capability predicates (`SupportsCapability`, `AllowCapability`) receive a
context bounded by `Limits.CommandTimeout`, as that field promises; they
previously inherited the connection's lifetime. A drain's refresh pass shares
one timeout, and a context whose refresh misses it ends with NOUPDATE. A
sorted context's refresh can call `Sort` a second time, with a UID-set query
over the entries it keeps, to place them; that call shares the same timeout
and adds no obligation beyond answering UID criteria.
