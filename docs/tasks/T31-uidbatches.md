# T31 — UIDBATCHES (RFC 10022) in both directions

**Issue:** [#51](https://github.com/kiliant/go-imap/issues/51)

**Milestone:** post-M6. The vocabulary, codec and client ship in root
`v1.3.0` together with T30. The server ships in `imapserver/v0.4.0`, after the
root tag (`docs/RELEASING.md`).
**Depends on:** completed T30, whose decoded-response routing the client
collector uses.

## Why this task exists

UIDBATCHES was published as RFC 10022, Standards Track, on 2026-07-31, and the
IANA IMAP Capabilities registry references it. `docs/RFC-COVERAGE.md` was
retrieved from the registry on the same day and kept UIDBATCHES on its watch
list as a draft, so no task covered it. A registry recheck on 2026-10-15 found
it. Apart from the deliberately deferred capabilities, it is the only
registered capability that neither direction implements.

UIDBATCHES returns UID ranges that split the selected mailbox into batches of N
messages, newest first. Its main use is UIDONLY (RFC 9586), where a client has
no sequence numbers to build batches from.

## Ownership and scope

New files, created and owned by this task:

- `uidbatches.go` in the root package: shared vocabulary.
- `internal/imapcodec/uidbatches.go`: the command and response codec, in both
  directions.
- `imapclient/ext_d_uidbatches.go`: the client command.
- `imapserver/ext_d_uidbatches.go`: the server command.

Focused tests and examples colocated with those files follow BOARD's rule. New
parsers land with their fuzz targets. Afterwards T13 owns the client and codec
fuzz files, and T24 owns the server ones.

Narrow handovers from completed tasks, named in their specs:

- T02: the response-code constants for TOOFEW and TOOMANY in `error.go`, and
  the new root file above. Nothing else in the root package.
- T11: the new file `imapclient/ext_d_uidbatches.go` inside its `ext_d_*`
  pattern. No existing group D file is included.
- T15: this task's entries in `CHANGELOG.md`, for both modules.
- T18: the new file `internal/imapcodec/uidbatches.go` only.
- T19: one field on `selectedState` in `imapserver/session.go`, recording the
  last request for the selection (see step 2).
- T23: the new file `imapserver/ext_d_uidbatches.go` inside its `ext_*`
  pattern, and the server cell of the new UIDBATCHES coverage row. No existing
  server extension file is included.
- T26: the client cell and footnote of the new UIDBATCHES row in
  `docs/RFC-COVERAGE.md` group D, and its live test.
- T30, once complete: the UIDBATCHES case in `imapclient/response_data.go`.
  Nothing else in T30's routing.

Out of scope: emulation, automatic batch sizing from MESSAGELIMIT, and use of
UIDBATCHES in `cmd/imapcli`.

## Protocol summary

RFC 10022 sections 3 and 4:

- Command: `UIDBATCHES SP nz-number [SP nz-number ":" nz-number]`, valid in
  the selected state. The first number is the batch size N. The optional
  range names 1-based batch indices, and batch 1 holds the highest UIDs.
- Response: `* UIDBATCHES (TAG "t") 215295:99696,99695:20351,...`. It carries a
  search-correlator and a comma-separated `uid-range` list in descending order.
  It is mandatory even when empty, for an empty mailbox or a range beyond the
  last batch. Endpoints may name UIDs that do not exist, and the final batch
  may end at UID 1.
- The client MUST:
  - request at least 500 messages per batch;
  - send a batch range low to high;
  - keep a range's span (batches × N) at or below 100,000 messages;
  - resend only after selecting a different mailbox, or after more than N/2
    messages have been expunged or added.
- The server:
  - MUST NOT put more than N messages in a batch, and SHOULD stay within 90%
    of N;
  - answers a size below its minimum with `NO [TOOFEW]`, a reversed range
    with `BAD [CLIENTBUG]`, too many messages with `NO [TOOMANY]`, and resend
    violations with `NO [LIMIT]`;
  - MUST support spans of at least 100,000 messages.
- UIDBATCHES is valid under UIDONLY, which is its intended use. It never
  changes the SEARCHRES `$` variable (section 3.4).

## Deliver in two reviewable steps

### 1. Vocabulary, codec and client (root `v1.3.0`)

**Vocabulary.**
- A keyed-only response type with `_ struct{}`, for example
  `imap.UIDBatchesData`. It carries the correlator tag and the ranges in wire
  order, each with its endpoints as sent (the higher UID first). Do not
  normalise or re-sort them.
- `imap.CodeTooFew` and `imap.CodeTooMany` on the open `ResponseCode` type.
- The server must be able to write the response from the same type.

**Client.**
- One method, with `context.Context` first and the optional batch range in an
  options struct. The exact shape is decided in API review; a plausible one is
  `UIDBatches(ctx, size, *UIDBatchesOptions) (*UIDBatchesData, error)`.
- **Refuse locally, before anything reaches the wire:**
  - a missing capability, with an `imap.Error` wrapping
    `ErrCapabilityNotAdvertised`;
  - everything RFC 10022 forbids a client to send: a size below 500, a zero or
    reversed range, and a span above 100,000 messages. Compute the span
    without overflow.
- **No emulation.** Appendix A.1's UID SEARCH equivalent needs sequence
  numbers. UIDONLY forbids them, and computing them takes a message count the
  client does not track. A caller can issue that search itself; this follows
  the RETURN (SAVE) precedent of having no unfaithful fallback.
- **UIDONLY:** the command is allowed when UIDONLY is enabled.
- **Collection:** use T30's decoded-response path, so that a collector claims
  only its own correlator. A pipelined second UIDBATCHES and other commands'
  untagged data are then never misattributed.
  - A response without the mandatory TAG correlator, or with ranges that do
    not parse, is a protocol error.
  - A missing response is a protocol error at Wait; an empty response is an
    empty result.
  - Ranges are not validated for order. They are delivered as sent.
- **Resend rule:** document it as the caller's obligation. The client keeps no
  counters. It cannot attribute mailbox changes exactly (VANISHED (EARLIER),
  changes it has not seen yet), and the server enforces the rule with `LIMIT`,
  which arrives as an `*imap.Error` with `CodeLimit`.
- **Documentation:**
  - the MESSAGELIMIT interaction (RFC 10022 section 3.2: request no more than
    `MessageLimit`);
  - that batches can be smaller than requested and can name UIDs that do not
    exist.

### 2. Server (`imapserver/v0.4.0`, after the root tag)

**Capability.** Advertise UIDBATCHES and handle the command in the selected
state. There is no backend surface: the input is the framework's selection
UID slice, as for UIDONLY and CONTEXT.

**Batches.** For M messages, batch i covers sequence positions
M-(i-1)N down to max(1, M-iN+1). Report each batch as the UIDs at its two
positions, except that the final batch ends at UID 1, as section 3.1.4 allows.
A batch never holds more than N messages. A range beyond the end yields an
empty response.

**Errors.**
- A size below 500 is refused with `NO [TOOFEW]`.
- A reversed range is refused with `BAD [CLIENTBUG]`.
- A range spanning more than 100,000 messages is refused with `NO [TOOMANY]`.
- A request without a range is answered in full. The selection is already
  bounded by `Limits.MaxSelectedMessages`, and the answer holds at most
  `MaxSelectedMessages/500` ranges, so no new limit is needed.

**Resend rule.**
- Record the last request on the selection: size, range, message count and
  highest UID.
- Refuse an identical repeat with `NO [LIMIT]` unless, since that request,
  more than N/2 messages were added (UIDs above the recorded highest) or more
  than N/2 were expunged (derived from the counts).
- Derive both from the UID slice when the request arrives; no hook into
  update delivery is needed. A new selection starts afresh.
- A request with a different range is paging, which section 3.1.5 describes,
  and is allowed.
- Record the policy in the file's doc comment, because the RFC leaves "resend"
  undefined for a changed size or range.

**Interactions.** `$` stays unchanged, and UIDONLY needs no special case.

## API and implementation constraints

- Additive only on the frozen root and client API. Review the public names
  before merge. `apidiff` must report no incompatibilities.
- No exported interfaces, no new error type, and no external dependencies.
- The codec stays bidirectional, with round-trip tests for the command and the
  response.
- Hostile input must not panic. Internal codecs return errors, and the client
  boundary returns `*imap.Error`.

## Done when

- **Codec:** round trips and malformed-input tests for the command and the
  response. Fuzz targets for the codec, the client response parser and the
  server command parser.
- **Client scripted tests:**
  - full and ranged requests, empty responses, and the local refusals (size
    500, reversed range, 100,000 span, missing capability), each refused
    without writing;
  - UIDONLY enabled;
  - TOOFEW, TOOMANY and LIMIT surfacing as `*imap.Error` codes;
  - interleaving with other commands' untagged data, and a pipelined second
    UIDBATCHES.
- **Server tests:**
  - exact batch boundaries for M divisible and not divisible by N, the final
    batch ending at 1, and ranges beyond the end;
  - each error code;
  - the resend rule: a refused repeat, a repeat allowed after N/2 changes, a
    repeat allowed after reselecting, and a different range allowed;
  - UIDONLY, and `$` unchanged;
  - a loopback test against the real client.
- **Live interop:**
  - First, probe the matrix for UIDBATCHES and record which servers
    advertise it on the issue.
  - Live client tests then run on those servers and skip elsewhere, naming the
    capability.
  - Coverage becomes `verified` only after two independent servers pass. The
    project's own server does not count. If none advertises UIDBATCHES, the
    coverage footnote says the behavior is covered by scripted and loopback
    tests only.
- **Validation and docs:**
  - race tests pass in both modules;
  - the coverage row, its footnote, a usage example and the changelog entries
    for both modules reflect what was delivered.

## Protocol references

- [RFC 10022](https://www.rfc-editor.org/rfc/rfc10022.html), sections 3–4: the
  command, the response, batch sizes, ranges, limits, and the MESSAGELIMIT,
  UIDONLY and SEARCHRES interactions. Appendix A.1: the UID SEARCH equivalence.
  [Erratum 9141](https://www.rfc-editor.org/errata/eid9141), editorial, held
  for document update: Appendix A.1's "batch count" means batch size.
- [RFC 9051 section 9](https://www.rfc-editor.org/rfc/rfc9051.html#section-9):
  `uid-range` and `search-correlator`.
- [RFC 9586](https://www.rfc-editor.org/rfc/rfc9586.html): UIDONLY.
