# T05 — Mailbox commands

**Agent:** `client-core` · **Milestone:** M1 · **Depends on:** T03

**Owns:** `imapclient/select.go`, `list.go`, `status.go`, `namespace.go`

Runs in parallel with T04 and T06.

## Commands

`SELECT`, `EXAMINE`, `CREATE`, `DELETE`, `RENAME`, `SUBSCRIBE`, `UNSUBSCRIBE`,
`LIST`, `LSUB`, `STATUS`, `NAMESPACE` (RFC 2342), `UNSELECT` (RFC 3691), `CLOSE`,
`CHECK`.

## Requirements

- **`SELECT`/`EXAMINE` response handling** is more than the mailbox name:
  `EXISTS`, `RECENT`, `FLAGS`, `PERMANENTFLAGS`, `UIDNEXT`, `UIDVALIDITY`,
  `UNSEEN`, `HIGHESTMODSEQ`. Collect them into a mailbox-status struct that later
  extensions can extend by adding fields.
- **`UIDVALIDITY` change is a correctness event**, not a datum. Surface it
  prominently — a client that misses it corrupts its local cache. Document the
  required caller response.
- **`LIST` must be built for LIST-EXTENDED from the start** (T08 adds the wire
  syntax): selection options, return options, multiple patterns. Design the
  options struct now so T08 adds fields, not a second method. `LSUB` is deprecated
  in rev2 — implement it, but map subscription queries onto `LIST` with the
  `SUBSCRIBED` selection option when available.
- **`STATUS` items are one of the three open-ended sets.** Use the T02 type. Do
  not introduce a bool-field struct here.
- **Mailbox name encoding** goes through the T01 codec: modified UTF-7 normally,
  raw UTF-8 under `UTF8=ACCEPT`. Callers always pass Go strings; the encoding is
  never their problem.
- **Hierarchy delimiter** comes from `LIST`/`NAMESPACE` and varies by server
  (`/`, `.`, `\`). Never hardcode it.
- `CLOSE` expunges, `UNSELECT` does not. Both must exist; document the difference
  since it is a silent data-loss trap.

## Done when

Full mailbox lifecycle passes against Dovecot and GreenMail: create with a
non-ASCII name, list, subscribe, status, select, rename, delete. `UIDVALIDITY`
change detection tested by recreating a mailbox.
