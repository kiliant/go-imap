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

- **`CapabilitySupport`** — `SupportsCapability(name string) bool`, keyed by the
  wire token. Use it where support is spread across data the backend returns and
  no type can see it: CHILDREN, SAVEDATE, WITHIN, CONDSTORE. A future RFC is then
  a new *token*, which is a data change rather than a type change — rule 1
  applied to the witness layer.
- **A structural check** that the session or selected mailbox implements the
  optional interface. Use it where the interface *is* the whole of the support:
  QUOTA, ACL, METADATA. The type system then makes it impossible to advertise
  what is not implemented.

Every extension command handler calls `requireCapability` before doing any work,
whichever witness its capability uses. Holding an optional interface is not
consent to advertise it.

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

### Exception: `MoveSupport` predates `CapabilitySupport` — recorded 2026-08-13

`MoveSupport` (`SupportsMove() bool`) is a single-capability witness for atomic
MOVE, added by T19 before `CapabilitySupport` existed. It is redundant:
`CapabilitySupport("MOVE")` expresses the same thing.

It is kept rather than collapsed because MOVE's witness is also consulted for the
IMAP4rev2 gate, so removing it is a behavioural change to rev2 advertisement
rather than a rename — and `imapserver` is pre-1.0 but T19/T20 shipped against
this surface. **Collapsing it is the right move and the window is open only until
`imapserver` v1.0**; after that the second witness is permanent. Recorded here so
the decision is deliberate, per CLAUDE.md's requirement that a deviation carry a
written exception. T25 should either collapse it or promote this paragraph to a
permanent entry.

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
