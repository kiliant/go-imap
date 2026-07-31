# T10 — Extensions group C: content & structure

**Agent:** `extensions` · **Milestone:** M3 · **Depends on:** T08

**Owns:** `imapclient/ext_c_*.go`

Runs in parallel with T11.

## Scope

| Capability | RFC | Notes |
|---|---|---|
| BINARY | 3516 | `BINARY[...]`, `BINARY.SIZE[...]`, `literal8` |
| CATENATE | 4469 | Server-side message assembly on `APPEND` |
| MULTIAPPEND | 3502 | Several messages in one `APPEND` |
| COMPRESS=DEFLATE | 4978 | Stream compression |
| UTF8=ACCEPT / ALL / APPEND / ONLY / USER | 9755, 5738 | **9755, not the obsoleted 6855** |
| SORT | 5256 | Server-side sorting |
| SORT=DISPLAY | 5957 | `DISPLAYFROM`, `DISPLAYTO` |
| THREAD | 5256 | `ORDEREDSUBJECT`, `REFERENCES` |
| MULTISEARCH | 7377 | Search across mailboxes |
| PARTIAL | 9394 | Paged `SEARCH`/`FETCH` results |
| SEARCH=FUZZY | 6203 | `FUZZY` search key |

## Notable difficulties

- **COMPRESS=DEFLATE** wraps the connection *after* the tagged `OK`, and both
  directions must be flushed correctly on every command boundary or the peer
  stalls waiting for bytes still sitting in the compressor. This is the single
  most deadlock-prone extension in IMAP. Test with `-race` and with a timeout on
  every read. Interaction with TLS is layering, not nesting: compress inside TLS.
- **BINARY** returns `literal8` and may fail with `UNKNOWN-CTE` for content the
  server cannot decode — fall back to `BODY[]` plus client-side decoding, and
  document that fallback.
- **CATENATE** composes from URLs (RFC 5092) and may need `URLAUTH` (T11).
  Coordinate — if you need `URLAUTH` before T11 lands, note it in your progress
  file rather than implementing it in T11's files.
- **UTF8=ACCEPT** changes literal syntax and mailbox-name encoding (raw UTF-8
  rather than modified UTF-7). The codec paths exist from T01; your job is
  negotiation and correct selection between them.
- **SORT/THREAD** fallbacks: client-side sorting is possible but requires fetching
  the sort keys for the whole mailbox. Implement it, and document the cost
  honestly so callers can decide.
- **PARTIAL** (9394) paginates large result sets. Verify the RFC number — it is
  *not* the older `CONTEXT=SEARCH` paging from 5267.

## Done when

Each verified against two servers where available. COMPRESS tested under `-race`
with a large fetch, asserting no deadlock. `UTF8=ACCEPT` tested with a non-ASCII
mailbox name and a non-ASCII search. Coverage rows updated.
