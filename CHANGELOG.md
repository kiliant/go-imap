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

## [Unreleased]

No release has been tagged yet. Everything below is the work leading to the
first one, grouped by area rather than by date; the release that cuts it will
move these entries under its own heading.

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

  Moved: `StatusData` (with `Number`), `ListData`, `NamespaceDescriptor`,
  `NamespaceData`, `MailboxStatus` (see below), `AppendData`, `CopyData`,
  `MultiAppendData`, `ACLRights`, `ACLEntry`, `ACLData`, `ListRightsData`,
  `MyRightsData`, `QuotaResourceName` with its four constants,
  `QuotaResource`, `QuotaData`, `QuotaRootData`, `QuotaResourceLimit`,
  `MetadataEntryName`, `MetadataEntry`, `MailboxMetadata`, `VanishedData`,
  `SeqMatchData`, `SyncStoreData` (with `HasModified`), `ESearchReturnKey` with
  its seven constants, `ESearchData`, `PartialRange`, `PartialSearchData`,
  `MultiSearchResult`, `MultiSearchData`, `SortKey` with its ten constants,
  `SortKeySpec`, `SortData`, `ThreadAlgorithm` with its two constants,
  `ThreadNode`, `ThreadData`, `IDField`, `IDData`, `InProgressData`,
  `JMAPAccessData`, `LanguageData`, `ComparatorData`, `MessageLimitPartial`,
  `ReferralData`, `GenURLAuthData`, `URLFetchItem`.

  The reason is structural: a server backend cannot name a type in `imapclient`
  without `imapserver` importing `imapclient`, which inverts the dependency
  graph the layering exists to protect. `apidiff` reports the move as
  incompatible and that report is a false positive — it models a type by its
  defining package and cannot see that an alias preserves identity; a minimal
  control reproduces the same spurious output. Verified instead by compiling an
  external consumer written against the pre-move spelling.

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

[Unreleased]: https://github.com/kiliant/go-imap/commits/main
