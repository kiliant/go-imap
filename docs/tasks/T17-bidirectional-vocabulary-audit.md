# T17 — Bidirectional vocabulary audit

**Agent:** `api-guardian` (lead) + `client-core` · **Milestone:** M4 —
**this task blocks v1.0** · **Depends on:** T16 approved

**Owns:** `*.go` in the root package, for changes this audit finds necessary.
Ownership passes from T02, which is complete.

## The question this task asks

For every exported type in `package imap`:

> The client **consumes** this type — it decodes it off the wire. Can a server
> **produce** it as readily, and encode it back onto the wire, without the type
> changing shape?

Nothing else. It is not a general API review — T14 did that, and did it against
the client. This one is specifically about the direction that has never been
exercised.

## Why it gates v1.0

Adding a type to `package imap` after the freeze is additive and always allowed.
**Reshaping an existing one is not**, and after v1.0 the only remedy is a second
type alongside the first, carried forever.

The failure mode is narrow and easy to miss: a type that is *usable* from the
server but not *natural* — one that loses information the server has, or that
cannot represent a state a server must express, or whose zero value is meaningful
only when decoding. Every such type is fine today, because only the client uses
it. Each one becomes permanent at the tag.

`docs/SERVER-DESIGN.md` §0 establishes that this is not hypothetical: the entire
semantic codec — envelope, body structure, fetch data, search criteria — exists
in exactly one direction today.

## Known findings — start here, they are already confirmed

The first revision of the server design assumed this audit would probably find
nothing. That assumption was wrong, and checking it cost one afternoon. These
three are established; the audit's job is to resolve them and then look for more.

### 1. ~35 response data types are in the wrong package

`StatusData`, `ListData`, `AppendData`, `CopyData`, `MailboxStatus`,
`NamespaceData`, `QuotaData`, `ACLData`, `ESearchData`, `VanishedData` and about
25 more all live in `imapclient`, not in `package imap`.

A server backend interface cannot name any of them without `imapserver`
importing `imapclient`, which inverts the dependency graph the whole layering
exists to protect.

**The move is not a breaking change if done correctly.** Leaving an alias behind
preserves type identity, so every caller and every keyed struct literal keeps
compiling:

```go
// in imapclient, after the move
type StatusData = imap.StatusData
```

This is the technique the standard library used to relocate `context.Context`.
**Verify it rather than trusting it**: run `apidiff` across the move and confirm
it reports compatible. If it does not, stop and reconsider — do not push through.

Not every type moves. The test is whether a *server* needs to name it. Purely
client-side response plumbing stays where it is.

### 2. `AppendData` and `CopyData` cannot be constructed from outside their package

Both carry unexported fields (a "response code was received" flag). A type with
unexported fields can only be fully constructed by its own package — so even
after moving to `package imap`, a server in `imapserver` still cannot produce a
complete value.

**Verdict: make presence an exported field, not a constructor.** A constructor
that preserves hidden state leaves the type asymmetric — readable by anyone,
writable only through a blessed path — and a pure vocabulary type should have its
full state visible and constructible from *both* protocol directions. Replace the
unexported flag and the `Received()` accessor with an explicit exported field.

This is a reshaping change and therefore **must land before the tag**.

### 3. `MailboxStatus.UIDValidityChanged` is client-only

It is a client-side derived observation — "this differs from what you last saw".
No server produces it.

**Verdict: it stays client-side.** Do not move it and document it as
server-always-false. A field one of the two users can never meaningfully produce
is evidence that the shared type has the wrong semantic boundary, and while we
are still pre-v1 the asymmetry can be removed instead of documented forever.
`imap.MailboxStatus` carries the mailbox state both directions can express;
whatever the client derives by comparing against its own cache belongs to the
client.

## Method

Work from the type list, not from intuition. `go doc github.com/kiliant/go-imap`
is the checklist; every exported symbol gets a verdict.

For each, write down the answer to three questions:

1. **Production.** Can a server construct a valid value from stored message
   bytes and backend state? Concretely: `imap.Envelope` and `imap.BodyStructure`
   must be constructible by the T21 generator, not merely readable.
2. **Round-trip.** Does decode(encode(x)) == x for every value the server can
   produce? Where it does not, is the loss acceptable and documented?
3. **Expressiveness.** Is there a legal server-side state this type cannot
   express? `imap.FetchData` is the one to look hardest at — the client models
   what it receives, and a server must emit items in response to requests the
   client would never make of itself.

Highest-risk areas, in order — spend the time here:

| Type | The specific worry |
|---|---|
| `BodyStructure` and its six concrete types | Extension fields are optional on the wire and the client can leave them nil; a server must decide when to emit them. Is "nil" distinguishable from "absent"? |
| `Envelope`, `Address` | Group syntax, and malformed real-world headers a server must reproduce rather than reject |
| `FetchData` / `FetchDataKey` | Modelled as what the client received. A server emits in response to a request — is the mapping from `FetchItem` to `FetchData` total? |
| `SearchCriteria` tree | Only ever encoded. Decoding it produces a tree that must be *evaluable*; check nothing is representable-but-meaningless |
| `SectionPartial`, `PartSpecifier` | Byte-exact extraction semantics, and what a server returns for an out-of-range partial |
| `Error` / `ResponseCode` | `Tag` and `Type` are client-shaped (a server sends these rather than receiving them). May be fine; say so explicitly |
| `NumSet` / `SeqSet` / `UIDSet` | `*` handling — the client sends it, the server must resolve it against a mailbox |

## Deliverable

`.state/progress/T17.md` carrying a per-type verdict table, then one of:

- **No change needed** — recorded per type, with the reasoning, so the next
  reviewer does not redo it.
- **Additive change** — new sibling type or constant. Land it or defer it; either
  is safe, and deferring is the default because it stays additive forever.
- **Reshaping change** — **must land before v1.0.** This is the entire point of
  the task. Route through `api-guardian` and record it in `CHANGELOG.md` as a
  pre-v1.0 break.

"No changes needed" is a legitimate verdict for an individual type, and it is
only worth something written down per type rather than asserted. It is **not**
the likely outcome for the audit as a whole — the three known findings above
already disprove that, and they were found by a single afternoon's inspection
rather than by a systematic pass. Budget accordingly.

## Do not

- Change `imapclient`. If a client-side change looks necessary, this audit has
  drifted out of scope; record it and stop.
- Add server *machinery* to `package imap`. The rule that it performs no I/O and
  imports nothing from the module is not relaxed by this task. Vocabulary only.
- Treat an awkward-but-workable type as a break. The bar is "cannot express" or
  "loses information", not "would have been designed differently".

## Done when

Every exported symbol in the root package has a written verdict, every
reshaping change is landed, and `api-guardian` signs off in writing that
`package imap` is safe to freeze **in both directions**.
