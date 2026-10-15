# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Two conventions are specific to this project, and both exist to serve the goal
in `CLAUDE.md` — reaching a v1.0 that does not have to break for the next RFC:

- **Every entry that touches an exported symbol says so explicitly**, naming the
  symbol. That is what makes the `apidiff` job's output reviewable rather than
  noise: each line it prints should be traceable to a line here.
- **Pre-v1.0 breaking changes are allowed but never silent.** They appear under
  *Changed* or *Removed* marked `BREAKING`, with the reason. After v1.0 the
  policy hardens: additive only, and a removal needs two minor releases of
  deprecation and does not land before a major.
- **Entries name their module.** Since T25 the repository holds two modules with
  independent version lines: the root module at `v1.x` and `imapserver` at
  `v0.x`, tagged `imapserver/v0.a.b`. They make different promises, so an entry
  that does not say which module it belongs to does not say whether it was
  allowed. See `docs/RELEASING.md`.

## [Unreleased]

### Root module — `github.com/kiliant/go-imap`

#### Added

- `imapclient.UnilateralDataHandler.ContextUpdate`, the shared types
  `imap.ContextUpdate`, `imap.ContextUpdateItem` and
  `imap.ContextUpdateChange`, the alias `imapclient.ContextUpdate`, the return
  keys `imap.ESearchReturnKeyAddTo` and `imap.ESearchReturnKeyRemoveFrom`, and
  `imapclient.ESearchData.ContextTag`: CONTEXT updates (RFC 5267 section 4.3)
  are delivered. The unsolicited ESEARCH responses carrying ADDTO and
  REMOVEFROM were discarded, or claimed by another pending extended SEARCH or
  SORT, which then failed. They are now routed by the original command's tag
  before any command sees them. Each update keeps every return item in wire
  order, including repeated and unknown keys. Items carry their raw value,
  and ADDTO and REMOVEFROM also carry typed position/set pairs in UID or
  sequence-number mode. `ContextTag` names the context for
  `Client.CancelUpdate`, also from the blocking `SortExtended`. CancelUpdate
  now documents when a registration ends. Additive. (#44)

#### Changed

- `imapclient.Client.SortExtended` and `SortExtendedUID` accept RETURN
  (UPDATE) when the server advertises CONTEXT=SORT and refuse it with
  `ErrCapabilityNotAdvertised` otherwise. Before, they always refused it,
  because updates were not delivered. `SearchExtended` and `MultiSearch` now
  refuse RETURN (UPDATE) without CONTEXT=SEARCH instead of sending it. Without
  ESEARCH, `SearchExtended` used to emulate the search and drop UPDATE
  silently. A MULTISEARCH with UPDATE requires a selected mailbox (RFC 7377
  section 2.2). CONTEXT=SEARCH and CONTEXT=SORT also select the extended
  SEARCH and SORT syntax that RFC 5267 section 4.1 says they imply. No
  signature change. (#44)

#### Fixed

- Decode STATUS, LIST and ESEARCH/METADATA response data once before command
  collection. STATUS results stay with their requested mailbox, including
  reversed pipelined replies; unrelated STATUS events reach the existing
  callback. Legacy LSUB no longer consumes unsolicited LIST events. Names-only
  METADATA notifications no longer fail GETMETADATA or LIST-METADATA, and
  another mailbox's metadata values do not leak into GETMETADATA results.
  No exported API change; metadata-change callbacks remain future T30 work.
  (#44)

### Server module — `github.com/kiliant/go-imap/imapserver`

#### Added

- `backendtest.Run` checks RECURSIVEMATCH combined with REMOTE and
  SPECIAL-USE, including CHILDINFO from a subscribed descendant without a
  special-use attribute. Remote descendants are checked when the harness
  seeds a remote child beneath an unsubscribed parent; otherwise that case
  explicitly skips. Backends without the LIST-EXTENDED witness are checked
  for discovery of the harness names through `LIST "" "*"`, which the
  framework's evaluation needs. No exported API change. (#34)

## [1.2.1] - 2026-10-15

### Root module — `github.com/kiliant/go-imap`

#### Fixed

- `imapcli -mailbox M -uid N` prints the message's summary line again on a
  server that sends UID, FLAGS and ENVELOPE after `BODY[]`, as
  `imapserver/memory` does. The line showed UID 0 and nothing else, because
  it was read from the body's response before the client had added the
  items that follow the body. Reading them there was also a data race. The
  summary now comes from its own command, before the body is fetched. A
  `main` package: no exported API change. (#36)
- `imapcli -mailbox M` labels the UNSEEN response code `First unseen: seq N`
  and leaves it out when the server reports none. It printed `Unseen: N`,
  which read as a count of unseen messages but is the sequence number of the
  first one. A `main` package: no exported API change. (#37)

#### Documentation

- The `imapcli` manual warns that backup mirrors need persistent storage,
  not `/tmp`, macOS `$TMPDIR`, or another automatically cleaned location.
  No timestamp or backup behaviour change. (#41)

## [imapserver/v0.3.0] - 2026-10-15

### Server module — `github.com/kiliant/go-imap/imapserver`

Requires root `v1.2.0`, for `imap.ListData.ChildInfo`; `imapserver/go.mod` was
bumped from `v1.1.0` for this release. No exported symbol changed: `apidiff`
against `imapserver/v0.2.0` reports none. One behaviour changed deliberately,
see *Changed*. RECURSIVEMATCH now reaches only a backend that witnesses
`"LIST-EXTENDED"`. A backend that implemented the option against v0.2.0 adds
that witness to keep receiving it, and the framework answers for it in the
meantime.

#### Added

- LIST writes the CHILDINFO extended data item (RFC 5258 section 3.5) from
  `imap.ListData.ChildInfo`. It is written only when the command used
  RECURSIVEMATCH, and only for base selection options the command requested.
  So LSUB, a plain LIST and the LIST answering SELECT never carry it.
  `ListWriter.WriteList` documents this, and that `ListData.Extended` is
  never written. `ListOptions.SelectRecursiveMatch` now documents what a
  backend must return. No exported API change.
- `imapserver/memory` implements `LIST (SUBSCRIBED RECURSIVEMATCH)`. It
  returns a parent with a subscribed descendant and its CHILDINFO, and
  returns a hierarchy level that is not a mailbox as `\NonExistent`. Before,
  it ignored the option and returned no such parent. It witnesses
  `"LIST-EXTENDED"` to say it evaluates the option itself.

#### Changed

- RECURSIVEMATCH reaches a backend only if it witnesses `"LIST-EXTENDED"`
  through `CapabilitySupport`. Any other backend never receives
  `ListOptions.SelectRecursiveMatch`, and the framework evaluates the option
  for it from `Session.List` calls without it. That evaluation starts from the
  backend's own answer to the command without RECURSIVEMATCH, so it never
  returns less than that. It adds the parents of subscribed descendants found
  through `List` for `"*"`. Only those added parents depend on the framework's
  reading of the reference (reference followed by pattern, RFC 3501 section
  6.3.8, matched case-sensitively, INBOX included), and a name the backend
  leaves out of `"*"` gets none. Before, every backend received the option,
  and one that ignored it answered wrongly.
  Refusing the option was not possible: LIST-EXTENDED enables it (RFC 5258
  section 3), and IMAP4rev2's LIST incorporates it. The token routes the
  evaluation and does not gate the advertisement, which the
  `CapabilitySupport` documentation now describes. No exported API change.
  **BREAKING in behaviour** for a backend written against v0.2.0 that
  implemented RECURSIVEMATCH: it stops receiving the option until it
  witnesses `"LIST-EXTENDED"`, and is answered by the framework's evaluation
  meanwhile.
- `backendtest` checks RECURSIVEMATCH only for a backend that witnesses
  `"LIST-EXTENDED"`, so one that claims the witness and ignores the option
  fails. It skips the case for any other backend.

#### Fixed

- `LIST (REMOTE RECURSIVEMATCH)` and `LIST (SPECIAL-USE RECURSIVEMATCH)` are
  answered BAD, as RFC 5258 section 3 requires. RECURSIVEMATCH modifies a
  base selection option. REMOTE is an independent option (RFC 5258 section
  6), and so is SPECIAL-USE (RFC 6154's formal syntax), so neither qualifies.
  SUBSCRIBED is the only base option the framework implements.
- `LIST (SPECIAL-USE)` returns only mailboxes that have a use attribute, as
  RFC 6154 section 2 requires. Before, it returned every matching mailbox,
  and the selection option only made the use attributes visible. The
  framework now drops results without a use attribute before writing them, so
  every backend is correct on the wire. `imapserver/memory` also honours
  `ListOptions.SelectSpecialUse` itself. The field's documentation says the
  framework enforces it, and both `SelectSpecialUse` and `ReturnSpecialUse`
  now cite section 2 instead of the examples in section 5. No exported API
  change.

## [1.2.0] - 2026-10-15

### Root module — `github.com/kiliant/go-imap`

#### Added

- `cmd/imapcli`, a read-only command-line client that lists folders, lists
  messages and prints one message by UID, using EXAMINE and `BODY.PEEK[]` so
  it never changes server state. A `main` package: no exported API change.
- `imapcli -backup DIR`: an incremental, read-only mirror of every mailbox
  into Maildir, for a file-level backup tool such as restic. Messages are
  stored byte for byte with INTERNALDATE as mtime. Flag changes are renames,
  and expunged messages and deleted mailboxes are removed. Writes go through
  `tmp/` and a rename, so an interrupted run never leaves a partial message.
  Also new: `-pass-file` and `-pass-cmd` for unattended runs, `-exclude`, a
  per-directory lock and account marker, and COMPRESS when offered. imapcli
  now authenticates only with password mechanisms. Before, a server offering
  OAUTHBEARER beside PLAIN (Stalwart does by default) got the password as a
  bearer token and refused it. A `main` package: no exported API change.
- `imapclient.Client.SortExtended`, `imapclient.Client.SortExtendedUID` and
  `imapclient.ESortOptions`: SORT with a RETURN list, answered as ESEARCH data.
  ESORT, RFC 5267. The client previously detected ESORT but had no way to send
  it. Falls back to a plain SORT for MIN, MAX, ALL and COUNT. Additive;
  `apidiff` reports three compatible additions.
- `imapclient.UnilateralDataHandler.StatusResponse`, the shared
  `imap.StatusResponse` type and its alias `imapclient.StatusResponse`:
  untagged OK, NO and BAD responses no command claims now reach the caller
  with their response code. They were dropped, which lost INPROGRESS
  notifications (RFC 9585) and connection-level ALERT text (RFC 3501 section
  7.1). The type lives in the root package like every other response shape,
  because a server sends the same thing. Additive in both packages.
- `imapclient.UnilateralDataHandler.Status` and `.List`: unsolicited
  STATUS and LIST responses, which NOTIFY uses for events in mailboxes other
  than the selected one (RFC 5465 section 5), were discarded and are now
  delivered. Additive. A malformed unsolicited STATUS or LIST is now a
  protocol error, as for every other unilateral response the client parses.
  SELECT and EXAMINE claim the LIST an IMAP4rev2 server sends for the
  selected mailbox (RFC 9051 section 6.3.2), so it is not mistaken for an
  event. They claim it only once IMAP4rev2 is enabled. On IMAP4rev1, where
  no server sends one, every LIST during SELECT is an event, such as a NOTIFY
  SubscriptionChange for the selected mailbox, and is delivered.
- `imap.ListData.OldName`: the previous name from LIST's OLDNAME extended data
  item, which NOTIFY uses to report a rename (RFC 5465 section 5.4) and an
  IMAP4rev2 server uses when SELECT named a mailbox in non-canonical form
  (RFC 9051 section 6.3.2). The client's LIST parser fills it. Extended data
  it cannot parse costs the extended data, never the response. Additive.
- `imap.ListData.ChildInfo`, `imap.ListData.Extended`, the type
  `imap.ListExtendedItem` and its alias `imapclient.ListExtendedItem`: the
  client keeps LIST's CHILDINFO extended data item, which it used to drop.
  CHILDINFO is the reason RECURSIVEMATCH exists (RFC 5258 section 3.5): it
  marks a parent returned because a descendant matched. So
  `imapclient.ListSelectRecursiveMatch` was unusable until now. Every extended
  data item, OLDNAME and CHILDINFO included, is kept raw in `Extended` in wire
  order. `OldName` and `ChildInfo` are decoded from those items, so an item a
  later release learns to decode stays in `Extended` too. A malformed OLDNAME
  or CHILDINFO is kept raw and decodes to nothing, without costing the other
  items. Of repeated ones, the first that decodes is used. Verified against
  Dovecot, Stalwart and Cyrus. Additive.
- `imapclient.Client.SortExtended` refuses RETURN (UPDATE), because the
  unsolicited ESEARCH updates it asks for are not delivered yet. Allowing it
  later is compatible.
- `imapclient.AuthenticateOptions.CredentialKind`, the type
  `imapclient.CredentialKind` and its constants `CredentialPassword` and
  `CredentialBearerToken`: they say what `Authenticate`'s password argument
  holds, and restrict automatic SASL selection to the mechanisms that take it.
  With `CredentialPassword`, a server offering OAUTHBEARER or XOAUTH2 beside
  PLAIN, as Stalwart does by default, gets SCRAM, CRAM-MD5, PLAIN or LOGIN
  instead of the password as a bearer token. Settings that contradict the
  kind, and kinds this release does not know, are refused before anything is
  sent. Unset, selection is unchanged: the default stays as documented in v1,
  and changing it is left for v2. Additive.

#### Fixed

- A FETCH that streams more than one body no longer has a data race. Reading
  a body literal to EOF a second time, which `io.Copy` always does, or closing
  it after it was read, touched decoder state. The reader goroutine was
  already using that state for the next response. The streamed body now
  leaves the decoder alone once its payload is consumed, empty bodies
  included. Internal (`internal/imapwire`), no exported API change. Found by
  `imapcli -backup` under `-race`.
- A FETCH response whose body section is NIL or a zero-length literal is
  delivered once it is complete. Before, it was handed over at the body while
  the items after it were still being added to the `Items` map the caller was
  reading. That was a data race, and could end in a fatal concurrent map
  access. No exported API change.
- `imapclient.Client.Close` no longer deadlocks on an idle connection after
  `Client.Compress`. The DEFLATE wrapper took its read lock before closing the
  socket, while the reader goroutine held that lock blocked in a read only the
  socket's close could end. Found by the first live COMPRESS test, against
  Cyrus 3.10.
- `imapclient.Client.GetQuotaRoot` and `GetQuota` accept a QUOTA response with
  no resource list, as Courier-IMAP sends for a root without limits, and report
  the root with no resources instead of failing the command with a protocol
  error. `GetQuota` likewise reports the requested root with no resources when
  the server completes without any QUOTA response, as Apache James 3.8 does.
- `imapclient.Client.Authenticate`'s automatic selection skips
  SCRAM-SHA-256-PLUS and SCRAM-SHA-1-PLUS when the connection has no TLS
  channel binding to offer: over cleartext, or under TLS 1.2 without Extended
  Master Secret. Before, it chose them first and failed before sending
  anything, so a cleartext connection to a server that also offers plain SCRAM
  could not authenticate at all. The plain SCRAM chosen instead sends the GS2
  flag "n", which RFC 5802 does not treat as a downgrade. An explicit -PLUS
  `Mechanism`, and a server offering only -PLUS variants, still fail as
  before. No exported API change.

## [imapserver/v0.2.0] - 2026-10-03

### Server module — `github.com/kiliant/go-imap/imapserver`

Still requires root `v1.1.0`: the root module has no code change since that tag,
so no root release precedes this one. Two deliberate breaks, both in the
capability-witness surface and both reported by `apidiff` against
`imapserver/v0.1.0`; a backend migrates with the signature change below and by
answering `"MOVE"` from `SupportsCapability` where it implemented `MoveSupport`.

#### Added

- `imapserver.UpdateMailboxFlags`, allowing a backend to publish a changed
  mailbox-wide applicable flag set before a new keyword appears in FETCH.
- `imapserver.CapabilitySupportOptions`, the guarded extension point for
  capability-witness queries.

#### Changed

- **BREAKING:** `imapserver.CapabilitySupport.SupportsCapability` now takes a
  `context.Context` and `*imapserver.CapabilitySupportOptions`. Backends should
  migrate `SupportsCapability(name string) bool` to
  `SupportsCapability(ctx context.Context, name string, options
  *imapserver.CapabilitySupportOptions) bool`; nil options select the defaults.
  This changes `(*memory.Backend).SupportsCapability` to the same signature.
- The memory backend now keeps a persistent mailbox registry of applicable
  keywords and re-announces untagged `FLAGS` when STORE creates one, before any
  FETCH response first reports it.

#### Fixed

- STORE-created keywords are announced in an untagged mailbox `FLAGS` response
  before their first FETCH FLAGS appearance, including when a solicited FETCH
  observes backend state newer than the connection's deferred update queue.
  The imaptest triage was removed; repeated unfiltered stress workloads and raw
  wire regressions now pass.

#### Removed

- **BREAKING:** `imapserver.MoveSupport` was removed. Backends and sessions now
  witness atomic MOVE solely by returning true for the `"MOVE"` token through
  `imapserver.CapabilitySupport`; selected mailboxes must still implement
  `imapserver.MoveMailbox`; `(*memory.Backend).SupportsMove` was removed with
  the redundant witness.

## [1.1.0] - 2026-08-21

### Root module — `github.com/kiliant/go-imap`

#### Added

- `imap.Envelope.RawDate`, allowing servers to reproduce malformed or unusually
  spelled message Date headers when encoding ENVELOPE data.
- Server-framework foundations: command-direction literal handling and response
  encoding in `internal/imapwire`, bidirectional semantic codecs in
  `internal/imapcodec`, and streaming message analysis/SEARCH evaluation in
  `internal/imapmessage`.

#### Changed

- `imapclient` now uses the shared semantic codec for FETCH, SEARCH, ENVELOPE
  and BODYSTRUCTURE without changing its exported API or interoperability
  behaviour.
- **The NOTIFY vocabulary is now defined once, in `package imap`.** The client
  and the server had each declared their own event and specifier names and had
  diverged: `imapclient` spelled events `MessageNew`, `imapserver` spelled them
  `MESSAGENEW`, so a backend comparing against the wrong package's constant
  matched nothing and simply never delivered.

  `imap.NotifyEventName` and `imap.NotifyMailboxSpecifier` are the definition.
  `imapclient`'s types keep their own identity — this package is frozen — but
  their constant *values* are now derived from the root constants, so the two
  cannot drift apart again, and a test requires the two constant sets to stay
  one-for-one. `imapclient`'s exported API is unchanged; `apidiff`
  reports no difference for it.

## [imapserver/v0.1.0] - 2026-08-21

### Server module — `github.com/kiliant/go-imap/imapserver`

First released on 2026-08-21.

**This module does not carry the root module's v1 compatibility promise.** It is
v0.x deliberately: the backend contract has had one round of real backend
authors and no more, and freezing it on that evidence is how the library this
project exists to replace ended up in beta for years. Breaks between minors are
allowed, will be named here, and are caught by `apidiff` running against the
previous `imapserver/v*` tag.

#### Added

- **The server framework.** Exported API: `Server`, `New`, `Serve`, `ServeConn`,
  `Close`, `Options`, `Limits`, `ConnInfo`, `Credentials` and the connection
  lifecycle around them. The three entry points take an options struct each —
  `ServeOptions`, `ServeConnOptions`, `ServerCloseOptions`, all empty and all
  accepting nil — so that per-listener settings can arrive without a break. RFC
  8314 implicit TLS on 993 alongside RFC 2595 `LOGINDISABLED` on cleartext 143
  is the case: it needs `RequireTLS` and `AllowInsecureAuth` to differ per
  listener, and today they are server-wide.
  Protocol framing, connection state, capability negotiation, sequence-number
  translation and update delivery are the framework's; accounts and stored mail
  are the backend's.
- **The backend contract.** Exported API: `Backend`, `Session`,
  `SelectedMailbox` — the mandatory IMAP4rev1 baseline, frozen by design — plus
  the writer types they stream through: `ListWriter`, `FetchWriter`,
  `ExpungeWriter`, `Updater`. A future extension adds an optional interface or
  a guarded option field, never a method to one of these. `WriteExpunge` takes
  `*WriteExpungeOptions` because its payload is a bare `imap.UID`; the other
  writers carry a growable struct already, which is the same guarantee.
- **Update delivery.** Exported API: the `Update` interface and its variants,
  plus `UpdateBatch` and `ChangeToken`. A backend publishes changes; the
  framework decides what each connection may see and when, which is what keeps a
  command from being told about a change it must not observe yet.
- **Twenty-four optional capability interfaces**, discovered by type assertion:
  `MoveMailbox`, `CondStoreMailbox`, `QResyncMailbox`, `ReplaceMailbox`,
  `SortMailbox`, `ThreadMailbox`, `MultiSearchSession`, `CatenateSession`,
  `ACLSession`, `ACLSetSession`, `QuotaSession`, `QuotaSetSession`,
  `MetadataSession`, `NamespaceSession`, `NotifySession`, `FilterSession`,
  `ComparatorSession`, `LanguageSession`, `URLAuthSession`,
  `MessageLimitSession`, `UnauthenticateSession`, `SCRAMCredentials`, and the
  `CapabilitySupport` witness. Adding a capability is a new
  interface or a new witness token, not a change to an existing type.
- **~55 capabilities across RFC groups A–E**, listed per RFC in
  `docs/RFC-COVERAGE.md`. Everything in scope is implemented except UTF8=ALL and
  UTF8=USER (deprecated by RFC 9755) and UTF8=ONLY (asserts a deployment policy
  the framework does not enforce), each recorded with its reason.
- **`imapserver/memory`** — a supported in-memory backend implementing every
  optional interface, not a toy: it is what this project's own conformance and
  interoperability suites run against.
- **`imapserver/backendtest`** — a reusable conformance suite a third-party
  backend can point at itself. It exercises the mandatory contract and every
  optional interface implemented, skipping the rest. A backend author's first
  stop, ahead of the interface list.
- **Runnable examples** under `imapserver/examples/`: a minimal server, a TLS
  one, and one per optional-interface witness style.

#### Notes for backend authors

- **A session wrapper hides every optional interface it wraps.** The framework
  discovers support by type-asserting the value it holds. Wrap a session that
  implements two dozen optional interfaces in a type implementing one, and the
  server supports one. `imapserver/examples/config.go` documents the pattern
  and the trap together.
- **`IMAP4REV2` is an umbrella and is witnessed by its members.** A backend is
  offered it only when it witnesses the whole set RFC 9051 §1 incorporates —
  CHILDREN, MOVE, NAMESPACE, SPECIAL-USE, STATUS=SIZE and UIDPLUS. Witnessing
  only some of them and being held to all of them was a real defect, caught in
  review before the first tag; see `docs/API-STABILITY.md` §10.

#### Changed before the first tag

- **BREAKING (pre-tag): `Session.Close` and `SelectedMailbox.Unselect` now take
  an options struct**, `*SessionCloseOptions` and `*UnselectOptions`. They were
  the only two methods on the whole backend surface without one, on the two
  frozen mandatory interfaces, whose own doc promises "an option field, not a
  method here" as the extension route — a promise that was false for exactly
  those two. RFC 6785 (IMAPSIEVE) is the concrete pressure: a session ending
  because the connection closed is a different event from one ending because
  UNAUTHENTICATE reclaimed it, and CLOSE's implicit expunge is a different event
  from UNSELECT's deliberate lack of one. Both structs are empty today.
  `TestBackendMethodsTakeOptions` now gates the rule, which nothing did before.

- **Extension SEARCH keys and FETCH items are capability-gated.** Previously a
  backend witnessing nothing still received `FUZZY` and `MODSEQ` from a server
  that advertised neither `SEARCH=FUZZY` nor `CONDSTORE`: every extension
  *command* handler gated itself, but a search key is not a command and a fetch
  item is not a command. The framework now classifies both — as data, in
  `imapserver/capability_keys.go` — and refuses what it cannot classify.
- `imapserver/memory` now witnesses `SEARCH=FUZZY`, which it evaluated but never
  advertised.

- **BREAKING (pre-tag): `backendtest.Harness.New` takes a context, an options
  struct and returns an error**, `func(ctx context.Context, options *InstanceOptions) (*Instance, error)`. A real backend's setup
  can block and can fail; without these the only way to report a failure was to
  capture the subtest's `*testing.T` in the closure, and there was no way to
  cancel at all. `InstanceOptions` is empty today and exists for rule 3: this
  package is a backend author's documented first stop, so day-one adopters write
  closures against this signature, and provisioning an implicit-TLS instance
  (RFC 8314), a second identity for ACL rights (RFC 4314) or an instance that
  survives a reconnect (RFC 7162) would each be a new parameter otherwise.

- **Deferred updates and the queue bound.** Because unsolicited updates now wait
  for a pipeline to drain, a client that keeps a deep pipeline full during heavy
  concurrent mailbox churn can reach the update-queue bound where the previous
  code would have drained between commands. The outcome is a `BYE` and a closed
  connection rather than a desynchronised one, which is the direction to fail
  in, but it is a behaviour change worth knowing about.

#### Fixed

- **Untagged `EXPUNGE` obeys both halves of RFC 3501 §7.4.1.** The first half —
  no delivery while a pipelined `FETCH`, `STORE` or `SEARCH` is outstanding —
  was already deferred past a command's tagged completion, and pipelining
  defeated that: "after the tagged OK of command *n*" is simultaneously "while
  command *n+1* is in flight". The condition is now the connection's own
  backlog. The second half — "MUST NOT be sent when no command is in progress"
  — was not modelled at all, so the event loop delivered between commands on its
  own update signal. Delivery now requires a command in progress; the exemption
  for a command's own changes is per batch rather than per drain, so STORE can
  no longer flush unrelated sessions' removals through that exemption.

- **Removal commands apply the queued revision prefix before reporting sequence
  numbers.** EXPUNGE and MOVE used to convert the backend's returned UIDs
  against the selected snapshot immediately. When an earlier unsolicited removal
  was still deferred, that snapshot was one revision behind the UIDs, so the
  sequence numbers belonged to a mailbox view the client had never been told
  about (`seq too high` under Dovecot's `imaptest`). `drainUpdatesThrough` now
  pops every batch through the command's own origin as one ordered unit;
  EXPUNGE/MOVE no longer write sequence numbers themselves. They also drain the
  whole current queue first: a deferred removal parks every ADD behind it, and
  rejecting those UIDs from `WriteExpunge` was the intermittent
  `NO [SERVERBUG] EXPUNGE failed` under stress.

  Found by `imaptest` stress. Pinned by
  `TestExpungeUpdateWaitsForPipelinedCommands`,
  `TestExpungeUpdateWaitsForACommandToBeInProgress`,
  `TestDeferredCommandUpdateKeepsItsAccounting` and
  `TestExpungeAppliesQueuedAddsBeforeBackendCall`.

- **BINARY fetch items are gated on the framework's feature rather than on the
  `BINARY` token.** RFC 9051 incorporates RFC 3516's fetch half, so `BINARY[]`
  is legal for a session that enabled IMAP4rev2 whether or not the token — which
  additionally claims the APPEND half rev2 did not incorporate — was ever
  advertised.

#### Known issues

- ~~Untagged `EXPUNGE` during a pipelined `FETCH`/`STORE`/`SEARCH`~~ — **fixed**.
  See *Fixed* above.
- ~~`EXPUNGE` between commands / revision-skewed sequence numbers~~ — **fixed**.
  See *Fixed* above.
- **A keyword created by `STORE` is not re-announced in `FLAGS`.**

## [1.0.0] - 2026-08-06

### Added

- **`package imap` — core protocol vocabulary.** Exported API: the shared types
  every layer speaks — `Error` as the single protocol error type carrying
  response code, tag and text; `ResponseCode`, `FetchItem` /
  `FetchItemKeyword`, `SearchCriteria`, `StatusItemKeyword`, `Flag`,
  `NumSet`/`SeqSet`/`UIDSet` with set arithmetic, `Envelope`, `BodyStructure`,
  and the RFC 2047 / RFC 2231 MIME header helpers. The FETCH item, SEARCH
  criterion and STATUS item sets are deliberately open — extension RFCs add
  members to all three — so none is a closed constant list or a fixed struct of
  `bool` fields, and capability names stay plain strings for the same reason.
  The package performs no I/O, which is what will let a future server framework
  reuse it without an API break.
- **`imapclient` — connection and session layer.** Exported API: `Client`,
  `Options`, `Dial`, `DialTLS`, `DialStartTLS`, `NewClient`, plus the unilateral
  update callbacks. Secure dial and STARTTLS, greeting and capability handling,
  pipelined command demultiplexing with state enforcement, cancellation that
  poisons the session rather than desynchronising it, and redacted tracing.
  `context.Context` is the first parameter of every blocking call from the first
  commit — retrofitting it later is breaking, and is the most common reason Go
  network libraries never reach v1.
- **Authentication.** Exported API: `Client.Login`, `Client.Authenticate`,
  `AuthenticateOptions`, `LoginOptions`. Mechanisms live in `internal/imapsasl`:
  PLAIN (RFC 4616), CRAM-MD5 (RFC 2195), SCRAM-SHA-1 and SCRAM-SHA-256 (RFC
  5802, RFC 7677) with their `-PLUS` channel-binding variants bound to the TLS
  exporter, OAUTHBEARER (RFC 7628) and the de-facto XOAUTH2.
- **Mailbox and message commands.** Exported API: `Client.Select`, `Unselect`
  (RFC 3691), `CloseMailbox`, `Create`, `Delete`, `Rename`, `Subscribe`,
  `Unsubscribe`, `List`, `Status`, `Namespace` (RFC 2342), `Fetch`/`FetchUID`,
  `Store`, `Search`, `Append`, `Copy`, `Move`, `Expunge`, `Check`, `Noop`, and
  their command handles and data types. Large
  bodies stream; a 5 MiB message is verified not to be buffered whole.
- **CAPABILITY, ENABLE, IDLE and IMAP4rev2.** Exported API:
  `Client.Capability`, `Client.Capabilities`, `Client.CapabilityValues`,
  `Client.Enable`, `Client.EnableUTF8Accept`, `Client.Idle` and `IdleCommand`.
  Capability values and refresh, ENABLE subset tracking, the rev2 mandatory-
  capability bridge (RFC 9051), IDLE (RFC 2177) with readiness and renewal, and
  a NOOP-polling fallback for servers without it.
- **Extension group A — core modern.** Exported API: UIDPLUS `UID EXPUNGE` and
  `APPENDUID`/`COPYUID` data (RFC 4315), SEARCHRES saved results (RFC 5182),
  `MOVE` with an opt-in non-atomic COPY+STORE+EXPUNGE fallback (RFC 6851),
  ESEARCH with a client-side fallback (RFC 4731), LIST-EXTENDED (RFC 5258),
  LIST-STATUS (RFC 5819), SPECIAL-USE and CREATE-SPECIAL-USE (RFC 6154),
  CHILDREN (RFC 3348), WITHIN (RFC 5032), and `Client.ID` with `IDOptions`,
  `IDData`, `IDField` and `IDString` (RFC 2971).
- **Extension group B — synchronisation and identity.** Exported API: CONDSTORE
  conditional FETCH and STORE and QRESYNC quick resynchronisation (RFC 7162),
  OBJECTID `EMAILID`/`THREADID`/`MAILBOXID` (RFC 8474), SAVEDATE (RFC 8514),
  STATUS=SIZE (RFC 8438), APPENDLIMIT (RFC 7889), PREVIEW (RFC 8970), and
  REPLACE / UID REPLACE with the RFC 8508 §3.4 non-atomic fallback.
- **Extension group C — content and structure.** Exported API: BINARY (RFC
  3516), CATENATE (RFC 4469), MULTIAPPEND (RFC 3502), COMPRESS=DEFLATE (RFC
  4978), the UTF8= family (RFC 9755, RFC 5738), SORT and THREAD (RFC 5256),
  SORT=DISPLAY (RFC 5957), MULTISEARCH (RFC 7377), PARTIAL (RFC 9394) and
  SEARCH=FUZZY (RFC 6203).
- **Extension groups D and E — administrative, legacy and niche.** Exported API:
  QUOTA (RFC 9208), ACL and LIST-MYRIGHTS (RFC 4314, RFC 8440), METADATA and
  LIST-METADATA (RFC 5464, RFC 9590), NOTIFY (RFC 5465), UNAUTHENTICATE (RFC
  8437), UIDONLY (RFC 9586), INPROGRESS (RFC 9585), MESSAGELIMIT / SAVELIMIT
  (RFC 9738), JMAPACCESS (RFC 9698), URLAUTH (RFC 4467, RFC 5524, RFC 5550),
  LANGUAGE and I18NLEVEL (RFC 5255), CONTEXT=SEARCH / CONTEXT=SORT / ESORT (RFC
  5267), FILTERS (RFC 5466) and the referral response codes (RFC 2221, RFC
  2193).
- **`Options.WriteTimeout`.** Exported API: additive keyed field on the existing
  `imapclient.Options`, default 5 minutes. Bounds a server that stops draining
  its receive window, mirroring the read side. Sets no precedent — it is the
  keyed-literal mechanism `docs/API-STABILITY.md` rule 7 specifies.
- **`AuthenticateOptions.PrepareCredentials` and `LoginOptions.PrepareCredentials`.**
  Exported API: additive keyed fields enabling SASLprep (RFC 4013, RFC 3454)
  over user name and password, backed by new `internal/saslprep` and
  `internal/unicodenorm` packages (NFC/NFKC, Unicode 15.0.0, all 19074
  `NormalizationTest` cases, no `x/text`). **Opt-in, and empirically so:**
  Dovecot 2.4.3 and Stalwart 0.11.8 both compare raw password octets, so
  applying preparation unconditionally would break authentication against them.
  Bearer tokens, the OAuth mechanisms and caller-supplied mechanisms are
  deliberately untouched.
- **`(*imapclient.StatusData).Number`.** Exported API: additive accessor
  returning `(uint64, bool)` for an open `imap.StatusItemKeyword`. `StatusData`
  keeps its open `Values` map so unmodelled STATUS items still reach the caller;
  `Number` makes the previously undocumented type contract public and testable,
  distinguishing an absent item from a present zero.
- **Interop harness.** `interop/**`, driving Dovecot, Stalwart, GreenMail, Cyrus
  and Courier natively and Apache James under emulation, behind `-tags=interop`
  and `-tags='interop interop_emulated'`. Tests skip on absent server
  capabilities and fail when a server omits a capability its profile claims — a
  permanently red matrix is a matrix nobody reads, and a silently all-skipping
  one is worse. No exported API.
- **Fuzzing.** 61 fuzz targets across the wire codec, the response parsers of
  every extension group, the response codes, SASLprep and normalisation. A
  10-minute campaign over all 61 completed clean (2026-08-03, ~1.085 billion
  executions, no crasher). No exported API.
- **API surface tests** (`api_surface_test.go`): assert that no `internal/` type
  is reachable from an exported signature (rule 6, via a `go/types` walk), that
  structs callers construct carry the keyed-literal doc note (rule 7), that
  command entry points take an options struct (rule 3), and that every program
  under `examples/` still compiles. No exported API.
- **Release engineering** (`.github/**`): CI for tests on the two most recent Go
  majors across linux and macOS; `go vet` / `staticcheck` / `gofmt` including
  the `interop` and `interop interop_emulated` tagged builds; the native and
  emulated interop matrices; 60-second per-target fuzz smoke on every pull
  request and 10-minute campaigns nightly; an `apidiff` gate against the
  previous tag; and a supply-chain assertion that `go.sum` stays absent or
  empty. No exported API.

### Changed

- **The shared response vocabulary moved from `imapclient` to `package imap`
  (T17).** Exported API: about 50 symbols that describe wire data rather than
  client plumbing now live in `package imap`, with an alias of the same name
  left in `imapclient`. Type identity is preserved, so every existing caller and
  every keyed struct literal keeps compiling — the technique the standard
  library used to relocate `context.Context`.

  Moved, and aliased: `StatusData` (with `Number`), `ListData`,
  `NamespaceDescriptor`, `NamespaceData`, `AppendData`, `CopyData`,
  `MultiAppendData`, `ACLRights`, `ACLEntry`, `ACLData`, `ListRightsData`,
  `MyRightsData`, `QuotaResourceName` with its four constants,
  `QuotaResource`, `QuotaData`, `QuotaRootData`, `QuotaResourceLimit`,
  `MetadataEntryName`, `MetadataEntry`, `MailboxMetadata`, `VanishedData`,
  `SeqMatchData`, `SyncStoreData` (with `HasModified`), `ESearchReturnKey` with
  its seven constants, `PartialRange`, `PartialSearchData`,
  `MultiSearchResult`, `MultiSearchData`, `SortKey` with its ten constants,
  `SortKeySpec`, `ThreadAlgorithm` with its two constants, `ThreadNode`,
  `ThreadData`, `IDField`, `InProgressData`, `JMAPAccessData`, `LanguageData`,
  `ComparatorData`, `MessageLimitPartial`, `ReferralData`, `GenURLAuthData`,
  `URLFetchItem`.

  Moved but **not** aliased, because each shed a field only a decoder can fill:
  `MailboxStatus`, `SortData`, `IDData` and `ESearchData`. The `imapclient`
  types of those names are now wrapper structs embedding their `imap`
  counterparts — see the four `BREAKING` entries below.

  The reason is structural: a server backend cannot name a type in `imapclient`
  without `imapserver` importing `imapclient`, which inverts the dependency
  graph the layering exists to protect. `apidiff` reports the move as
  incompatible and that report is a false positive — it models a type by its
  defining package and cannot see that an alias preserves identity; a minimal
  control reproduces the same spurious output. Verified instead by compiling an
  external consumer written against the pre-move spelling.

  One real, if cosmetic, cost: `go doc ./imapclient` no longer lists the methods
  of an aliased type, since Go attributes them to the defining package. Look for
  `StatusData.Number` and `SyncStoreData.HasModified` (and `ESearchData.Partial`
  / `RelevancyScores` below) under `go doc github.com/kiliant/go-imap`, not under
  `imapclient` — they are still callable exactly as before, just documented one
  package over.

- **BREAKING — `imap.AppendData`, `imap.CopyData` and `imap.MultiAppendData`
  carry an explicit presence field (T17).** Exported API: `AppendData` gained
  `HasUID`; `CopyData` and `MultiAppendData` gained `HasUIDs`; and
  `imapclient.CopyData.Received()` was **removed** in favour of
  `CopyData.HasUIDs`, which is what it computed. Presence was previously
  inferred from a non-zero `UIDValidity`, which is sound only for a decoder that
  knows the wire cannot carry a zero UIDVALIDITY. A producer holding a value it
  did not decode cannot distinguish "the response code is absent" — a normal
  outcome under RFC 4315 section 3 — from "not filled in yet". A derived
  accessor is also the asymmetry this audit exists to remove: readable by
  anyone, settable only by the declaring package.

  **Note for callers:** the removed `Received()` was nil-tolerant and the field
  is not. `CopyCommand.Wait` returns a nil `*CopyData` alongside its error, so
  `d, _ := cmd.Wait(ctx); if d.Received()` was safe and `d.HasUIDs` panics.
  Check the error, or check `d != nil`.

- **BREAKING — `SortData` and `IDData` split the same way as `MailboxStatus`
  (T17).** Exported API: `imap.SortData` (`SeqNums`, `UIDs`) and `imap.IDData`
  (`Fields`) are new. `imapclient.SortData` now embeds `imap.SortData` and keeps
  only `Emulated`; `imapclient.IDData` now embeds `imap.IDData` and keeps only
  `Received`. Both fields describe how a decoder obtained the value — a
  client-side SORT fallback, and the absence of an untagged ID response — so no
  server can fill either. Field reads are unchanged by promotion; a keyed
  literal setting a shared field must name the embedded value.

- **`imap.MailboxStatus` can express NOMODSEQ (T17).** Exported API:
  `imap.MailboxStatus` gained `NoModSeq bool`. Additive. The client already
  decoded the NOMODSEQ response code onto `imapclient.SyncMailboxStatus`, so the
  shared SELECT vocabulary was the only side that could not say it — an
  asymmetry, not a symmetric gap, and therefore fixed rather than deferred.

- **BREAKING — `ESearchData` split, and its parse helpers became methods
  (T17).** Exported API: `imap.ESearchData` is new and carries no `Emulated`
  field; it gains the value-receiver methods `Partial() (*PartialSearchData,
  error)` and `RelevancyScores() ([]uint8, error)`. `imapclient.ESearchData` now
  embeds it and keeps only `Emulated`. `imapclient.ParsePartialSearchData` and
  `imapclient.ParseRelevancyScores` are **removed**, superseded by the methods;
  `data.Partial()` replaces `ParsePartialSearchData(data)` and works on both
  spellings through promotion. `imapclient.ESearchReturnKeyRelevancy` now
  references `imap.ESearchReturnKeyRelevancy` rather than redeclaring the same
  literal.

  `Emulated` says the value was reconstructed from an ordinary SEARCH response
  because the server does not advertise ESEARCH, so no server produces it — and
  `imap.MultiSearchResult.Data` is an `imap.ESearchData` by value, so leaving it
  on the shared type put a permanently-unfillable field in front of every server
  answering a MULTISEARCH (RFC 7377). The helpers became methods rather than
  changing parameter type because a server decoding an upstream ESEARCH must be
  able to call them without importing `imapclient`. The receivers are values so
  that `result.Data.Partial()` works on a non-addressable field.

- **BREAKING — every exported struct in `package imap` gained an
  unkeyed-literal guard (T17).** Exported API: 39 structs gained an unexported
  `_ struct{}` field, including all thirteen `Search*` criteria structs, the
  five `FetchItem*` request structs, the five `BodyStructure` concrete types,
  `Envelope`, `Address`, `Error`, `FetchMessageData`, `SectionPartial` and the
  four streaming fetch values. Keyed literals are unaffected; unkeyed ones no
  longer compile, and nothing in the tree, the tests or the examples used one
  under any build tag.

  There is one exception, `NumRange[N]`: a range is exactly a start and a stop,
  so `NumRange[SeqNum]{1, 5}` stays legal. `docs/API-STABILITY.md` rule 7 now
  states the requirement by principle and lists that exception with its reason.
  Adding a guard is itself a breaking change, so this could only be done before
  the tag.

- **BREAKING — `MailboxStatus` split into shared state and client observation
  (T17).** Exported API: `imap.MailboxStatus` is new and carries the mailbox
  state both protocol directions can express. `imapclient.MailboxStatus` (and
  its alias `imapclient.SelectData`) now embeds it and keeps only
  `UIDValidityChanged`. Field reads are unchanged by promotion; a keyed literal
  must now name the embedded value,
  `&MailboxStatus{MailboxStatus: imap.MailboxStatus{...}}`. `UIDValidityChanged`
  is derived by comparing the reported UIDVALIDITY against the one the client
  last saw, so no server can produce it. Moving it with a "server always leaves
  this false" note was rejected: a field one of two users can never fill is
  evidence of a wrong semantic boundary, and pre-v1.0 is when that is removable
  rather than permanent for ever.

- **BREAKING — every command entry point takes an options struct.** Exported
  API: 28 `imapclient.Client` methods gained a trailing (or pre-variadic)
  options pointer — `Check`, `CloseMailbox`, `Copy`, `CopyUID`, `Create`,
  `Delete`, `Enable`, `EnableUTF8Accept`, `Expunge`, `Fetch`, `FetchUID`,
  `Idle`, `Namespace`, `Noop`, `Rename`, `Subscribe`, `Unselect`, `Unsubscribe`,
  `FetchBinarySize`, `FetchBinarySizeUID`, `MultiAppend`, `MailboxSize`,
  `AppendLimit`, `MailboxHighestModSeq`, `Login`, `Capability`, `WaitGreeting`
  and `Logout`. `docs/API-STABILITY.md` rule 3 requires options in a struct,
  because a new RFC adds a field where a new parameter breaks every caller. The
  cost of *not* doing this was already visible in the tree: two methods carried
  doc comments explaining that they could not gain a feature because they had no
  options struct. Deliberately taken before v1.0, when it is still allowed.
- **BREAKING — SEARCH RETURN and COPYUID share one vocabulary each.** Exported
  API: duplicate types introduced independently by the group A and group B work
  were collapsed onto a single set, and tagged `OK` response codes
  (`APPENDUID`, `COPYUID`, `MODIFIED`) are now delivered to the caller.
- **BREAKING — `Client.CreateMailbox` folded back into `Client.Create`.** See
  *Removed*. Other API-review corrections made before their extension groups
  reached `main` — positional payload options across groups C, D and E, an
  unexported INPROGRESS options type, and a positional LIST-with-METADATA
  variant — are not listed separately: they never appeared in a merged exported
  surface, so they are not changes anyone can observe.
- **Interop container names embed the process ID.** Two packages starting the
  same server profile within one wall-clock second previously generated
  identical names and the container runtime refused the second outright. No
  exported API.

### Removed

- **BREAKING — `imapclient.Client.CreateMailbox`.** Exported API: it existed
  only because `Create` took no options struct and adding a parameter would have
  broken every caller. With `Create` now taking `*CreateOptions`, the sibling
  method has no reason to exist. Use `Create` with options.

### Fixed

- **Response hardening limits are enforced on the production read path.**
  Bounded line, literal, token and per-command retained-response sizes, plus a
  default read deadline, so a hostile or broken server cannot exhaust memory.
  Malformed input reaches the caller as an `*imap.Error`, never a panic. No
  exported API.
- **Session hangs, panics and missing parsers** in the pipelined command
  demultiplexer; a rejected literal now leaves the stream synchronised per RFC
  3501 §4.3 while a write timeout correctly poisons the session instead. No
  exported API.
- **Unilateral `VANISHED` responses** are dispatched to the update callbacks
  rather than dropped. No exported API.
- **`1:*` number-set encoding**, `RECENT` no longer requested in the rev2
  LIST-STATUS default set, and the OBJECTID / PREVIEW / SAVEDATE parsers
  corrected against live servers. No exported API.
- **APPENDLIMIT precedence and the 63-bit ceiling** are documented and the
  mailbox-specific value correctly overrides the server-wide one. No exported
  API.
- **`FetchDataRaw` reader contract** corrected in its doc comment: it described
  a lifetime the implementation did not provide. Documentation only; no
  signature change.
- **Flaky `TestFetchAbandonedBeforeCloseDoesNotPanic`** (~30% failure rate) made
  deterministic. No exported API.

### Security

- **Credential redaction** covers the prepared (SASLprep) forms of a user name
  and password as well as the caller's input. The prepared octets are what
  reached the server, and the two differ by construction for exactly the inputs
  the option exists to handle, so a server echoing the authentication identity
  in its `NO` text would otherwise have leaked the prepared name.
- **Zero external dependencies**, asserted in CI: `go.sum` must be absent or
  empty, `go.mod` must contain no `require` directive, and no package — under
  any build-tag combination, including test-only code — may import outside the
  standard library and this module. Test-only dependencies are covered too; the
  interop harness shells out to a container CLI rather than using an SDK.

[Unreleased]: https://github.com/kiliant/go-imap/compare/v1.2.1...HEAD
[1.2.1]: https://github.com/kiliant/go-imap/releases/tag/v1.2.1
[imapserver/v0.3.0]: https://github.com/kiliant/go-imap/releases/tag/imapserver/v0.3.0
[1.2.0]: https://github.com/kiliant/go-imap/releases/tag/v1.2.0
[imapserver/v0.2.0]: https://github.com/kiliant/go-imap/releases/tag/imapserver/v0.2.0
[1.1.0]: https://github.com/kiliant/go-imap/releases/tag/v1.1.0
[imapserver/v0.1.0]: https://github.com/kiliant/go-imap/releases/tag/imapserver/v0.1.0
[1.0.0]: https://github.com/kiliant/go-imap/releases/tag/v1.0.0
