# T27 — Client gaps T11 left open

**Agent:** `extensions` + `api-guardian` · **Milestone:** post-M6 (root `v1.2.0`) · **Depends on:** T11

**Owns:** `imapclient/ext_d_inprogress.go`, `imapclient/ext_d_notify.go`,
`imapclient/ext_c_sort.go`, the `UnilateralDataHandler` and `SortOptions`
declarations, the search-key additions in the root package, and the matching
rows in `docs/RFC-COVERAGE.md`

## Why this exists

T11 closed with seven escalations recorded only in its working notes. Three
were later resolved (FILTER became `imap.SearchFilter` in T23; the PARTIAL
return option and the MESSAGELIMIT code exist), one is documentation only
(UIDONLY carries no sequence numbers). Four are real gaps in the client, and
`docs/RFC-COVERAGE.md` reads `done` for capabilities they affect:

1. **ESORT (RFC 5267).** The client detects the capability (`SupportsESort`)
   and declares `SearchReturnESortAll`, but `SortOptions` has no return-option
   field, so `SORT RETURN (...)` cannot be sent. The row overstates.
2. **INPROGRESS (RFC 9585).** Untagged `OK [INPROGRESS ...]` is parsed, but
   nothing hands it to the caller: neither `Options` nor
   `UnilateralDataHandler` has a hook for it.
3. **NOTIFY non-selected events (RFC 5465).** STATUS and MailboxName events for
   mailboxes other than the selected one are discarded by the connection-level
   handler; `Client.Notify`'s documentation says so. Only selected-mailbox
   events reach the caller.
4. **UIDAFTER / UIDBEFORE (RFC 9586).** The search keys UIDONLY introduces have
   no representation in the search-criteria tree.

## Constraints

The root and `imapclient` APIs are frozen at v1: every change here must be
additive — a new field on a guarded options or handler struct, a new
constant, a new search-key type — and must show as compatible in `apidiff`.
`UnilateralDataHandler` is already a struct of callbacks precisely so that new
unsolicited response kinds can be added this way. Each addition goes through
`api-guardian` before merge.

## Done when

All four are reachable through the public API, each has a scripted unit test,
a fuzz target covers any new parser path, ESORT and NOTIFY have interop tests
on the servers that advertise them (Dovecot, Stalwart and Cyrus for ESORT;
Dovecot and Cyrus for NOTIFY), `apidiff` reports only compatible changes, and
the coverage rows and `CHANGELOG.md` describe the state accurately.
