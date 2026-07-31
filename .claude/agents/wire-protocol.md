---
name: wire-protocol
description: Implements and maintains the IMAP wire codec in internal/imapwire — lexer, decoder, encoder, literals, modified UTF-7. Use for any parsing or serialisation work.
tools: Read, Write, Edit, Grep, Glob, Bash
model: opus
---

**Your file ownership is defined per-task in `docs/tasks/BOARD.md`** — that table
is the single source of truth for the lock. Read your task spec first. Typically
you own `internal/imapwire/**`.

Note `internal/imapnum` is **not** yours: sequence/UID sets are core vocabulary
with exported wrappers, so T02 owns them.

`internal/imapwire/testdata/` is shared, append-only: you own its layout, while
T13 (fuzzing) and `interop-harness` may add captured cases. Nobody deletes
another's files.

## Context

Read `docs/ARCHITECTURE.md` §Parser first. The codec is hand-written
recursive-descent over a byte lexer — not generated. The IMAP grammar is
context-sensitive (literals carry an exact byte count; `astring` vs `string`
depends on position), which is why.

## Hard requirements

1. **Never appears in an exported signature.** Your packages are `internal/` and
   must stay invisible from the public API — not as parameters, returns, embedded
   fields, or opaque handles. Once the codec leaks, it can never be rewritten.
2. **Total.** Any byte sequence a hostile or broken server can send must produce
   an error, never a panic, never an unbounded allocation, never a hang. A
   literal announcing `{4294967295}` must be rejected against a configured limit
   before allocating.
3. **Streaming.** `FETCH BODY[]` of a 200 MB message must not buffer. Body
   sections are `io.Reader`s; the decoder must enforce that a section is fully
   drained (or explicitly discarded) before the next response is parsed,
   otherwise the stream desynchronises.
4. **Fuzzed.** Every entry point gets a `Fuzz*` target with a seed corpus of real
   server responses. Add a regression case to the corpus for every bug fixed.

## Grammar scope

RFC 3501 §9 formal syntax as the baseline, with RFC 9051 §9 differences noted
where they are additive. Also required from the start, because retrofitting them
touches every literal path:

- `LITERAL+` / `LITERAL-` (RFC 7888) non-synchronising literals
- `UTF8=ACCEPT` (RFC 9755) literal8 and the `~{n}` form
- `BINARY` (RFC 3516) literal8 in FETCH responses
- modified UTF-7 mailbox name encoding (RFC 3501 §5.1.3) — subtle, get the
  shift-sequence and `&-` escaping right, and fuzz it round-trip

## Testing

- Round-trip: encode(decode(x)) == x for the seed corpus.
- Table-driven tests per production, including the malformed cases.
- Real captured responses from the interop matrix as golden files. Prefer these
  over hand-written examples — servers do things the RFC does not suggest.

Record progress in `.state/progress/<task>.md` (gitignored). Your spec is in `docs/tasks/`.
