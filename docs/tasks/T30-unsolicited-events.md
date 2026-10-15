# T30 — Unsolicited events and command-response routing

**Issue:** [#44](https://github.com/kiliant/go-imap/issues/44)

**Milestone:** post-M6, next additive root release (candidate `v1.3.0`).
**Depends on:** completed T27 and T29; execute after the root `v1.2.1` patch
release and backend conformance follow-up #34.

## Ownership and scope

T30 takes only the following completed work forward, under BOARD's lock-passing
rule. The originating specs name the same handover:

- T03: untagged response routing in `imapclient/conn.go` and the
  `UnilateralDataHandler` declaration/documentation in `imapclient/client.go`.
- T05: response collectors in `imapclient/{status,list}.go`; preserve mailbox
  encoding and the existing SELECT/EXAMINE ownership rules.
- T08 and T27: ESEARCH collection/correlation in `imapclient/ext_a_esearch.go`.
- T10 and T27: extended SORT collection in `imapclient/ext_c_sort.go` and
  MULTISEARCH routing in `imapclient/ext_c_multisearch.go`.
- T11 and T27: `imapclient/ext_d_metadata.go`, `imapclient/ext_e_context.go`,
  and event-delivery documentation in `imapclient/ext_d_notify.go`. T11 also
  hands over only `listMetadataCollector` in `imapclient/ext_d_acl.go`, the
  existing LIST-METADATA adapter that must share METADATA response decoding;
  all ACL and LIST-MYRIGHTS behavior remains outside T30.
- T02: additive shared response vocabulary in `metadata.go` and
  `searchdata.go`, only if needed by the reviewed event representation.
- T18: response decoding/encoding in `internal/imapcodec` only where needed
  for those event forms; retain bidirectional semantics.
- T15: root changelog entries for this task only. Release automation and
  unrelated release notes remain with T15.
- T29: the `UnilateralDataHandler.List` comment, while preserving T29's
  documented SELECT/EXAMINE ownership behavior.

Focused tests, any new client event-routing files, the affected coverage
footnotes, and this task's changelog entries are in scope. New parser paths
land with fuzz coverage; T13 retains ongoing fuzz/corpus ownership. No other
server, authentication, transport or FETCH behavior is transferred.

## The concrete failures

T27 records these gaps in `docs/RFC-COVERAGE.md`, footnotes `t27context` and
`t27notify`:

- CONTEXT updates are discarded without an active collector. With another
  extended SEARCH or SORT pending, that command can claim the update and fail.
  `SortExtended` therefore refuses RETURN (UPDATE).
- METADATA change notifications are discarded. The existing metadata parser
  expects entry/value pairs, whereas a notification carries entry names only.
- STATUS for a different mailbox is merged into the active STATUS result.
  A LIST event arriving during a listing joins that command's results.

The collector chain currently shares a decoder: a collector cannot consume the
correlator or mailbox and then return false, because the next collector would
start in the middle of the response. Fix ownership before adding callbacks.

## Deliver in three reviewable steps

### 1. Decode once and route by information actually present

Introduce a bounded decoded representation for the affected responses and
offer the parsed data to its command or event consumer. Do not rewrite the
streaming FETCH path or buffer arbitrary responses/literals to solve routing.

Preserve the existing response-count and decoder limits, mailbox
normalisation (only INBOX is case-insensitive), unknown extension data, and
command failure behavior. An unrelated STATUS must not overwrite another
mailbox's result. Keep plain SORT's existing pipelining permissions, and do
not relax the extended-search exclusion until routing is proven correct.

LIST has no command tag. Reference/pattern interpretation can be server
specific, and an event can be identical to a solicited result. Identify the
cases the wire can distinguish and document a conservative ownership rule
for the others; do not promise perfect attribution or add a slash-based local
matcher that changes legitimate listings. Preserve the documented rev2
SELECT/EXAMINE LIST ambiguity.

### 2. Deliver CONTEXT updates

Decode ADDTO/REMOVEFROM as ordered actions, preserving repeated keys, positions,
number order, correlator tag and UID-versus-sequence mode. A map alone loses
repeated update items. Model extension names as open values, not closed enums.

Expose updates through an additive handler on `UnilateralDataHandler`, and
make the originating SEARCH/SORT tag available for `CancelUpdate` (reuse the
existing `Command.Tag` where possible). A completed context's update must not
become another command's initial result. Define delivery during initial
completion, CANCELUPDATE, NOUPDATE, selection changes, logout and disconnect.
Only then enable extended SORT's RETURN (UPDATE), gated on CONTEXT=SORT; do
not emulate server-maintained updates when the capability is absent.

### 3. Deliver metadata changes

Add an optional metadata-change callback. Preserve notification entry names
without pretending that each carries a NIL value: names-only changes, empty
values and explicit NIL values are different states. The empty mailbox name
identifies server metadata. A notification during GETMETADATA must not be
parsed as that command's entry/value result, even for the same mailbox.

Callbacks follow the existing reader-goroutine contract: they must not block
or issue a command and wait for it. Retained data must not subsequently mutate.
Nil handlers still consume and validate responses without losing alignment.
No automatic GETMETADATA refresh, unbounded event queue or background worker.

## API and implementation constraints

- Additive only on the frozen root/client API. Review public names, lifecycle
  and ownership before implementation is merged; `apidiff` must report no
  incompatibilities. Shared wire vocabulary must be usable in both directions.
- Introduce no external dependencies or new methods on exported interfaces.
- Keep unknown responses safe and preserve existing callers' command results.
- Server CONTEXT evaluation, new protocol extensions and the v2 password-SASL
  default are out of scope. Split any newly discovered independent defects
  into their own fixes and tracked issues.

## Done when

- Scripted tests interleave old CONTEXT updates with a new extended SEARCH,
  extended SORT, MULTISEARCH and unrelated commands. Check repeated ordered
  actions, UID and sequence modes, no pending command, cancellation and
  NOUPDATE, then selection change and disconnect cleanup.
- STATUS tests use two distinct mailboxes and INBOX case variants; LIST tests
  cover distinguishable events and explicitly pin unavoidable ambiguity.
- Metadata tests cover names-only mailbox/server notifications alongside
  GETMETADATA, including empty and NIL values and nil callbacks.
- Race tests pass in both modules, new parser paths have fuzz targets/seeds,
  response limits still hold, and all API additions have been reviewed.
- Live interop runs in CI on supporting matrix servers. Skips name missing
  capabilities; coverage becomes verified only after two independent servers
  exercise the behavior. Scripted-only coverage is stated as such.
- Coverage footnotes, callback documentation, a usage example and the root
  changelog reflect delivered behavior. The task issue closes with the
  implementation PR, not with this plan.

## Hand-over to T31

Once this task is complete, [T31](T31-uidbatches.md) takes the UIDBATCHES case in
`imapclient/response_data.go`. The UIDBATCHES collector then uses the same
decode-once path and can decline a foreign correlator without advancing the
decoder. No other part of this task's routing passes.

## Protocol references

- [RFC 5267 sections 4.3–4.3.5](https://www.rfc-editor.org/rfc/rfc5267.html#section-4.3):
  context correlation, ordered updates, refusal and cancellation.
- [RFC 5464 sections 4.4–4.4.2](https://www.rfc-editor.org/rfc/rfc5464.html#section-4.4):
  value-bearing replies versus names-only notifications.
- [RFC 5465 section 5](https://www.rfc-editor.org/rfc/rfc5465.html#section-5):
  NOTIFY events and their response forms.
