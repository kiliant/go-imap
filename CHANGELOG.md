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

#### Fixed

- **imapmessage:** base64 bodies decode as RFC 2045 section 6.8 requires:
  characters outside the alphabet are ignored and `=` ends the data. Spaces or
  tabs inside base64 no longer make BINARY fail with UNKNOWN-CTE or SEARCH fall
  back to the encoded octets. BINARY accepts multipart and message sections
  and returns their stored octets (RFC 3516). An address without a domain,
  such as `From: d`, stays in the ENVELOPE with host `MISSING_DOMAIN` rather
  than being dropped. Adds the re-encoding of NUL-bearing parts behind
  `imapserver/message`. Only `imapserver` uses this package; a standalone
  server build receives these fixes with the next root release. No exported
  API change. (#205)

### Server module — `github.com/kiliant/go-imap/imapserver`

#### Changed

- **BREAKING (pre-v1 contract):** `QResyncMailbox.Resync` may now be called
  repeatedly within a selection for UID FETCH with CHANGEDSINCE and VANISHED,
  in addition to QRESYNC SELECT/EXAMINE. `QResyncSelect` carries the selection's
  UIDVALIDITY, CHANGEDSINCE clock and full requested UID set; sequence-match
  data is absent. Only Vanished is used for UID FETCH, before ordinary FETCH
  results. Backendtest checks repeatable history under the QRESYNC witness.
  No exported signature or interface change. (#54, G-3)
- **BREAKING behaviour (pre-v1):** VANISHED is refused with BAD unless used
  on UID FETCH after ENABLE QRESYNC with QRESYNC still advertised. Previously
  the parsed modifier was ignored, including in these invalid forms.
  No exported API change. (#54, G-3)
- **BREAKING behaviour (pre-v1):** UIDONLY SELECT and EXAMINE omit the rev1
  UNSEEN response code, which carries a message sequence number. Ordinary
  rev1 sessions retain it. No exported API change. (#54, G-1)

- **BREAKING behaviour (pre-v1):** once UIDONLY is enabled, SEARCH, SORT,
  THREAD, COPY, MOVE and REPLACE join FETCH and STORE in requiring their UID
  forms. Sequence-set criteria at every search nesting depth and QRESYNC
  sequence-match data are refused with BAD [UIDREQUIRED]. UID SEARCH `$`
  continues to refer to saved UIDs. No exported API change. (#54, S-02)
- **BREAKING behaviour (pre-v1):** APPEND, MULTIAPPEND, CATENATE and REPLACE
  reject 8-bit message headers with NO until UTF8=ACCEPT or IMAP4rev2 is
  enabled. The framework scans staged headers before calling the backend;
  8-bit body content remains accepted. No exported API change. (#54, S-03)
- **BREAKING behaviour (pre-v1):** the obsolete UTF8=APPEND capability and
  memory witness are removed. Clients use UTF8=ACCEPT (RFC 9755) instead of
  RFC 5738's obsolete `UTF8 (literal8)` syntax. CATENATE TEXT now requires an
  ordinary literal as specified by RFC 4469; literal8 is accepted as the
  whole APPEND/REPLACE payload with BINARY. No exported API change. (#54, S-06)

- **BREAKING (pre-v1 contract):** `UpdateBatch` and `Updater.Push` now require
  publishing every committed batch to all attached updaters before a mutating
  call returns or STATUS/SELECT exposes the commit, including external commits
  and commits followed by errors. Late-feed backends must add a publication
  fence. Both reference backends already satisfy this duty; `backendtest.Run`
  now checks APPEND, COPY, STORE, EXPUNGE and MOVE at the call boundary and tests
  the STATUS/SELECT visibility fence. Correct
  `ExpungeWriter.WriteExpunge`: wire removals come from the ordered batches
  after the backend call. No interface or signature change. (#54, B-1)
- **BREAKING (pre-v1 bound semantics):** `Limits.MaxQueuedUpdateBytes` permits
  the undrained byte budget plus one atomic commit. `MaxQueuedUpdates` bounds
  undrained batches. Applied, protocol-deferred removals are compact UID records
  bounded by MaxSelectedMessages, separately from backlog. A withheld removal
  therefore does not cause a later STORE or ordinary change to disconnect clients.
  Add `Limits.MaxStoreResponseBytes`, default 32 MiB, charging wire bytes plus
  1 KiB per retained response. Exhaustion switches to a fresh streaming refetch
  after mutation completion, preserving OK and conditional MODIFIED results.
  A failed committed-state stream sends BYE. (#54, B-2/D-5, review N-1/N-2)
- **BREAKING behaviour:** SORT classifies keys before backend access. Unknown
  keys and repeated REVERSE receive BAD; DISPLAYFROM/DISPLAYTO require
  SORT=DISPLAY; RELEVANCY requires SEARCH=FUZZY and a FUZZY criterion. Correct
  the SortMailbox contract. Enforce URL-PARTIAL withdrawal and report withdrawn
  FILTERS as a gate refusal. Existing interfaces and signatures stay unchanged.
  (#54, review N-4)
- **BREAKING behaviour:** URL-PARTIAL withdrawal also refuses CATENATE range
  URLs with NO [BADURL] before backend resolution. The command drains later
  parts without resolving them or committing. URLFETCH returns NIL for each
  unavailable range URL and continues serving other URLs. (#54, Z-3)
- **BREAKING behaviour:** A refused CATENATE URL containing `]`, CR, LF or
  NUL is answered with a plain NO, because BADURL cannot echo it (RFC 4469
  section 5). It was NO [SERVERBUG], and `OnError` received a
  `backend-error` event carrying the client's URL; no event is reported now.
  Other refused URLs keep NO [BADURL <url>]. APPEND and REPLACE commit nothing
  either way. No exported API change. (#204)
- **BREAKING (pre-v1 contract):** `message.Message.OpenBinarySection` accepts
  multipart and encapsulated message sections and returns their stored octets,
  as RFC 3516 allows; it refused them before. Base64 that contains characters
  outside the alphabet is decoded with those characters ignored (RFC 2045
  section 6.8) instead of failing with CodeUnknownCTE, which now means only an
  unknown transfer encoding. (#205)
- **BREAKING behaviour (memory):** A BINARY APPEND whose parts contain NUL is
  stored with those parts re-encoded as base64, which RFC 3516 section 4.4
  permits because nothing is lost. FETCH BODY of such a message closed the
  connection, since no IMAP string can carry NUL; BODY and BODYSTRUCTURE now
  describe base64 and BINARY returns the original octets. (#205)
- **BREAKING behaviour (memory):** Subscriptions belong to mailbox names, not
  mailboxes. DELETE keeps a subscription (RFC 3501 section 6.3.9), and
  LIST (SUBSCRIBED) returns it as `\NonExistent` (RFC 5258 section 3).
  UNSUBSCRIBE removes it even after the mailbox is gone, and re-creating the
  name finds it subscribed. RENAME no longer moves the subscription to the new
  name; it stays on the old one (RFC 9051 section 6.3.6). LIST also returns a
  hierarchy level that is not a mailbox as `\NonExistent \HasChildren` when a
  pattern such as `a/%` matches it and its descendants lie beyond the
  pattern (RFC 5258 section 3.5). No exported API change. (#205)

- **BREAKING behaviour:** NOTIFY validates registrations before calling the
  backend or closing the current one. Unpaired MessageNew/MessageExpunge,
  FlagChange or AnnotationChange without both, two selected specifiers,
  non-message events for the selected mailbox, fetch attributes outside it,
  empty lists and a misplaced STATUS indicator are BAD; events the framework
  cannot deliver are `NO [BADEVENT (MessageNew MessageExpunge FlagChange)]`,
  and a backend's BADEVENT list is completed the same way (RFC 5465 sections
  3.1, 5, 6.1). Backends now receive only legal configurations.
  `NotifySession` documents both. No signature change. (#54, second
  independent review R6)
- **BREAKING behaviour:** the framework enforces NOTIFY's selected-mailbox
  policy. NOTIFY NONE, SELECTED NONE or an omitted selected group suppress
  other sessions' EXISTS, EXPUNGE/VANISHED, RECENT, FLAGS and FETCH;
  per-event groups suppress FlagChange alone. Suppressed removals stay in the
  client's sequence view and suppressed additions stay out of it; the
  client's own commands still report their effects. A NOTIFY SET returns what
  accumulated, SELECTED delivers removals immediately, overflow and
  UNAUTHENTICATE end the policy. Backends keep publishing every change.
  No signature change. (#54, second independent review R5)
- **BREAKING behaviour:** once any CONDSTORE enabling command has been issued,
  untagged FETCH from STORE, from FETCH with MODSEQ or CHANGEDSINCE or an
  implicit `\Seen`, and from other sessions carries UID (RFC 7162 section
  3.2.4); a FETCH asking for neither keeps its exemption. Selected NOTIFY
  FlagChange carries UID with or without CONDSTORE (RFC 5465 section 5.1).
  UIDONLY responses use RFC 9586's `* <uid> UIDFETCH` form. No signature
  change. (#54, second independent review R3)
- `StoreOptions.Silent` documents that a successful conditional STORE.SILENT
  reports MODSEQ for each stored message, which the framework supplies itself
  when the backend honoured Silent; no new backend obligation. `QResyncMailbox`
  documents that Resync runs after the ordinary SELECT responses and that its
  failure releases the selection. `Session.Rename` states that INBOX keeps its
  inferiors. No signature change. (#54, second independent review R1/R2/R7)

- **BREAKING:** remove `MessageLimitSession`, `MessageLimits` and
  `MessageLimitOptions`, and withdraw MESSAGELIMIT/SAVELIMIT advertisements.
  Their mailbox-size/retention meanings contradicted RFC 9738's per-command
  processing limits, which the framework did not enforce. Server support is
  deferred until the framework can enforce the RFC contract.
  (#54, independent review A1)
- **BREAKING:** correct the `QUOTA=RES-STORAGE` backend obligation introduced
  in #177: STATUS DELETED-STORAGE is in octets, not kibibytes. Memory now
  returns the sum of deleted messages' octet sizes. STORAGE quota usage
  remains in units of 1024 octets. (#54, third review A1)
- **BREAKING behaviour:** gate SAVEDBEFORE, SAVEDON and SAVEDSINCE on
  SAVEDATE. Reject unclassified date, size, interval, object-ID and MODSEQ
  metadata keys before backend calls or `NewSearchQuery` construction,
  enforcing the existing capability guarantee. STATUS RECENT and NOTIFY
  RECENT are unavailable in rev2. No signature change. (#54, B1/D4)
- **BREAKING:** remove `message.MatchOptions.SavedUIDs`, a field the
  framework's SEARCHRES normalization made unusable by backends.
  `message.Match` returns `ErrUnsupportedCriteria` for unresolved
  `imap.SearchSavedResult`, including nested and short-circuited criteria.
  The message package has not yet had a tagged release. (#54, B4)
- **BREAKING behaviour:** document `Session.Create`'s trailing hierarchy
  declaration: backends create the name without the final delimiter. An
  OBJECTID backend that kept the delimiter in the name now fails the
  post-CREATE MAILBOXID lookup. Memory and Maildir now do so, and
  the framework uses the backend's delimiter for the post-CREATE MAILBOXID
  lookup. Clarify `StatusOptions.Items` and `SessionUpdate.Status` contracts.
  No signature change. (#54, B2/B5)
- **BREAKING behaviour:** LIST (SUBSCRIBED RECURSIVEMATCH) suppresses
  redundant CHILDINFO, as RFC 5258 section 3.5 and example 9 do: a parent
  carries CHILDINFO only while a subscribed descendant is not itself
  returned, and a parent returned only for it is omitted. Applies to the
  framework's evaluation and memory's native one; `ListOptions.SelectRecursiveMatch`
  documents the same for witnessing backends. Maildir's RFC 9051 CREATE fix
  exposed the previous redundant response in imaptest's listext script.
  No signature change. (#54, third review)
- **BREAKING behaviour:** MULTISEARCH parses RFC 7377 source filters
  (`mailboxes`, `subtree`, `subtree-one`, `personal`, `subscribed`,
  `inboxes`, `selected`) and expands them through LIST and, for personal
  sources, `NamespaceSession` when available. The previous non-standard
  `IN (name …)` form receives BAD. Document on `MultiSearchSession` that
  `inboxes` uses RFC 5465's personal-namespace fallback; no new interface or
  method is required. No signature change. (#54, C9/D1)

- **BREAKING:** IMAP4REV2 now requires the backend-only `STATUS-DELETED`
  witness. Memory and Maildir supply it and answer STATUS DELETED. Backends
  without it no longer advertise or enable rev2. (#176)
- **BREAKING:** QUOTASET now requires at least one declared, unfiltered
  `QUOTA=RES-*` resource. Registered resources use `CapabilitySupport` witnesses;
  bare RFC 2087 QUOTA remains available without them. Memory declares STORAGE
  and MESSAGE and answers DELETED-STORAGE; backendtest checks the resource
  and STATUS obligations. (#177)
- **BREAKING:** remove the never-supplied `GetMetadataOptions.MaxSize` and
  `HasMaxSize` fields and dead backend filtering. The framework owns MAXSIZE
  and computes LONGENTRIES from unfiltered values at the requested depth. (#178)
- **BREAKING behaviour:** unknown or unadvertised STATUS items receive BAD
  before backend calls, including LIST-STATUS. NOTIFY responses use the same
  negotiated item set. (#179)
- Permit documented, forward-only Maildir format migrations in v1 minors,
  preserving identities and unknown fields. Every bump must be called out;
  downgrades require restoring a pre-migration backup. Align the format 3/4
  entries with this owner-approved policy. (#180)
- Extend the request-field binding gate to `AppendMessage`. Correct APPEND
  staging, QRESYNC validation, memory authentication/support and server option
  defaults documentation; name `NewSearchQuery`'s options parameter. (#181, #182)

- Maildir format 4 persists compact transaction changes between bounded full
  checkpoints, and indexes delivery directories with change timestamps.
  Mutations copy affected mailbox pointer slices instead of cloning the whole
  account through JSON. A durable transaction head detects lost change files;
  versions 1–3 and prepared journals migrate with identities and unknown fields
  preserved. Older readers refuse format 4; rollback requires a pre-migration
  backup. This follows the documented minor-release migration policy (#180).
  Public API and defaults unchanged.
  Measured scale comparisons are in `docs/MAILDIR-SCALE.md`. (#117)

- **BREAKING:** memory URLAUTH generation rejects missing/invalid access
  identifiers, wrong key owners, nonexistent mailboxes, malformed message URLs
  and expired requests with BAD. Valid generation preserves the exact signed
  rump bytes. Resolution enforces access identities and expiry, reads from the
  URL owner's account, and honors UIDVALIDITY, sections and partial ranges.
  Hex verifiers are compared in constant time after decoding. Mailbox keys bind
  to UIDVALIDITY and are removed on delete/rename, preventing old tokens from
  exposing replacement mailboxes. The backend grants no submit role. No exported
  signatures change; conformance fixtures now supply valid rump URLs. (#162)

- `CapabilitySupport` documents the complete backend witness catalogue:
  advertisement gates, the LIST-EXTENDED routing token, backend-only
  BINARY-FETCH, required interfaces, states and obligations. Clarify that a
  Session answer replaces the Backend answer even when false. A gate compares
  the catalogue against compiled descriptors, backend feature witnesses and
  static spoken tokens in both directions. No exported API change. (#134)

- **BREAKING:** IMAP4REV2 advertisement now requires `CapabilitySupport`'s
  backend-only BINARY-FETCH witness, independently of the full BINARY token.
  Backends without it lose the rev2 umbrella before the v1 freeze. Memory and
  Maildir witness the fetch half. `message.Message.OpenBinarySection` and
  guarded `message.BinarySectionOptions` share bounded-buffer CTE decoding,
  exact length measurement, text CRLF conversion and decoded partial ranges.
  BINARY fetches report partial origins, respect PEEK/Seen, reject containers
  and return UNKNOWN-CTE for unknown or malformed CTEs. Unsupported FETCH errors
  no longer expose Go type names. No existing exported signatures change. (#126)

- **BREAKING:** `Limits.MaxLiteralBytes` now defaults to 32 MiB instead of
  64 KiB, and non-positive `Limits.MaxCommands` means unlimited instead of
  selecting 10,000. Positive command limits send BYE before disconnecting.
  Negative `Limits.MaxConnections` and `MaxConnectionsPerIP` disable those
  connection limits; zero retains the existing defaults. Every `Limits` field
  documents its default and sentinel semantics. Resource-limit defaults may be
  retuned in minor releases, with a changelog entry; explicitly set positive
  values to retain application policy across upgrades. (#124)
- Oversized APPEND literals return `NO [TOOBIG]`, including later MULTIAPPEND
  and CATENATE parts. An oversized non-synchronising literal receives its
  refusal before the connection closes without reading its payload. STATUS
  and LIST-STATUS cap APPENDLIMIT at the framework's literal limit, preserving
  smaller backend limits without changing backend-owned maps. (#124)

- **BREAKING:** `URLAuthSession` now uses guarded `GenURLAuthOptions`,
  `URLFetchOptions` and `ResetURLAuthKeyOptions{Mechanisms}` in place of
  `URLAuthOptions`. `FetchURLAuth` returns guarded `*URLFetchResult` with owned
  `Body`, `Size` and `HasSize`; known-size bodies stream, unknown-size bodies
  spool to disk. Correct URLFETCH's URL/nstring wire pairs, accept multiple
  GENURLAUTH pairs and RESETKEY mechanisms, and withdraw unimplemented
  URLAUTH=BINARY. Memory supplies exact sizes, hexadecimal tokens and atomic
  mechanism validation for key resets. (#122)

- **BREAKING:** `Limits.MaxLiteralBytes` also bounds aggregate APPEND and
  URLFETCH payload bytes per command. CATENATE parts spool to a bounded temporary
  file, with later literals drained and later URLs skipped after refusal.
  Oversized resolved content returns NO [TOOBIG]; readers and temporary files
  are released on success, refusal, cancellation and framing failure. (#133)

- **BREAKING:** replace `imapserver.MetadataOptions` with guarded
  `GetMetadataOptions` and `SetMetadataOptions`, and update `MetadataSession`
  signatures and both backends. GETMETADATA retains its depth field; MAXSIZE is framework-owned;
  SETMETADATA has independent empty options. Document shared-command and future
  field naming contracts on `ACLOptions`, `QuotaOptions`, `LanguageOptions`
  and `ComparatorOptions`. (#131)

- **BREAKING:** `message.Message.OpenBodySection` now ends in guarded
  `*message.SectionOptions`. Nil preserves the existing stored-byte behaviour;
  retain the reader, size and error results. Update both backends, examples,
  tests and fuzzing, and remove the method's options-gate exemption. (#128)

- **BREAKING:** MULTIAPPEND now requires the new optional `MultiAppendSession`
  atomic batch interface, with guarded `AppendMessage` and `MultiAppendOptions`.
  It returns shared `imap.MultiAppendData`; memory commits all messages and
  publishes one update batch. Maildir and other backends without atomic support
  no longer advertise MULTIAPPEND. Stage complete APPEND commands in a temporary
  file before backend calls, including single-message commands, so malformed
  later input and zero-length messages leave the mailbox unchanged. Plain
  literals remain file streams rather than whole-message framework buffers.
  Add opt-in backend conformance for rollback, cancellation and update chains.
  (#121)
- Interop acceptance recognises current imaptest group and per-command
  summaries as well as the legacy summary. Nonzero failures fail the test even
  without an Error-prefixed complaint; the synchronising-literal tool exception
  remains specific. No exported API change. (#164)

- **BREAKING:** `SCRAMCredentials` implementations must also supply a separate
  `CapabilitySupport` witness for each AUTH=SCRAM-SHA-1, AUTH=SCRAM-SHA-256,
  AUTH=SCRAM-SHA-1-PLUS and AUTH=SCRAM-SHA-256-PLUS token they serve. Structural
  credential support alone no longer advertises mechanisms. Each -PLUS variant
  additionally requires TLS. Document this contract on both exported
  interfaces, update memory's witnesses and activate SCRAM options for
  plus-only advertisements. No exported signatures change. (#123)

- **BREAKING:** remove the unused `SelectedMailbox.Status` method before the
  server v1 freeze. `Session.Status` handles every STATUS request, including
  requests for the selected mailbox; its documentation now states that contract.
  Remove the redundant implementations and move the backend conformance check
  to `Session.Status`, retaining numeric STATUS item validation. (#119)

- **BREAKING (server v0.x):** `SessionUpdater.Push` queue overflow now ends the
  NOTIFY registration as required by RFC 5465, rather than continuing after
  dropping events. The framework closes the updater, calls
  `NotifySession.Notify` with nil updater/configuration and reports
  NOTIFICATIONOVERFLOW. The client can register again. Document per-mailbox
  coalescing and overflow on these exported methods; no signatures change.
  (#132)

- **BREAKING:** `backendtest.Controls` hooks now end in dedicated guarded
  options pointers. `ForceSelectFailureOptions.Enabled` replaces the positional
  boolean; nil options restore normal selection. Document the callback shape
  for future controls and verify restoration in the conformance suite. (#129)

- **Maildir format migration:** account state/journals migrate to version 3,
  preserving identities and unknown fields; older readers refuse its
  bounded-history semantics. Rollback requires a pre-migration backup; this
  follows the documented minor-release migration policy (#180). (#74)
- **BREAKING, Maildir resource policy:** `maildir.Options.MaxTombstones` defaults to
  10000 per mailbox with a durable QRESYNC checkpoint and conservative older
  requests. `MaxMetadataValueBytes` defaults to 65536 and `MaxMetadataEntries`
  to 1024 per mailbox/server scope, with atomic METADATA MAXSIZE/TOOMANY
  refusals. Existing values remain readable. (#74)
- **maildir:** add guarded `PollFailure`, `Backend.PollFailures`, and
  `Options.OnPollFailure` for current account/mailbox diagnostics, cancellation
  and callback ownership. `OnPollFailure` and `PollFailures` are the sole
  callback/accessor; remove the unreleased legacy poll-error surface. Add `Options.IsolateAccountErrors`
  to keep healthy accounts available without reprovisioning failed identities;
  the default continues to fail Open. Recover a missing journalled cur-to-cur
  source as a tombstone. Enforce public-import allowlists separately for
  production and tests. Avoid full-state clones on idle polls and quadratic
  MOVE retention scans; document the measured scale envelope. Private backend
  response-shaping helpers remain separate to avoid freezing unnecessary new
  public APIs; message parsing and matching already use imapserver/message.
  Public additions only; existing signatures are unchanged. (#74)

- **BREAKING, update-delivery behaviour:** `UpdateBatch` revisions and origin
  accounting remain ordered, but a withheld removal no longer blocks later
  additions and flag updates. Client sequence positions remain stable until
  removal announcements are permitted. Retained removals count toward
  `Limits.MaxQueuedUpdates` and `MaxQueuedUpdateBytes`; already delivered
  STORE updates do not accumulate. CONDSTORE completions carry a safe
  HIGHESTMODSEQ checkpoint while removals wait. No exported signature changes.
  (#73)

- **BREAKING:** remove `SelectSnapshot.Flags`, `PermanentFlags`, `ReadOnly`,
  `NumRecent`, `HighestModSeq` and `NoModSeq`; their fields in `Status` are the
  sole source for SELECT responses and the retained selection checkpoint.
  Document snapshot validation, copy retained flag slices, and migrate both
  backends and backendtest. No root-module API changes. (#120)

- **Server v1 preparation:** document backendtest's prospective compatibility
  policy: checks may enforce existing obligations, while new obligations need
  harness opt-in or a witnessed optional interface. Extend the configuration
  guard and options-parameter gates to public server subpackages and their
  constructors. Retain independent versions, targeting `imapserver/v1.0.0`
  after the full review and two compatible substantive minor releases. No
  exported signatures change. (#54)

- **BREAKING:** `AUTH=XOAUTH2` and `AUTH=OAUTHBEARER` now require separate
  `CapabilitySupport` witnesses on the Backend, in addition to TLS. Backends
  accepting bearer credentials must return true for each supported wire token
  to retain advertisement and AUTHENTICATE support. Password-only backends no
  longer offer unusable bearer mechanisms. No exported signatures changed. (#72)

- **BREAKING, maildir behaviour:** FETCH no longer serializes the whole request
  against other account operations. It releases the account lock for analysis
  and streaming, retaining one open message source until its writer returns.
  Concurrent STORE, rename, expunge and deliveries can proceed; later vanished
  UIDs are skipped and Close still waits for the whole FETCH. UID lookup between
  responses uses binary search. This relaxes the documented serialization
  guarantee before the v1 stability window. No exported signatures change. (#74)

#### Added

- `message.Message.EncodeNULParts` and `message.EncodeOptions` write a stored
  message with every NUL-bearing part re-encoded as base64, for backends that
  accept BINARY APPEND (RFC 3516 section 4.4). Other bytes are copied unchanged,
  and a message that needs no change is not written. Additive. (#205)

- SORT and UID SORT accept the PARTIAL return option under CONTEXT=SORT
  (RFC 5267 section 4.4, as RFC 9394 extends it). The window is taken from
  the sorted order and reported as `PARTIAL (<range> <set>)`, or `NIL` when
  it holds nothing; negative ranges count from the end. A malformed range is
  refused with BAD. As for SEARCH (#206), PARTIAL together with ALL reports
  both, and a repeated PARTIAL uses the last range. ESORT results
  write ascending runs as ranges, `1:5` rather than `1,2,3,4,5`, and spell
  out every other order (RFC 5267 section 3.2). Framework-owned; no exported
  API or backend contract change. (#205)

- `imapserver.MultiSearchMailboxResult.ModSeqs` optionally pairs each matching
  UID with its modification sequence, so ESEARCH can report MODSEQ for MIN,
  MAX, PARTIAL or the whole result (RFC 7162 section 3.1.5, RFC 4731 section
  3.2). Memory supplies it. Additive. (#54, pre-fourth review)

- `imapserver.Options.OnError`, guarded `ServerEvent` and open string
  `ServerEventKind` expose backend failures, connection limits/timeouts,
  accept/TLS errors and recovered connection panics. Callbacks run serially
  outside connection loops with a bounded queue and reported drop counts.
  A callback may initiate shutdown; shutdown does not wait for callbacks.
  Backend causes remain private on the wire. Additive exported API. (#139)

- `imapserver.UpdateRecent{NumRecent}` reports a selection-specific absolute
  RECENT count through the existing extensible update vocabulary. Memory and
  Maildir claim new messages for one read-write selection; EXAMINE leaves
  pending claims intact. Maildir persists pending/claimed state across reopen,
  and treats old records without that state conservatively as recent. FETCH,
  STORE and SEARCH use the selection's flags; rev2 omits all RECENT wire data.
  Additive; existing backend interfaces are unchanged. (#160)

- `imapserver.CapabilityFilter` and guarded `CapabilityFilterOptions` let an
  opted-in Backend or Session withdraw backend-backed capabilities without
  changing legacy `CapabilitySupport` semantics. Session policy takes precedence;
  withdrawal also prunes dependents and the IMAP4REV2 umbrella. Command gates
  enforce server-only metadata scope, and backendtest skips withdrawn optional
  behaviours. Remove Maildir's inert spoken structural witnesses. Additive. (#137)

- `imapserver.AppendOptions.Size` and `HasSize` expose the ordinary APPEND
  literal's declared octet count before the backend reads it, including a
  known zero size. CATENATE leaves size unknown. Backends must still handle
  short or failing readers. Additive. (#138)

- `imapserver.FetchOptions` embeds `MutationOptions` so backends can identify
  implicit \Seen updates with the FETCH command's origin token. Memory and
  Maildir preserve it in their update batches. Updates from older backends
  that leave the origin at zero remain supported. Additive. (#136)

- `imapserver/maildir`: a persistent backend built through the public server
  and message APIs. Exports `Open`, concrete `Backend`, guarded `Options`,
  `AccountOptions` and `CloseOptions`, plus `Backend.Authenticate`,
  `SupportsCapability`, `Close` and `PollFailures`. It provides account
  isolation, OS root locking, versioned journal recovery, durable UID identity,
  flags/keywords, subscriptions, metadata and QRESYNC history, and polling of
  external Maildir delivery. APPEND preserves bytes; external LF delivery is
  normalized once to CRLF. Supports MOVE, CONDSTORE and QRESYNC without
  importing the memory backend or internal packages. Additive. The interop
  harness adds the build-tagged `MaildirProfile` and drives foreign clients
  against both backends sequentially. Mailbox creation/rename rejects filesystem
  case and normalization aliases before preparing a transaction; reopening
  aliased state fails without merging identities. (#53)

- A runnable `imapserver/examples/maildir.go` server demonstrates opening a
  persistent account, polling diagnostics and ordered server/backend shutdown.
  The existing example compile gate covers it. `maildir.Open` now displays
  unnamed results in its public signature; its types are unchanged. (#140)

- UIDBATCHES (RFC 10022) is now advertised for every backend and computed
  from the framework's selection UID list, without a backend interface or
  capability witness. Responses preserve descending batch order, support
  paging and UIDONLY, and leave SEARCHRES unchanged. Invalid requests receive
  TOOFEW, TOOMANY or CLIENTBUG; identical repeats receive LIMIT until at least
  half a batch was added or expunged. Different sizes/ranges and reselections
  are deliberately allowed. No exported API change. The server release must
  require the published root release containing the shared vocabulary. (#51)
- `imapserver.NewSearchQuery` and guarded `SearchQueryOptions` let backend
  authors construct validated UID-only queries for direct calls. The constructor
  copies criteria and UID sets and rejects unresolved, unknown or excessively
  nested keys. `backendtest` checks baseline and witnessed extension SEARCH keys
  and uses explicit ALL queries for SORT/THREAD; nil queries are not required
  backend inputs. Memory SEARCH now supplies save dates and EMAILIDs to its
  evaluator, fixing the regressions exposed by the conformance checks. (#130)

- `imapserver/message` provides the supported public message-analysis API:
  `Analyze`, concrete `Message`, `AnalyzeOptions`, `Metadata`, `MatchOptions`,
  `Match`, `ErrUnsupportedCriteria` and `ErrUnsupportedCharset`. It generates
  ENVELOPE/BODYSTRUCTURE, streams exact BODY sections with filters/partials,
  and evaluates SEARCH with explicit mailbox metadata and charset options.
  Calls take contexts, preserve source I/O errors and never own the source.
  Envelope/structure accessors return independent copies. The memory backend,
  including extension paths, now uses this package rather than internal
  message helpers. An AST guard enforces that boundary. Additive. (#53)
- `backendtest.Run` checks RECURSIVEMATCH combined with REMOTE and
  SPECIAL-USE, including CHILDINFO from a subscribed descendant without a
  special-use attribute. Remote descendants are checked when the harness
  seeds a remote child beneath an unsubscribed parent; otherwise that case
  explicitly skips. Backends without the LIST-EXTENDED witness are checked
  for discovery of the harness names through `LIST "" "*"`, which the
  framework's evaluation needs. No exported API change. (#34)

#### Fixed

- THREAD responses follow the RFC 5256 section 5 grammar. A single reply
  continues its parent's number run, `(1 2)` rather than `(1 (2))`, and
  sibling and top-level thread-lists are adjacent, `(2 (7)(12))` and
  `(1)(2)`, rather than separated by SP. `imapclient` rejected the old form as
  an invalid server response. Compatible wire correction; no exported API or
  backend contract change. (#205)

- The STATUS responses a NOTIFY SET STATUS writes before its tagged OK carry
  HIGHESTMODSEQ when FlagChange or AnnotationChange is registered and the server
  advertises CONDSTORE or QRESYNC, whether or not CONDSTORE is enabled, as RFC
  5465 section 3 requires. Event STATUS responses keep the section 5.2 rule
  and include it only once CONDSTORE is enabled. No exported API change. (#205)

- APPEND and REPLACE answer NO [TRYCREATE] when the destination does not
  exist, even if staging refused the command first, for example over a
  CATENATE URL. RFC 3501 section 6.3.11 requires TRYCREATE for a missing
  destination; the framework now asks `Session.Status` with no items whether
  the destination exists before reporting such a refusal. Any answer other
  than NONEXISTENT or TRYCREATE keeps the original response. No exported API
  change and no new backend obligation. (#205)

- memory: CATENATE URLs that inherit the account (relative, message-only and
  absolute without userinfo) resolve for logins containing `@` or `:`, such as
  e-mail addresses. The resolver wrote the login into the URL's userinfo
  unescaped, gave the authority a second `@` and refused every such URL with
  NO [BADURL]. No exported API change. (#205)

- memory: SORT and THREAD ORDEREDSUBJECT follow RFC 5256 and RFC 5957
  exactly. The base subject is computed by the full section 2.1 procedure,
  including `[blob]` prefixes, `Re[2]:`-style leaders and `[fwd: ...]`
  wrappers. FROM, TO and CC compare the first address's mailbox alone, and
  DISPLAYFROM and DISPLAYTO fall back to mailbox@host, then mailbox. String
  keys are compared under i;ascii-casemap, which folds to upper case, so
  punctuation such as `[` and `_` now sorts before letters. No exported API
  change. (#205)

- Narrow memory's CATENATE account-inheritance documentation to absolute URLs
  without userinfo. Mechanism-only userinfo remains refused: NO [BADURL], or
  a plain NO when the URL cannot be echoed (#204).
  Resolver and APPEND regressions preserve that boundary. Documentation/test
  correction; no exported API or runtime behaviour change. (#54, H-3, #207)

- UID FETCH with CHANGEDSINCE and VANISHED retains wildcard ranges when the
  mailbox is empty, so `1:*` reports the retained removal history. Repeated,
  reversed and mixed ranges and PARTIAL retain the existing history semantics.
  Compatible correction; no exported API or backend contract change. (#54, H-1)
- Document the existing acceptance of repeated SEARCH ALL return options and
  PARTIAL with ALL (#206), and memory's same-account CATENATE URL host/port
  aliases (#207). Preserve those behaviours. State the struct-comparability
  compatibility rule and clarify which resource budgets are implemented by
  Limits and which remain application/backend policy or design goals.
  Documentation only; no exported API, default or input-acceptance change. (#54, O-2/O-3)

- UID FETCH with CHANGEDSINCE and VANISHED reports historical removals through
  `QResyncMailbox.Resync` as VANISHED (EARLIER) before FETCH responses, including
  requests naming only removed UIDs. PARTIAL limits surviving FETCH results
  without narrowing the history report. VANISHED requires the UID form and
  advertised, enabled QRESYNC; NOMODSEQ remains refused. Resync errors fail
  UID FETCH without abandoning the selection. No exported API change. (#54, G-3)
- STORE checks UIDONLY before a read-only selection, so sequence-number STORE
  on EXAMINE receives BAD [UIDREQUIRED]; UID STORE keeps NO [READ-ONLY].
  No exported API change. (#54, G-2)

- REPLACE shares APPEND's staged payload parser, supports CATENATE and BINARY
  literal8, and commits only after the complete command terminator. Malformed
  REPLACE, LIST and LSUB lines cannot escape their parser; REPLACE read timeouts
  send BYE before closing. BINARY also accepts synchronising and non-synchronising
  literal8 APPEND and MULTIAPPEND. No exported API change. (#54, S-01, S-04, S-05, S-10)
- UID FETCH accepts RFC 9394's PARTIAL modifier, including negative and reversed
  ranges. The framework windows existing UIDs before applying CHANGEDSINCE,
  independent of modifier order. An oversized LITERAL- APPEND-family literal
  returns BAD [TOOBIG]. A failed SAVE search returning NO clears `$`; BAD
  preserves it. No exported API change. (#54, S-07–S-09)
- Memory resolves CATENATE relative and absolute URLs with account ownership,
  UIDVALIDITY, BODY sections and byte ranges validated, using streaming readers
  that retain cancellation. `backendtest.Run` checks relative section content
  and BINARY APPEND under their existing advertised obligations; no new backend
  duty or exported API change. (#199, #54)

- When `UpdateFlags.ModSeq` is zero, the framework fetches current FLAGS and
  MODSEQ together for CONDSTORE clients, skipping UIDs already removed. STORE's
  fallback also refreshes FLAGS and MODSEQ from one fetch. Queued flag changes
  keep the checkpoint fenced. The exported optional-zero contract remains
  valid; no new backend obligation. (#54, final review)
- SEARCH clock fallback, UIDONLY projection and keyword announcement compare
  all FETCH item spellings case-insensitively. Enforce FetchWriter's existing
  exactly-one-nonzero-UID rule across spellings, preventing random sequence
  mappings from conflicting UID keys. Authenticated read timeouts send BYE.
  UNAUTHENTICATE accepts an optional final DEFLATE block after sync flush.
  No exported API change. (#54, review N-3 and teardown/timeout follow-ups)
- Coalesce repeated UID requests before backend FETCH and in both reference
  backends' results. Legal repeated items no longer produce NO [SERVERBUG] under
  the strict single-UID validation; conflicting backend UID values still fail.
  (#54, interop follow-up)
- Retain backend ConnInfo.TLS and SCRAM-PLUS channel binding through the
  buffered transport installed by UNAUTHENTICATE after COMPRESS. Cover
  implicit TLS and STARTTLS, TLS 1.2/1.3, default mechanism selection and both
  SCRAM-PLUS variants across repeated resets. Handler-owned APPEND literal
  and IDLE reads, and incomplete unknown commands, also send BYE on transport
  read timeout before closing. No exported API change. (#54, Z-1/Z-4)
- LOGIN and AUTHENTICATE preserve commands buffered behind authentication,
  including SASL continuation and UNAUTHENTICATE/re-authentication pipelines.
  UNAUTHENTICATE errors send BYE instead of the forbidden NO; success removes
  COMPRESS after its OK and closes the session with a fresh bounded cleanup
  context. Correct the earlier refusal-preserves-selection claim. (#54, B-3,
  D-1/D-2/D-8)
- Enforce `CapabilityFilter` for MOVE and UID MOVE. FETCH result keys are
  explicitly case-insensitive in `imap.FetchMessageData` and `FetchWriter`;
  backendtest accepts those keys and diagnoses missing/malformed values
  without indexing or type-assertion panics. Its RENAME INBOX removal-clock
  check requires QRESYNC rather than CONDSTORE alone. (#54, B-4/B-5/D-6)
- A failed drain-time FLAGS/MODSEQ fallback sends BYE and reports its cause.
  Rejected Serve/ServeConn connections cancel their child contexts. Selected
  STATUS lookups use the requested mailbox name, independent of the snapshot's
  optional descriptive name. CLOSE emits neither own nor foreign removals;
  CHANGEDSINCE 0 receives BAD. URLFETCH backend errors complete response framing
  and use the normal error mapping instead of silently becoming NIL. No new
  exported signatures. (#54, D-3/D-4/D-7/D-9/D-10/D-11)
- Cancelling a Serve/ServeConn context interrupts transport reads and writes,
  including an initial greeting blocked on an unread connection. The parent
  context closes the transport without waiting for the write timeout, while
  internal cancellation retains its terminal-response path. (#54, final review)
- `backendtest` accepts an empty initial `MailboxRevision`, which the
  framework already supports; revisions are opaque comparable values with
  no reserved zero value. Clarify that contract and correct `NamespaceSession`
  documentation: an absent interface withholds NAMESPACE, with no framework
  default. No exported signature change. (#54, final review)
- Concurrent `Server.Close` callers all wake when a connection finishes,
  using a broadcast under the active-connection lock. A single-consumer signal
  could leave another caller waiting until its context expired even after all
  `ServeConn` calls had returned. No exported API change. (#54, final review)
- Conditional STORE fills missing MODSEQ for rejected messages whose FLAGS
  the backend reports, as well as successfully stored messages. No new backend
  obligation. (#54, final review)
- Live `UpdateVanished` batches use the client's negotiated removal form:
  EXPUNGE for ordinary sessions and VANISHED after QRESYNC or UIDONLY. UIDs
  never announced by EXISTS are omitted, and `Earlier` is normalized to live
  removal semantics; SELECT/FETCH resync reports alone use EARLIER. The
  exported field remains source-compatible and its documentation is corrected.
  (#54, review N1)
- Selected NOTIFY retains the latest withheld flags per UID and returns them
  on successful SET, including SET with NONE. Resync checkpoints remain behind
  withheld flags and removals, including after an own silent STORE of the
  same UID. Document selected policy, refused replacement registrations,
  own additions and operations on removed placeholders. (#54, N2/L1)
- EXPUNGE, UID EXPUNGE and MOVE report HIGHESTMODSEQ after QRESYNC. The
  framework uses the existing selected-mailbox STATUS contract before its
  final update drain, then fences the checkpoint if changes remain withheld.
  The synchronous-publication duty is now explicit; see the pre-v1 contract
  correction below. No new backend interface. (#54, N3)
- STORE fills missing MODSEQ in a FLAGS-only backend response through FETCH
  after CONDSTORE is enabled, merging it into that response before accounting
  for the own update. The exported Store contract still permits FLAGS-only
  output. (#54, N4)
- A completed CLOSE, UNSELECT or UNAUTHENTICATE answers OK after cleanup
  errors, reports those errors through `Options.OnError`, and leaves the
  connection usable in its new state. Replacing SELECT continues after a
  release error. A failed UNAUTHENTICATE ends the connection with BYE. Document
  once-only release, fresh bounded cleanup contexts and error handling on
  `SelectedMailbox.Unselect` and `Session.Close`. Cleanup diagnostics include
  protocol errors and cancellation, which ordinary command error reporting
  treats as expected refusals. (#54, N5)
- `backendtest`'s RENAME INBOX case accepts arbitrary FETCH order, a recreated
  INBOX with new UIDVALIDITY and MAILBOXID, and checks clocks and resync only
  within one incarnation. Clarify unique, unordered `SearchResult.UIDs`.
  (#54, N6/O1)
- Memory assigns a new SAVEDATE on COPY, MOVE and RENAME INBOX, preserving
  INTERNALDATE (RFC 8514 section 3). Both backend package docs describe the
  refusal of a RENAME INBOX destination beneath INBOX. (#54, N7/L1)

- A SELECT or EXAMINE failing in QRESYNC's Resync or changed-message FETCH, or
  in its OBJECTID or rev2 LIST response, releases the installed selection
  before answering: the connection returns to the authenticated state, the
  updater is closed and Unselect runs once on a fresh bounded context. The
  tagged response keeps the original error's mapping; a failed Unselect goes
  to `Options.OnError`. (#54, second independent review R1)
- A successful conditional STORE.SILENT returns MODSEQ (and UID) for each
  stored message instead of nothing. Only responses actually written are
  accounted against the command's own flag updates, so a change the backend
  did not report still reaches the client. (#54, second independent review R2)
- `backendtest` accepts APPEND, COPY and MOVE results reported on some calls
  but not others without UIDPLUS, checks every reported value against its own
  message, accepts CATENATE's nil-reader refusal, checks CONDSTORE, QRESYNC,
  REPLACE, SORT and THREAD only when witnessed, takes URLAUTH fixture UIDs and
  LIST hierarchy delimiters from the backend, and passes NOTIFY only legal
  registrations. Strict UIDPLUS, per-family SCRAM and zero `HighestModSeq`
  behaviour is unchanged. A new baseline case covers RENAME INBOX.
  (#54, second independent review R4)
- Memory implements RENAME INBOX: messages move to a new mailbox identity
  with new UIDs, keeping flags, INTERNALDATE and content; INBOX, its inferiors and
  identity remain; INBOX selections see the removals, QRESYNC reports them,
  and a destination conflict changes nothing. Memory MOVE now records its
  source removals for QRESYNC and advances the source clock (RFC 7162 section
  3.2). In both backends a message moved by RENAME INBOX is recent in its new
  mailbox, as a COPY or MOVE destination is. (#54, second independent
  review R7)

- CONDSTORE activation routes backend failures through the normal error mapping,
  preserving protocol response codes and keeping private diagnostics off the
  wire. Its extra STATUS lookup uses the command context and timeout.
  (#54, independent review A2/B1)
- `backendtest` derives fixture UIDs when UIDPLUS is unwitnessed, requires
  APPEND/COPY/REPLACE assigned results only for UIDPLUS witnesses, tests only
  witnessed SCRAM families, and accepts the documented optional zero
  `CondStoreResult.HighestModSeq`. (#54, independent review B2)
- Memory NOTIFY matches the union of requested events, honors NONE and
  SUBSCRIBED, and publishes message additions, expunges and flag changes from
  every mutation. Selected mailboxes use their selected updater and receive no
  NOTIFY STATUS; the STATUS indicator reports only watched message events.
  Unsupported LIST/METADATA events receive `NO [BADEVENT (MessageNew
  MessageExpunge FlagChange)]`. Legal MessageNew fetch-attribute lists are parsed
  and refused with `NO [CANNOT]` before backend calls.
  (#54, independent review B3)
- Memory FETCH prepares all results before applying implicit `\Seen` changes,
  preventing an error on a later message from leaving unpublished changes.
  (#54, independent review B4)
- APPEND stops resolving CATENATE URLs after refusal and bounds each assembled
  message by the command's remaining aggregate byte budget.
  (#54, independent review B5)
- Memory RENAME moves inferiors atomically, refuses destination conflicts and
  moves into its own hierarchy, and preserves the whole hierarchy on refusal.
  `backendtest` now checks inferior renaming. (#54, independent review B6)

- MULTISEARCH accepts the result options of extended SEARCH: MIN, MAX, ALL,
  COUNT, PARTIAL and CONTEXT per mailbox (RFC 7377 section 2.1), SAVE only
  with the selected mailbox as the sole source, and CONTEXT=SEARCH UPDATE for
  the selected mailbox, with MAILBOX and UIDVALIDITY on its updates (section
  2.2). Results are sorted, deduplicated and bounded like SEARCH's. The
  MAILBOX correlator uses the negotiated mailbox encoding instead of a raw
  string. No signature change beyond the additive `ModSeqs`. (#54,
  pre-fourth review)
- After ENABLE UTF8=ACCEPT or IMAP4rev2, responses carry UTF-8 mailbox names;
  only the command decoder switched before, so LIST, STATUS and others still
  sent modified UTF-7. SEARCH and MULTISEARCH with a CHARSET, and SORT or
  THREAD with a non-UTF-8 charset, receive BAD after ENABLE UTF8=ACCEPT
  (RFC 9755 section 3). No signature change. (#54, pre-fourth review)
- STATUS (HIGHESTMODSEQ), LIST-STATUS with HIGHESTMODSEQ and ESEARCH with a
  MODSEQ key now enable CONDSTORE, as RFC 7162 section 3.1 lists them, and the
  first reports the selected mailbox's HIGHESTMODSEQ. A NOMODSEQ selection
  refuses ESEARCH MODSEQ only when that mailbox is searched. NOTIFY STATUS
  carries HIGHESTMODSEQ only once CONDSTORE is enabled (RFC 5465 section 5);
  `SessionUpdate.Status` documents this. MULTISEARCH source expansion lists the
  hierarchy at most once per kind, is bounded like LIST (NO [LIMIT]), and keeps
  shared and other-users namespaces out of `personal`. `IN (selected)` is
  treated like an absent source list, so `$` and sequence numbers resolve
  there too (RFC 7377 section 2.2). No signature change. (#54, fourth review)
- Memory's SORT, THREAD and MULTISEARCH evaluate with the same save dates,
  EMAILIDs and selection flags as SEARCH. No exported API change. (#54,
  pre-fourth review)

- SEARCHRES substitutes `NOT ALL` for unset or empty saved results, so
  `SearchQuery.Criteria` does not impose a new empty-UID-set convention on
  existing backends. No signature change. (#54, B3)
- Memory reports ACL creation right `k`, expands legacy `c`/`d` rights on
  SETACL, and includes their aliases in ACL/MYRIGHTS replies. NOTIFY STATUS
  includes HIGHESTMODSEQ when available. Unlimited quota resources are
  omitted, explicit zero limits remain visible, and SETQUOTA validates and
  replaces the entire limit set atomically. No exported API change.
  (#54, D2/D3/D5)
- NOTIFY STATUS responses pushed while the backend handles NOTIFY SET now
  precede its tagged OK, as RFC 5465 section 3 requires for the STATUS
  indicator; `NotifyConfig.StatusOnSet` documents the timing. Queued NOTIFY
  events are also delivered at the start of every command, so a NOOP no
  longer completes ahead of them. No signature change. (#54, third review)

- Resolve SEARCHRES `$` to the selection's saved UIDs at every depth before
  SEARCH, SORT, THREAD and selected-source ESEARCH reach a backend; unset and
  empty saved results match nothing. ESEARCH with IN refuses `$`, including
  nested occurrences. Direct query construction rejects unresolved references.
  Add conformance and loopback regressions. (#183)
- Send untagged CLOSED before replacing a selection, including failed
  reselects, while CLOSE and UNSELECT keep their ordinary responses. (#184)
- With OBJECTID, include MAILBOXID on successful CREATE and SELECT/EXAMINE
  through the existing STATUS contract. A failed SELECT identity lookup leaves
  no selection; a failed post-CREATE lookup returns NO [SERVERBUG] stating
  explicitly that creation succeeded. No exported API additions. (#185)
- Advertise RIGHTS=texk alongside ACL and withdraw it when ACL is filtered.
  Use protocol names in search/FETCH refusals and neutral size-limit wording
  for APPEND and other content. (#186, #187)

- The Maildir backend compiles on 32-bit Linux again, where syscall time
  fields are int32. An INTERNALDATE outside that range is refused as
  unrepresentable rather than truncated. CI now type-checks both modules for
  linux/386 and linux/arm. (#174)

- Resolve UID SEARCH wildcards against the selected maximum, normalize
  reversed PARTIAL windows, and report the first CONDSTORE activation clock.
  Flag MODSEQ searches may use the supported single message-wide clock.
  Correct nested message/rfc822 and multipart/digest section interpretation.
  Restore modified UTF-7 LIST matching, LSUB hierarchy entries and
  same-mailbox MOVE. CONTEXT=SEARCH refreshes additions and flag changes, and
  reports removals using pre-expunge sequence numbers. The interop ID corpus
  uses an explicitly configured 64 KiB line allowance; production defaults
  are unchanged. (#165)

- **imapserver:** accept standalone FETCH items, including body sections and
  modifiers, through the shared item parser. Both reference backends return a
  hierarchy delimiter/root response for empty LIST patterns. Add baseline
  conformance and parser/loopback regressions. No exported signatures change.
  (#159)

- **imapserver:** reject wildcard QRESYNC sets, non-ascending sequence-match
  data and unequal sequence/UID counts with BAD before SELECT or Resync reaches
  a backend, including on UIDVALIDITY mismatch. Validation counts ranges
  without expanding them. No exported API change. (#118)

- **imapserver:** Unsolicited flag updates include MODSEQ only after CONDSTORE is enabled for the receiving session. No exported API change. (#88)

- **imapserver:** unsupported SEARCH charsets return NO [BADCHARSET] with the
  message helper's supported charset list, including in empty memory mailboxes.
  `message.ErrUnsupportedCharset` remains identifiable through errors.Is;
  `message.Match` now returns an *imap.Error wrapping it. Document the backend
  error contract and client-visible versus private error fields in the package
  doc. No exported signatures change. (#135)

- **imapserver:** GETMETADATA MAXSIZE filters values in the framework and
  reports the largest omitted size with the required METADATA LONGENTRIES
  response code. MAXSIZE 0 retains empty values; DEPTH and server/mailbox
  scopes remain intact. Document that MAXSIZE is applied by the framework.
  No exported signatures change. (#54)

- **imapserver:** wait for the reader to settle its pending-input flag before
  executing a handed-off command. A fast UID SEARCH could otherwise complete
  while its own handoff looked like pipelined input, withholding an EXPUNGE
  past the command's legal delivery point. The wait is cancelable; actual
  pipelines still defer unsolicited removals. No exported API change. (#73)

- **imapserver:** Consecutive additions and expunges use compact storage within
  atomic update batches, avoiding queue overflow caused by per-message object
  overhead during bulk delivery and removal. EXPUNGE ordering, revision/origin
  accounting, and aggregate queue limits remain enforced. Typed-nil update
  entries also count toward the retained-byte bound. No exported API change. (#75)

- **imapserver:** NOMODSEQ mailboxes refuse MODSEQ, CHANGEDSINCE and UNCHANGEDSINCE before backend access; selecting a clock-tracking mailbox restores support. No exported API change. (#91)

- **imapserver:** Plain STORE reports MODIFIED in message sequence numbers; UID STORE continues to report UIDs, including after expunges. No exported API change. (#89)

- **imapserver:** SAVE-only SEARCH updates the saved result without emitting SEARCH/ESEARCH data, in UID, rev1 and rev2 sessions. No exported API change. (#90)

- **imapserver/maildir:** Preflight RENAME hierarchy collisions before preparing a journal, preserving account reopenability after refusal. No exported API change. (#92)

- **imapserver/maildir:** Fresh UIDVALIDITY allocators start at current Unix time; persisted allocator values remain unchanged. No exported API change. (#93)

- **imapserver/maildir:** Resume interrupted initial setup and clean unpublished atomic-write temporaries while preserving registered-identity checks. No exported API change. (#95)

- **imapserver/maildir:** Skip invalid or unreadable new/ deliveries in place, preserving valid ingestion and reopenability with observable diagnostics. No exported API change. (#96)

- **imapserver/maildir:** Enforce the existing 256 MiB state-file budget on
  writes as well as reads. Oversized journals are refused with LIMIT before
  preparation, preserving account usability and recovery; oversized files are
  rejected rather than silently truncated when read. No exported API change.
  (#74)

- **imapserver/maildir:** Convert INTERNALDATE without UnixNano overflow and refuse dates the filesystem cannot represent. No exported API change. (#94)

- CONDSTORE SEARCH now evaluates message modification sequences and reports
  the matching result clock in SEARCH/ESEARCH, including nested criteria,
  rev2 responses and MIN/MAX-only results. A MODSEQ criterion enables
  CONDSTORE for the session. Uses the existing backend SEARCH/FETCH contract;
  no exported API changes. (#66)

- An unsolicited EXPUNGE or VANISHED is no longer sent after the tagged
  response of FETCH, STORE, SEARCH, SORT or THREAD. A command now stops being
  in progress at its tagged response, as RFC 3501 §7.4.1 defines it, so a
  client that sends its next sequence-numbered command immediately no longer
  loses synchronisation. A withheld removal is delivered during the next
  command that permits one: before the tagged response of the UID forms of
  those commands (RFC 9051 §7.5.1), during IDLE, NOOP, CHECK and other
  non-sequence commands. The plain forms still never carry one. No exported
  API changes. (#69)

- `imapserver/maildir`: FETCH validates requested sections for every targeted
  message before committing implicit `\Seen` flags. Unsupported sections,
  missing MIME parts and invalid partials leave flags, MODSEQ and message
  filenames unchanged. Successful FETCH still publishes one update batch and
  streams message bodies. No exported API change. (#53)
- **imapserver tests:** isolate the suite temporary namespace so FETCH spool leak checks cannot see other package processes' files (#70).
- **imapserver:** report REPLACE removals as EXPUNGE or VANISHED before command completion instead of suppressing them (#76).
- **imapserver:** consume REPLACE literal terminators on success and refusal, and honor UID REPLACE's reader barrier (#77).
- **imapserver:** reject invalid METADATA entry names with BAD before calling the backend (#78).
- **imapserver:** evaluate MODSEQ in memory SORT, THREAD and MULTISEARCH, and append the highest matching MODSEQ to nonempty legacy SORT results (#79).

## [1.3.0] - 2026-10-21

### Root module — `github.com/kiliant/go-imap`

#### Added

- `imap.Address.SourceRoute` preserves the obsolete RFC 822 source-route
  field in ENVELOPE decoding and encoding. Additive; no existing signature
  changes. Stored routed addresses and addresses with comments in the local
  part also remain searchable by their normalized mailbox. (#165)

- `imap.UIDBatchesData`, `UIDBatch`, `UIDBatchIndexRange`, response codes
  `CodeTooFew` and `CodeTooMany`, and `imapclient.Client.UIDBatches` with
  client-owned `UIDBatchesData` and guarded `UIDBatchesOptions`: RFC 10022
  client support. Requests validate batch size and range limits before I/O;
  exactly one correlated response is required, including an empty result.
  Pipelined replies are collected by tag. Supports UIDONLY, leaves SEARCHRES
  unchanged, and has no fallback. Additive. The server direction ships in
  the next `imapserver` minor. (#51)

- `imapclient.UnilateralDataHandler.Metadata`, shared `imap.MetadataChange`
  and its `imapclient.MetadataChange` alias deliver names-only METADATA
  notifications, including server metadata when the mailbox is empty.
  Callbacks receive changed entry names, separately from GETMETADATA's NIL,
  empty and present values, and may retain the data. Notifications interleaved
  with GETMETADATA or LIST-METADATA stay separate from command results. A nil
  handler still consumes and validates them. Additive. (#44)
- `imapclient.UnilateralDataHandler.ContextUpdate`, the shared types
  `imap.ContextUpdate`, `imap.ContextUpdateItem` and
  `imap.ContextUpdateChange`, the alias `imapclient.ContextUpdate`, the return
  keys `imap.ESearchReturnKeyAddTo` and `imap.ESearchReturnKeyRemoveFrom` with
  their `imapclient` mirrors, and `imapclient.ESearchData.ContextTag`: CONTEXT
  updates (RFC 5267 section 4.3) are delivered. The unsolicited ESEARCH
  responses carrying ADDTO and REMOVEFROM were discarded, or claimed by
  another pending extended SEARCH or SORT, which then failed. They are now routed by the original command's tag
  before any command sees them. Each update keeps every return item in wire
  order, including repeated and unknown keys. Items carry their raw value,
  and ADDTO and REMOVEFROM also carry typed position/set pairs in UID or
  sequence-number mode. `ContextTag` names the context for
  `Client.CancelUpdate`, also from the blocking `SortExtended`. CancelUpdate
  now documents when a registration ends. Additive. (#44)

- `imapclient.CodeNoCompatibleAuthMechanism` identifies automatic SASL
  selection exhaustion as a local `*imap.Error` with `ErrorTypeProtocol`,
  before writing to the connection. `imapcli` now uses this discriminator
  for its LOGIN fallback instead of copying the library's password-mechanism
  list. Server refusals, privacy and channel-binding failures retain their
  separate errors and do not trigger fallback. Additive. (#21)

#### Changed

- `imapclient.Client.SortExtended` and `SortExtendedUID` accept RETURN
  (UPDATE) when the server advertises CONTEXT=SORT and refuse it with
  `ErrCapabilityNotAdvertised` otherwise. Before, they always refused it,
  because updates were not delivered. `SearchExtended` and `MultiSearch` now
  refuse RETURN (UPDATE) without CONTEXT=SEARCH instead of sending it. Without
  ESEARCH, `SearchExtended` used to emulate the search and drop UPDATE
  silently. A MULTISEARCH with UPDATE requires a selected mailbox (RFC 7377
  section 2.2). RETURN (UPDATE) is never emulated and is always sent in the
  extended form. Every other return option keeps the documented fallback when
  ESEARCH or ESORT is absent, even where CONTEXT=SEARCH or CONTEXT=SORT is
  advertised, because RFC 5267 does not make those imply MIN, MAX, ALL or
  COUNT. No signature change. (#44)

#### Fixed

- Guard shared command lookahead when a transport read fails after a complete
  keyword. Authenticated server connections now return the read error instead
  of recovering an internal bounds panic. No exported API or backend contract
  change; publish the root correction before the server module. (#54)

- The shared server-direction SEARCH parser accepts `UID $`, including nested
  criteria, as the saved SEARCHRES result. This also repairs SORT, THREAD and
  ESEARCH criteria and IMAP4rev2's incorporated syntax. Compatible correction;
  no exported API change. Ship this root fix before the server release. (#54, H-2)

- `imapclient.Client.Unauthenticate` ends an active COMPRESS layer at the
  successful completion, preserves TLS and prefetched plaintext, and prevents
  concurrent writes across the framing change. Shared internal boundary handling
  accepts sync flush, a final block, or both. REPLACE emulation compares UID keys
  without regard to case and rejects ambiguous UID values. Compatible corrections;
  no exported API change. Ship in the root release preceding the server's
  UNAUTHENTICATE teardown correction. (#54, RT-3/RT-4)

- Root `imap.StatusItemDeletedStorage` documents octets, matching RFC 9208
  and the server contract, rather than kibibytes. Documentation only. (#54)

- `imapclient.Client.Notify` writes `NOTIFY SET STATUS (group) (group)`:
  STATUS follows SET, and each watch group has its own parentheses (RFC 5465
  section 8). Previously STATUS and multiple groups were rejected by compliant
  servers. No exported signature change. (#197)

- **imapcodec:** the server-direction UIDFETCH response is written as RFC 9586
  section 7's `* <uid> UIDFETCH (...)`, not `* UIDFETCH <uid> (...)`. Only
  `imapserver` writes it; a standalone server build receives the fix with the
  next root release. No exported API change. (#54)

- `imapclient.MultiSearchMailboxes` encodes multiple mailbox names as a
  parenthesized list, as required by RFC 5465 and RFC 7377. Correct the
  `MultiSearchInboxes` documentation to describe delivery destinations and
  the personal-namespace fallback. No signature change. (#54, third review)

- **imapcodec:** accept atom, quoted and literal astring arguments for SEARCH
  string keys and HEADER values, allowing commands such as `SEARCH FROM alice`.
  No exported API change. (#127)

- **imapclient tests:** Stream fuzz targets confirm reader completion before
  reporting an expired deadline, avoiding false shutdown failures when both
  channels are ready after scheduling delays. Readers still running when a
  timeout is observed fail; the nightly timeout input is retained in the corpus.
  (#106)

- **imapclient:** publish immutable streaming FETCH portions, preserving later fields and bounded memory; adapt backup to combine portions and discard duplicate bodies without leaking temporary files (#64).
- **imapclient:** accept RFC 7162 legacy SEARCH and SORT MODSEQ suffixes without changing number results or exported APIs (#67).

- `imapclient.Client.SearchPartial` and `SearchPartialUID` now register
  RETURN (UPDATE) contexts, expose their tag in `ESearchData.ContextTag`, and
  deliver their updates through `UnilateralDataHandler.ContextUpdate`.
  UPDATE requires CONTEXT=SEARCH and is refused locally without it. Previously
  these methods sent UPDATE but discarded every update. No signature change.
  (#48)

- Preserve sequence-set wildcards in generic response values, including
  ESEARCH ALL and CONTEXT ADDTO/REMOVEFROM. Discard recoverably malformed
  ESEARCH responses that no command or live context owns, including unknown
  correlator labels, instead of closing the connection. Pending searches and
  live contexts still reject malformed replies; fatal decoder errors still
  close the connection. Fixes a regression since decode-once routing.
  No exported API change. (#49)

- Decode MULTISEARCH MAILBOX correlators using the negotiated mailbox
  encoding and normalise INBOX, consistently with LIST and STATUS. Both
  `imap.MultiSearchResult.Mailbox` and `imap.ContextUpdate.Mailbox` now contain
  decoded mailbox names rather than modified UTF-7 wire spellings.
  No signature change. (#50)
- Decode STATUS, LIST and ESEARCH/METADATA response data once before command
  collection. STATUS results stay with their requested mailbox, including
  reversed pipelined replies; unrelated STATUS events reach the existing
  callback. Legacy LSUB no longer consumes unsolicited LIST events. Names-only
  METADATA notifications no longer fail GETMETADATA or LIST-METADATA, and
  another mailbox's metadata values do not leak into GETMETADATA results.
  No exported API change in the response decoding itself.
  (#44)

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

[Unreleased]: https://github.com/kiliant/go-imap/compare/v1.3.0...HEAD
[1.3.0]: https://github.com/kiliant/go-imap/releases/tag/v1.3.0
[1.2.1]: https://github.com/kiliant/go-imap/releases/tag/v1.2.1
[imapserver/v0.3.0]: https://github.com/kiliant/go-imap/releases/tag/imapserver/v0.3.0
[1.2.0]: https://github.com/kiliant/go-imap/releases/tag/v1.2.0
[imapserver/v0.2.0]: https://github.com/kiliant/go-imap/releases/tag/imapserver/v0.2.0
[1.1.0]: https://github.com/kiliant/go-imap/releases/tag/v1.1.0
[imapserver/v0.1.0]: https://github.com/kiliant/go-imap/releases/tag/imapserver/v0.1.0
[1.0.0]: https://github.com/kiliant/go-imap/releases/tag/v1.0.0
