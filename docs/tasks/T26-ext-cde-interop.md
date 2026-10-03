# T26 — Live interop for client extension groups C–E

**Agent:** `extensions` + `interop-harness` · **Milestone:** post-M6 · **Depends on:** T10, T11, T12

**Owns:** `imapclient/ext_cde_interop_test.go` (new), and the group C–E client
status cells and their footnotes in `docs/RFC-COVERAGE.md`

## Why this exists

T10's "Done when" reads *each verified against two servers where available*.
T10 and T11 shipped with scripted-server unit tests only: `imapclient` has no
interop test for any group C–E capability, which is why every client cell in
those groups reads `done` while groups A and B read `verified`. Nothing was
wrong with the code that anyone knows of — but a capability exercised only
against a server this project scripted has only been checked against this
project's own reading of the RFC, which is exactly what an interop matrix exists
to rule out.

## Capability census — probed 2026-10-03, post-login, default matrix

Capabilities in groups C–E with at least two independent advertisers, which
can therefore reach `verified`:

| Capability | Advertised by |
|---|---|
| BINARY | Dovecot, Stalwart, Cyrus |
| CATENATE | Dovecot, Cyrus |
| MULTIAPPEND | Dovecot, Stalwart, Cyrus |
| COMPRESS=DEFLATE | Dovecot, Cyrus |
| UTF8=ACCEPT | Dovecot, Stalwart, Courier |
| SORT | Dovecot, Stalwart, GreenMail, Cyrus, Courier |
| SORT=DISPLAY | Dovecot, Stalwart, Cyrus |
| THREAD | Dovecot, Stalwart, Cyrus, Courier |
| QUOTA | Stalwart, GreenMail, Cyrus, Courier |
| ACL | Stalwart, Cyrus, Courier |
| NOTIFY | Dovecot, Cyrus |

Single-advertiser capabilities (MULTISEARCH, SEARCH=FUZZY, QUOTASET,
LIST-MYRIGHTS, METADATA, LIST-METADATA, UIDONLY, URLAUTH, CONTEXT=SEARCH,
UNAUTHENTICATE, I18NLEVEL=1) may get tests that skip elsewhere, but stay `done`.

ESORT is advertised by Dovecot, Stalwart and Cyrus but is **not** in scope:
the client cannot issue `SORT ... RETURN (...)` at all, so there is nothing to
verify. That gap, and the other client-side gaps T11 left open, are T27.

## Deliverables

1. One interop test per capability above, iterating `harness.RunningServers()`
   and skipping on absent capability — never failing on it.
2. Each test asserts protocol behaviour, not just a successful round trip: the
   returned data must be what the seeded messages imply (sort order, decoded
   binary content, thread shape, quota resource present, own rights present).
3. `docs/RFC-COVERAGE.md` rows moved to `verified` only for capabilities that
   passed on two independent servers, with a footnote naming them.

## Done when

The full client interop suite passes on the default matrix, every capability in
the census table above has run (not skipped) on at least two servers, and the
coverage rows say so. No exported symbol changes: a defect found here is fixed
in its own commit and goes through the normal API review if it touches the
surface.
