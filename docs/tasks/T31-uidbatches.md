# T31 — UIDBATCHES (RFC 10022) in both directions

**Issue:** [#51](https://github.com/kiliant/go-imap/issues/51)

**Milestone:** post-M6.
- **One release for both directions.** The root module's next minor after
  T30's `v1.3.0` (expected `v1.4.0`) carries the vocabulary, codec and client,
  and the next server minor (expected `imapserver/v0.4.0`) carries the
  server. Both are tagged the same day, root first (`docs/RELEASING.md`).
- **Why together.** Our own server may be the only implementation the client
  can be tested against before its API freezes.
- **Why not in `v1.3.0`.** T30 does not wait for T31.

**Depends on:** completed T30, whose decoded-response routing the client
collector uses.

## Why this task exists

UIDBATCHES was published as RFC 10022, Standards Track, on 2026-07-31, and the
IANA IMAP Capabilities registry references it. `docs/RFC-COVERAGE.md` was
retrieved from the registry on the same day and kept UIDBATCHES on its watch
list as a draft, so no task covered it. A registry recheck on 2026-10-15 found
it. Apart from the deliberately deferred capabilities, it is the only
registered capability that neither direction implements.

UIDBATCHES returns UID ranges that split the selected mailbox into batches of
N messages, newest first, so that a client can fix batch boundaries in
advance. That matters most under UIDONLY (RFC 9586), where the client has no
sequence numbers to build batches from.

## Ownership and scope

New files, created and owned by this task:

- `uidbatches.go` (root package): shared vocabulary.
- `internal/imapcodec/uidbatches.go` and `uidbatches_test.go`: the command and
  response codec, in both directions.
- `imapclient/ext_d_uidbatches.go`, with `_test.go`, `_example_test.go` and
  `_interop_test.go`: the client command, its usage example and its live
  test.
- `imapserver/ext_d_uidbatches.go` and `_test.go`: the server command and its
  loopback test.
- Fuzz targets in new files:
  - `internal/imapcodec/uidbatches_fuzz_test.go`;
  - `imapclient/ext_d_uidbatches_fuzz_test.go`;
  - `imapserver/ext_d_uidbatches_fuzz_test.go`.

  Afterwards T13 owns the first two and T24 the third. Existing shared fuzz
  and loopback files are not edited.

Narrow handovers from completed tasks, named in their specs:

- **T03:** the private `commandOptions.collectorForTag` factory and its call
  after tag allocation in `Client.issue`. A correlated collector must capture
  its tag before registration and I/O; assigning a command pointer after
  `issue` returns races a fast server. No other connection-core changes.

- **T02:** the TOOFEW and TOOMANY response-code constants in `error.go`, and
  the new root file above. Nothing else in the root package.
- **T11:** the new `imapclient/ext_d_uidbatches*` files inside its `ext_d_*`
  pattern. No existing group D file.
- **T15:** this task's entries in `CHANGELOG.md`, for both modules.
- **T18:** the new `internal/imapcodec/uidbatches*` files only.
- **T19:** one field on `selectedState` in `imapserver/session.go`, which
  records the last request for the selection (step 2).
- **T23:** the new `imapserver/ext_d_uidbatches*` files inside its `ext_*`
  pattern, and the server cell of the new UIDBATCHES coverage row. No existing
  server extension file.
- **T25:** the commit that bumps `imapserver/go.mod` and `go.sum` to require
  the root release that contains the vocabulary, including the comment that
  names the floor. The precedent is `0d38190`, "build(imapserver): require
  root v1.2.0".
- **T26:** the client cell and footnote of the new UIDBATCHES row in
  `docs/RFC-COVERAGE.md` group D.
- **T30, once complete:** the UIDBATCHES case in `decodeData` and its field on
  `responseData`, both in `imapclient/response_data.go`. Nothing else in T30's
  routing.

**Not handed over:**
- `examples/**` and `api_surface_test.go` stay with T14. A blocking method
  needs no entry in its list of command types.
- `.github/**` stays with T15. Fuzz targets are discovered automatically.

**Out of scope:**
- automatic batch sizing from MESSAGELIMIT;
- use of UIDBATCHES in `cmd/imapcli`;
- any client fallback (see step 1).

## Protocol summary

From RFC 10022 sections 3 and 4, and RFC 9051's ABNF:

**Command.** `UIDBATCHES SP nz-number [SP nz-number ":" nz-number]`, valid in
the selected state. The first number is the batch size N. The optional range
names 1-based batch indices, and batch 1 holds the highest UIDs.

**Response.**
- The form is `* UIDBATCHES (TAG "t") 215295:99696,99695:20351,...`: a
  search-correlator, where `tag-string` is an `astring`, and a list of
  `uid-range` in descending order.
- A `uid-range` is `uniqueid ":" uniqueid`, two non-zero UIDs with no `*`,
  covering every UID between them "regardless of order". Only the list is
  ordered, not the two ends of a range.
- Exactly one response per command, and it is mandatory even when empty: for
  an empty mailbox, or a range beyond the last batch.
- Endpoints may name UIDs that do not exist, and the final batch may end at
  UID 1.

**Client obligations.**
- MUST request at least 500 messages per batch.
- MUST send a range low to high.
- MUST keep a range's span (batches × N) at or below 100,000 messages. This
  applies only when a range is given.
- MUST NOT resend except after selecting a different mailbox, or after more
  than N/2 messages have been expunged or added. Section 3.1.1 later says the
  client MAY ask again "once N/2 or more" new messages have arrived; see the
  thresholds below.

**Server obligations and options.**
- MUST NOT put more than N messages in a batch, and SHOULD stay within 90%
  of N.
- If N ≥ M, MUST return exactly one range spanning all messages, provided
  the mailbox is not empty and the request includes batch 1. An empty
  mailbox, or a range starting after batch 1, gets an empty response.
- MUST support spans of at least 100,000 messages.
- Answers:

  | Condition | Answer | Requirement |
  |---|---|---|
  | size below the server's minimum | `NO [TOOFEW]` | MUST |
  | reversed range | `BAD [CLIENTBUG]` | MUST |
  | span over 100,000 | `NO [TOOMANY]` | MAY |
  | cannot return all requested ranges | `NO [TOOMANY]` | MUST |
  | resend violation | `NO [LIMIT]` | SHOULD (tracking is MAY) |

**Interactions.** UIDBATCHES is valid under UIDONLY and never changes the
SEARCHRES `$` variable (section 3.4).

## Deliver in two reviewable steps

Both steps ship in one release. Step 1 can be reviewed and merged first.

### 1. Vocabulary, codec and client (the next root minor after T30)

**Vocabulary, in the root package.** Every struct is keyed-only, with
`_ struct{}`.

```go
type UIDBatchesData struct {
	Tag     string     // the correlator
	Batches []UIDBatch // in wire order
	_       struct{}
}

type UIDBatch struct {
	Range UIDRange // both endpoints non-zero, never "*"; order as sent
	_     struct{}
}

type UIDBatchIndexRange struct { // the requested batch indices
	First, Last uint32 // zero means absent; set both or neither
	_           struct{}
}
```

- `UIDBatch` is a struct rather than a bare range so that per-batch data (a
  later CONDSTORE addition, for example) is a new field, not a parallel slice.
- `UIDBatchIndexRange` follows the `PartialRange` precedent and leaves room
  for a later form that counts from the oldest batch. The name says "index":
  `UIDBatch.Range` is already a range of UIDs, and the two must not be
  confused.
- `CodeTooFew` and `CodeTooMany` go on the open `ResponseCode` type.
  `CodeTooMany` is the top-level code RFC 10022 registered, for any command.
  Its doc says it is distinct from METADATA's `[METADATA TOOMANY]`, which is
  `CodeMetadata` with the argument TOOMANY.

**Codec.**
- Write every range as `start:stop`, both endpoints present, never through
  `NumRange`/`NumSet` `String` (which shortens `5:5` to `5` and writes 0 as
  `*`), and refuse a zero endpoint.
- Read leniently: accept a bare `uniqueid` as `u:u`. Reject 0 and `*`.
- The server writes ranges high:low, as the RFC's examples do.

**Client shape.** A blocking method. UIDBATCHES never touches `$`, so a
command handle like `ESearchCommand` would buy nothing.

```go
// imapclient
type UIDBatchesData struct {
	imap.UIDBatchesData
	_ struct{}
}

type UIDBatchesOptions struct {
	Indices imap.UIDBatchIndexRange // zero value: every batch
	_       struct{}
}

func (c *Client) UIDBatches(ctx context.Context, size uint32,
	options *UIDBatchesOptions) (*UIDBatchesData, error)
```

- The result is a client-owned wrapper, not an alias, following the
  `ESearchData`, `SortData` and `IDData` precedents. Observations only a
  client can make then have somewhere to go without touching the shared type.
  Examples are an `Emulated` flag, or batches shrinking because EXPUNGE
  arrived during the command (section 3.1.3.3.3).
- The size is positional, like every required argument in this API, and is a
  `uint32`, like `MessageLimitData.Limit`. A nil options pointer means every
  batch.

**Local refusals.** Each one is an `*imap.Error` with `ErrorTypeProtocol` and
the code below, and writes nothing to the wire:

| Condition | Code |
|---|---|
| size below 500 | `CodeTooFew` |
| zero, reversed or half-set range | `CodeClientBug` |
| span above 100,000, computed in 64 bits (only when a range is given) | `CodeTooMany` |
| missing capability | wraps `ErrCapabilityNotAdvertised` |

**No fallback in this release.** Appendix A.2 suggests a client-side UID
SEARCH fallback. It is not offered:
- under UIDONLY it is impossible, because it needs sequence numbers;
- outside UIDONLY it needs a message count the client does not keep, because
  EXISTS is only forwarded to the handler.

A later opt-in would be an options field, off by default, plus `Emulated` on
the wrapper. Both are additive.

**Collection** through T30's decoded-response path:
- A collector claims only its own correlator, so other commands' untagged
  data and a pipelined second UIDBATCHES are never misattributed.
- Exactly one response per command. A second one is a protocol error, as in
  `esearchCollector`.
- A response without TAG cannot be attributed and fails the connection. A
  well-formed response with a foreign tag is discarded, like other
  unsolicited data.
- `tag-string` is accepted as atom, quoted string or literal.
- After a tagged OK, a missing response is a protocol error. After NO or BAD,
  the server's `*imap.Error` is returned.
- Ranges are delivered in wire order and are not validated for order.

**Documentation.**
- The resend rule is the caller's obligation, in its strict form: resend only
  after a different selection, or after more than N/2 messages were added or
  expunged. The client keeps no counters, and a server may refuse with
  `NO [LIMIT]`.
- The MESSAGELIMIT interaction (section 3.2): request no more than
  `MessageLimit`.
- Batches can be smaller than requested and can name UIDs that do not exist.
- The command is allowed under UIDONLY.

### 2. Server (the next server minor, same day, after the root tag)

**Advertisement** is decided by the framework alone, like UIDONLY. There is
no `CapabilitySupport` token and no entry in `rev2Incorporated`. A future
backend fast path, which is Appendix A.1's motivation, must be a routing
token under `../API-STABILITY.md` §10, never a gate, because a gate would
silently drop UIDBATCHES from every released backend. The changelog states
that every backend advertises UIDBATCHES from that release on.

**Batches** are computed from the framework's selection UID slice; there is
no backend surface.
- For M messages, batch i covers sequence positions M-(i-1)N down to
  max(1, M-iN+1).
- Each batch is written high:low, as the UIDs at its two positions. The final
  batch ends at UID 1, which section 3.1.4 allows.
- If N ≥ M, there is exactly one range, when M > 0 and the request includes
  batch 1.
- Compute positions and the span in 64 bits. Check for a range beyond the end
  before indexing the slice. Iterate only over batches that exist, because
  `UIDBATCHES 500 4294967295:4294967295` is valid input.

**Errors.**
- A size below 500 is refused with `NO [TOOFEW]`.
- A reversed range is refused with `BAD [CLIENTBUG]`.
- A range spanning more than 100,000 messages is refused with `NO [TOOMANY]`.
  The RFC permits this, and it keeps the client limit testable against our
  own server.
- A request without a range is answered in full. The selection is bounded by
  `Limits.MaxSelectedMessages`, and the answer holds at most
  `MaxSelectedMessages/500` ranges.

**Resend rule**, as approved for this task:
- Record the last request on the selection: size, range, message count and
  highest UID.
- Refuse an identical repeat with `NO [LIMIT]` unless, since that request, at
  least N/2 messages were added (UIDs above the recorded highest) or at least
  N/2 were expunged (derived from the counts). The threshold is "N/2 or
  more", so that a client following the RFC's MAY is never refused.
- A request with a different range is allowed: that is section 3.1.5's
  paging. A request with a different size is allowed by this task's approved
  policy, not by the RFC, and the doc comment says so.
- A new selection starts afresh, including a reselection of the same mailbox.
  The RFC names only "a different mailbox", so this is deliberate leniency.
- The file's doc comment records the trade-off. Alternating requests
  sidestep the rule, which is accepted because one computation is O(M/N)
  index arithmetic over the slice. A stricter policy can come later on v0.x.

**Interactions.** `$` stays unchanged, and UIDONLY needs no special case.

**Release.** The require bump to the new root release (the T25 handover)
lands before the server tag. RELEASING's standalone-build gate checks it.

## API and implementation constraints

- Additive only on the frozen root and client API. Review the public names
  before merge. `apidiff` must report no incompatibilities.
- No exported interfaces, no new error type, and no external dependencies.
- The codec stays bidirectional, with round-trip tests for the command and the
  response.
- Hostile input must not panic. Internal codecs return errors, and the client
  boundary returns `*imap.Error`.

## Done when

**Codec.** Round trips and malformed-input tests for the command and the
response, including:
- `5:5` written as `5:5`;
- a zero endpoint refused;
- a bare `uniqueid` read as a range;
- `*` and 0 rejected.

Fuzz targets in the three new files.

**Client scripted tests.**
- Full and ranged requests, and empty responses.
- Each local refusal, with its code and nothing written.
- UIDONLY enabled.
- TOOFEW, TOOMANY and LIMIT from the server, surfacing as `*imap.Error` codes.
- Interleaving with other commands' untagged data, and a pipelined second
  UIDBATCHES.
- A duplicate response, a response without TAG, and a literal tag.

**Server tests.**
- Exact boundaries for M divisible and not divisible by N.
- N ≥ M giving exactly one range (M > 0, batch 1 requested), an empty
  response for an empty mailbox or a later range, and the final batch ending
  at 1.
- Ranges beyond the end, and hostile indices.
- Each error code.
- The resend rule: a refused repeat; repeats allowed after N/2 additions and
  after N/2 expunges; a different range allowed; a reselection allowed.
- UIDONLY, and `$` unchanged.
- A loopback test against the real client.

**Live interop.**
- First probe the matrix for UIDBATCHES, and record on the issue which servers
  advertise it.
- Live client tests in `ext_d_uidbatches_interop_test.go` run on those servers
  and skip elsewhere, naming the capability.
- Coverage becomes `verified` only after two independent servers pass; the
  project's own server does not count. If none advertises UIDBATCHES, the
  coverage footnote says the behavior is covered by scripted and loopback
  tests only.

**Validation and docs.**
- Race tests pass in both modules, and the require bump passes the
  standalone-build gate.
- The coverage row, its footnote, the usage example and the changelog entries
  for both modules reflect what was delivered.

## Protocol references

- [RFC 10022](https://www.rfc-editor.org/rfc/rfc10022.html):
  - sections 3–4: the command, the response, batch sizes, ranges, limits, and
    the MESSAGELIMIT, UIDONLY and SEARCHRES interactions;
  - Appendix A: the UID SEARCH equivalence, and the fallback it suggests;
  - [Erratum 9141](https://www.rfc-editor.org/errata/eid9141), editorial and
    held for document update: Appendix A.1's "batch count" means batch size.
- [RFC 9051 section 9](https://www.rfc-editor.org/rfc/rfc9051.html#section-9):
  `uid-range`, `uniqueid`, `search-correlator`, `tag-string`.
- [RFC 9586](https://www.rfc-editor.org/rfc/rfc9586.html): UIDONLY.
