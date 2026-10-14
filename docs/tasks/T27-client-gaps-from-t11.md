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
(UIDONLY carries no sequence numbers). Four were recorded as gaps in the client — three real, one withdrawn below —
and `docs/RFC-COVERAGE.md` read `done` for capabilities they affect:

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
4. ~~**UIDAFTER / UIDBEFORE (RFC 9586).**~~ **Withdrawn.** T11's note
   attributed these search keys to RFC 9586, but RFC 9586 (UIDONLY) does not
   define or mention them, and no published RFC or IANA registry entry does.
   Nothing is implemented from an unverified source; if a published RFC adds
   them, they become an additive `imap.SearchCriteria` type then.

## Found while doing it

- **ALERT was dropped too.** The INPROGRESS gap was one case of a general
  one: every untagged status response no command claimed was discarded,
  connection-level `[ALERT]` included. The fix is therefore one generic hook,
  not one per response code, so the next code needs no API change.
- **Rename events lost the old name.** NOTIFY reports a rename as LIST with
  `OLDNAME` extended data (RFC 5465 section 5.4), which neither `imap.ListData`
  nor the LIST parser modelled. `imap.ListData.OldName` is now filled by the
  client's LIST parser. The field passes API-STABILITY §9 because a server
  can meaningfully fill it, in a NOTIFY rename and in the RFC 9051 SELECT
  case. §9 does not require this framework to write it already. The server
  does not write it from a backend's LIST results, where it has no meaning.
  Server-side OLDNAME belongs to the NOTIFY MailboxName work, gated on what
  the session negotiated, and to a framework-derived RFC 9051 §6.3.2 case.
- **api-guardian review (2026-10-14): CHANGES REQUIRED, now addressed.**
  - Two blocking findings:
    - Plain SORT had joined the extended-search exclusion. That broke
      pipelining that v1.1 allowed, so now only a SORT with RETURN takes
      part.
    - The LIST that an IMAP4rev2 server sends during SELECT reached the
      NOTIFY hook. The SELECT and EXAMINE collectors now claim it.
  - Two names were fixed before they become permanent:
    - `UnilateralDataHandler.MailboxStatus` became `Status`, matching the
      other fields, which are named after their response.
    - `StatusResponse` moved to the root package, with an alias, like every
      other shared response shape.
  - Documented rather than fixed:
    - NOTIFY events swallowed by an in-flight STATUS or LIST.
    - METADATA events not delivered.
    - CONTEXT updates not delivered. Those rows now read `in progress`, and
      `SortExtended` refuses RETURN (UPDATE).

## Constraints

The root and `imapclient` APIs are frozen at v1: every change here must be
additive — a new field on a guarded options or handler struct, a new
constant, a new search-key type — and must show as compatible in `apidiff`.
`UnilateralDataHandler` is already a struct of callbacks precisely so that new
unsolicited response kinds can be added this way. Each addition goes through
`api-guardian` before merge.

## Done when

Items 1–3 are reachable through the public API, and the rename `OLDNAME`
found while doing it is modelled; each has a scripted unit test,
a fuzz target covers any new parser path, ESORT and NOTIFY have interop tests
on the servers that advertise them (Dovecot, Stalwart and Cyrus for ESORT;
Dovecot and Cyrus for NOTIFY), `apidiff` reports only compatible changes, and
the coverage rows and `CHANGELOG.md` describe the state accurately.
