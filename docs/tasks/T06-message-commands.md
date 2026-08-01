# T06 — Message commands

**Agent:** `client-core` · **Milestone:** M1 · **Depends on:** T03

**Owns:** `imapclient/fetch.go`, `store.go`, `search.go`, `append.go`, `copy.go`

Runs in parallel with T04 and T05.

## Commands

`FETCH`, `UID FETCH`, `STORE`, `UID STORE`, `SEARCH`, `UID SEARCH`, `COPY`,
`UID COPY`, `APPEND`, `EXPUNGE`.

## FETCH is the hardest thing in this library

- Items use the open-ended T02 `FetchItem` type. Do not add a bool-field struct.
- **Streaming.** `BODY[]` of a large message is an `io.Reader`, not a `[]byte`.
  The API must make it *hard* to accidentally buffer 200 MiB. Sections must be
  drained in order; an undrained section desynchronises the stream (T01 enforces
  this — surface it as a clear error, not a hang).
- Section specifiers in full: `BODY[]`, `BODY[HEADER]`, `BODY[TEXT]`,
  `BODY[1.2]`, `BODY[HEADER.FIELDS (From To)]`, `BODY[HEADER.FIELDS.NOT (...)]`,
  `BODY[1.MIME]`, plus the `BODY[]<partial>` byte-range form and `BODY.PEEK[...]`.
  `BODY.PEEK` vs `BODY` is a `\Seen` side effect — make the distinction
  impossible to miss in the API.
- Responses arrive **out of order** and a single message may span several
  untagged `FETCH` responses. Do not assume one response per message.
- Unsolicited `FETCH` responses (flag updates for other messages) arrive mid-
  command and must route to the unilateral handler, not into the caller's result.

## STORE

`+FLAGS`, `-FLAGS`, `FLAGS`, each with a `.SILENT` variant. Note that servers may
send flag updates even for `.SILENT` — handle both.

## SEARCH

- `SearchCriteria` from T02; composable `AND`/`OR`/`NOT`.
- `CHARSET` handling: report what the server supports rather than transcoding
  silently. Non-ASCII search on a server without `UTF8=ACCEPT` needs an explicit
  charset.
- Design for `ESEARCH` (T08) now: reserve an extensible return-options path on
  SEARCH rather than a closed enum. T08 shipped typed return options on the
  sibling `SearchExtended` entry point (Guardian-approved); the earlier
  `[]string` reservation on base `SearchOptions` was removed rather than left
  as dead API.

## APPEND

- Takes an `io.Reader` plus a length — messages are large.
- Optional flags and internal date.
- Uses non-synchronising literals (`LITERAL+`) when advertised; otherwise waits
  for the continuation request. Both paths need testing; servers differ.
- Design for `MULTIAPPEND` (T10) and `CATENATE` (T10) — the options struct should
  absorb them.

## UID vs sequence numbers

The distinction is in the type system (T02 `NumKind`), never a `bool` parameter.
Mixing them is *the* classic IMAP client bug, and it corrupts data silently.

## Done when

Fetch/store/search/append/copy round-trip against Dovecot and GreenMail,
including a 5 MiB message fetched without buffering (assert peak allocation),
`HEADER.FIELDS` parsing, and a `BODY.PEEK` that provably does not set `\Seen`.
