# T11 — Extensions groups D+E: administrative, server-side, legacy

**Agent:** `extensions` · **Milestone:** M3 · **Depends on:** T08 ·
**Status:** blocked

**Owns:** `imapclient/ext_d_*.go`, `imapclient/ext_e_*.go`

Runs in parallel with T10. This task closes the remaining coverage gap, so its
completion is what makes "all features covered" true.

## Group D — administrative

| Capability | RFC | Notes |
|---|---|---|
| QUOTA, QUOTA=, QUOTASET | 9208 | **9208, not the obsoleted 2087** |
| ACL, RIGHTS= | 4314 | `SETACL`, `DELETEACL`, `GETACL`, `LISTRIGHTS`, `MYRIGHTS` |
| LIST-MYRIGHTS | 8440 | Rights inline in `LIST` |
| METADATA, METADATA-SERVER | 5464 | `GETMETADATA`, `SETMETADATA` |
| LIST-METADATA | 9590 | Metadata inline in `LIST` |
| NOTIFY | 5465 | Event notification beyond IDLE |
| UNAUTHENTICATE | 8437 | Return to not-authenticated state |
| UIDONLY | 9586 | Suppresses sequence numbers entirely |
| INPROGRESS | 9585 | Progress response codes for long operations |
| MESSAGELIMIT=, SAVELIMIT= | 9738 | Server-imposed limits |
| JMAPACCESS | 9698 | Advertises a JMAP endpoint |

## Group E — legacy & niche

Parse path required so responses from servers advertising these do not break the
client; full command support is best-effort.

`LOGIN-REFERRALS` (2221), `MAILBOX-REFERRALS` (2193), `URLAUTH` (4467),
`URLAUTH=BINARY` (5524), `URL-PARTIAL` (5550), `LANGUAGE` / `I18NLEVEL=1` /
`I18NLEVEL=2` (5255), `CONTEXT=SEARCH` / `CONTEXT=SORT` / `ESORT` (5267),
`FILTERS` (5466).

Deferred, parse-only, no command support: `CONVERT` (5259), `IMAPSIEVE=` (6785),
`ANNOTATE-EXPERIMENT-1` (5257).

## Notable

- **`UIDONLY` (9586) is invasive**: the server stops sending sequence numbers
  altogether, and `EXPUNGE` becomes `VANISHED`. Any code path assuming a sequence
  number exists must already tolerate its absence. If it does not, that is a core
  design issue — escalate to `api-guardian` rather than patching around it here.
- **`NOTIFY` (5465) overlaps IDLE** and can deliver events for unselected
  mailboxes. Reuse the `UnilateralDataHandler` from T03; do not invent a second
  notification mechanism.
- **Referrals** are the reason `[REFERRAL]` response codes must parse even
  without support — a client that chokes on one cannot report a useful error.
- **`INPROGRESS` (9585)** arrives as untagged `OK` during long commands. Expose
  it as an optional progress callback in the options struct.

## Do not implement

`UIDBATCHES` is an IETF draft, not an RFC. Implementing against a draft that then
changes forces a post-v1.0 break. Re-check at each milestone; see the watch list
in `docs/RFC-COVERAGE.md`.

## Done when

`docs/RFC-COVERAGE.md` has no `planned` rows outside the explicitly `deferred`
set. Cyrus is the reference server for METADATA and ACL; verify against it.
