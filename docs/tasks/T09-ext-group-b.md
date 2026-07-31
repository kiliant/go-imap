# T09 — Extensions group B: synchronisation & identity

**Agent:** `extensions` · **Milestone:** M2 · **Depends on:** T07

**Owns:** `imapclient/ext_b_*.go`

Runs in parallel with T08.

## Scope

| Capability | RFC | Notes |
|---|---|---|
| CONDSTORE | 7162 | `MODSEQ`, `CHANGEDSINCE`, `UNCHANGEDSINCE`, `HIGHESTMODSEQ` |
| QRESYNC | 7162 | `SELECT ... QRESYNC`, `VANISHED`, `VANISHED (EARLIER)` |
| OBJECTID | 8474 | `EMAILID`, `THREADID`, `MAILBOXID` |
| SAVEDATE | 8514 | `SAVEDATE` fetch item |
| STATUS=SIZE | 8438 | `SIZE` status item |
| APPENDLIMIT | 7889 | Per-server and per-mailbox append size limit |
| PREVIEW | 8970 | `PREVIEW` fetch item, `LAZY` modifier |
| REPLACE | 8508 | `REPLACE`, `UID REPLACE` |

## This is the group that makes the library actually useful

CONDSTORE and QRESYNC are what let a client synchronise a large mailbox
incrementally instead of re-fetching it. They are also the most intricate
extensions in IMAP, and the ones where a subtle bug means **silent data loss in
the caller's local store**. Budget accordingly.

- `HIGHESTMODSEQ` + `UIDVALIDITY` together form the sync anchor. Either changing
  invalidates the other's meaning. Get this pair right before anything else.
- `VANISHED (EARLIER)` reports expunges that happened while disconnected — this
  is the whole point of QRESYNC. It is *not* interchangeable with `EXPUNGE`:
  `VANISHED` uses UIDs and does not renumber, `EXPUNGE` uses sequence numbers and
  does. Conflating them corrupts caches.
- `MODSEQ` values are 64-bit unsigned and must not be truncated.
- `UNCHANGEDSINCE` on `STORE` gives conditional update with a `MODIFIED` response
  code listing the failures — surface that list, do not reduce it to an error.
- `CONDSTORE` can be enabled implicitly by using a CONDSTORE parameter, not only
  by `ENABLE`. Handle both.

## New FETCH and STATUS items

`MODSEQ`, `EMAILID`, `THREADID`, `SAVEDATE`, `PREVIEW`, `SIZE`, `MAILBOXID` all
extend the T02 open sets. Each is a **new type**, not a modification of an
existing one. If you find yourself editing T02's types, stop and escalate to
`api-guardian` — that would be exactly the failure this project exists to avoid.

## Done when

An incremental resync against Dovecot and Stalwart correctly reports messages
added, flagged and expunged while disconnected — verified by a test that
disconnects, mutates via a second connection, reconnects, and asserts the delta.
Coverage rows updated.
