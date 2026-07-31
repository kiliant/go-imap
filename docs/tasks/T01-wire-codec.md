# T01 — Wire codec

**Agent:** `wire-protocol` · **Milestone:** M0 · **Depends on:** nothing

**Owns:** `internal/imapwire/**`

Runs in parallel with T02. Both must land before anything else starts.

## Goal

A total, streaming, hand-written codec for the IMAP wire grammar. This is the
foundation everything else sits on, and the one package that must never appear in
the public API — so it can be rewritten later without breaking anyone.

## Deliverables

### Layering rule: primitives only

**`internal/imapwire` must not import the root `imap` package.** It deals in
wire primitives — atoms, strings, literals, lists, numbers — and knows nothing
about envelopes or body structures.

Semantic decoding (wire → `imap.Envelope`, `imap.BodyStructure`, …) is built on
these primitives in `imapclient`, and lands with the task that owns the
corresponding types. This keeps the dependency graph acyclic and keeps T01 and
T02 genuinely parallel: neither needs the other's output to compile.

### Lexer / decoder (`decoder.go`, `chars.go`, `literal.go`, `resp.go`)

RFC 3501 §9 formal syntax as the baseline, plus the RFC 9051 §9 additions.
Primitive productions required:

- `atom`, `astring`, `string`, `quoted`, `literal`, `nstring`, `number`,
  `nz-number`, `uniqueid`
- parenthesised lists, arbitrarily nested (body structures nest without a
  specified bound — cap the depth, see limits below). Exposed as a list-reader
  the caller drives, so nesting is the caller's concern, not a fixed shape.
- `flag`, `flag-list`, `flag-perm`, including custom keywords and `\*` — as
  strings; the `imap.Flag` type is T02's
- `resp-text`, `resp-text-code`, `resp-cond-state`, `resp-cond-bye`
- `mailbox` (with modified-UTF-7 handling), `mbx-list-flags`
- `section`, `section-spec`, `section-part`, `section-msgtext` — including
  `HEADER.FIELDS (...)` and `HEADER.FIELDS.NOT (...)`
- `date-time`, `date` — returning `time.Time`
- response framing: read one complete response line, tagged/untagged/continuation

Deferred to the tasks owning the types (**not T01**): `envelope`, `address`,
`body`, `body-type-1part`, `body-type-mpart`, `body-ext-*` → T06;
`search-return-data` → T06/T08; `status-att-list` → T05.

### Encoder (`encoder.go`)

- Command serialisation with correct quoting: choose atom vs quoted vs literal by
  content, minimally.
- Literal handling with continuation-request synchronisation, and the
  non-synchronising `LITERAL+`/`LITERAL-` (RFC 7888) forms when enabled.
- `literal8` (`~{n}`) for `BINARY` (RFC 3516) and `UTF8=ACCEPT` (RFC 9755).
- Never emit a bare 8-bit byte in a quoted string; it must become a literal.

### Mailbox name encoding (`utf7.go`)

Modified UTF-7 per RFC 3501 §5.1.3. Subtle and frequently wrong — get the
shift-sequence handling and the `&-` escape right, and fuzz it round-trip. Under
`UTF8=ACCEPT` mailbox names are raw UTF-8 instead; both paths must exist and be
selectable.

### Streaming body sections

`FETCH BODY[]` on a 200 MB message must not buffer. Sections are exposed as
`io.Reader`. The decoder must enforce that a section is fully drained or
explicitly discarded before parsing the next response — otherwise the stream
desynchronises and later responses are attributed to the wrong command.

## Hard requirements

1. **Total.** Any byte sequence returns an error. No panic, no unbounded
   allocation, no hang. Includes integer overflow in literal-length arithmetic.
2. **Configurable limits**, checked *before* allocating:
   - max literal size (default 100 MB)
   - max line length (default 8 KB for non-literal lines)
   - max list nesting depth (default 100)
   - max untagged responses buffered per command
   A literal announcing `{4294967295}` must be rejected, not attempted.
3. **Invisible.** Nothing in this package may become reachable from an exported
   signature — checked by T14's API surface test, but design for it now.
4. **Deadline-aware.** Every read observes a deadline; a server that opens a
   literal and stalls must time out.

## Testing

- Table-driven tests per production, malformed cases included.
- Round-trip: `encode(decode(x)) == x` over the seed corpus.
- `FuzzDecoder`, `FuzzUTF7`, `FuzzDecoderBodySection` with a seed corpus in
  `testdata/`. Every bug fixed adds its input to the corpus. Semantic body
  structure decoding and its fuzzing belong to T06/T13.
- Golden files from real servers once T12 lands — prefer these over hand-written
  examples, because servers do things the RFC does not suggest.

## Done when

Fuzz targets run 5 minutes clean; `go test -race` passes; no exported symbol
references an unexported type in a way that could leak; the limits above are
enforced and unit-tested at their boundaries.
