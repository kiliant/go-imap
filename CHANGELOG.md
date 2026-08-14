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

### Server module — `github.com/kiliant/go-imap/imapserver`

Not yet released. This is the content of the first `imapserver/v0.1.0` tag.

**This module does not carry the root module's v1 compatibility promise.** It is
v0.x deliberately: the backend contract has had one round of real backend
authors and no more, and freezing it on that evidence is how the library this
project exists to replace ended up in beta for years. Breaks between minors are
allowed, will be named here, and are caught by `apidiff` running against the
previous `imapserver/v*` tag.

#### Added

- **The server framework.** Exported API: `Server`, `New`, `Serve`, `Options`,
  `Limits`, `ConnInfo`, `Credentials` and the connection lifecycle around them.
  Protocol framing, connection state, capability negotiation, sequence-number
  translation and update delivery are the framework's; accounts and stored mail
  are the backend's.
- **The backend contract.** Exported API: `Backend`, `Session`,
  `SelectedMailbox` — the mandatory IMAP4rev1 baseline, frozen by design — plus
  the writer types they stream through: `ListWriter`, `FetchWriter`,
  `ExpungeWriter`, `Updater`. A future extension adds an optional interface or
  a guarded option field, never a method to one of these.
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
  `MoveSupport` / `CapabilitySupport` witnesses. Adding a capability is a new
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

- **BREAKING (pre-tag): `backendtest.Harness.New` takes a context and returns an
  error**, `func(ctx context.Context) (*Instance, error)`. A real backend's setup
  can block and can fail; without these the only way to report a failure was to
  capture the subtest's `*testing.T` in the closure, and there was no way to
  cancel at all. This package is a backend author's first stop, so it should not
  be the one place that ignores rule 2.

#### Known issues

- **Untagged `EXPUNGE` can be delivered during a pipelined `FETCH`, `STORE` or
  `SEARCH`**, which RFC 3501 §7.4.1 forbids because it desynchronises sequence
  numbers. Expunges are already deferred past a command's tagged completion;
  pipelining defeats that, because "after the tagged OK of command *n*" is
  "while command *n+1* is in flight". Found by Dovecot's `imaptest` and
  recorded in its triaged table, so the suite fails on anything new.
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

[Unreleased]: https://github.com/kiliant/go-imap/compare/v1.0...HEAD
[1.0.0]: https://github.com/kiliant/go-imap/releases/tag/v1.0
