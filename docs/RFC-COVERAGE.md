# RFC / capability coverage

Source of truth: the **IANA IMAP Capabilities registry**
(<https://www.iana.org/assignments/imap-capabilities/>), retrieved 2026-07-31.

**Do not add a row from memory.** Several widely-cited RFC numbers are stale —
`UTF8=ACCEPT` is RFC 9755 (not 6855, which it obsoletes) and `QUOTA` is RFC 9208
(not 2087). If a capability is missing here, check the registry, then add it.

Status: `planned` → `in progress` → `done` → `verified` (exercised against at
least two independent servers in the interop matrix).

## Base

| Capability | RFC | Task | Status |
|---|---|---|---|
| IMAP4REV1 | 3501 | T01,T02,T05,T06 | done [^rev1] |
| IMAP4REV2 | 9051 | T07 | done |
| STARTTLS | 3501, 9051 | T03 | done |
| LOGINDISABLED | 3501, 9051 | T04 | done |
| AUTH= | 3501, 9051 | T04 | verified |
| SASL-IR | 4959 | T04 | verified |
| ENABLE | 5161 | T07 | done |
| IDLE | 2177 | T07 | verified |
| ID | 2971 | T08 | planned |
| NAMESPACE | 2342 | T05 | verified |
| UNSELECT | 3691 | T05 | verified |
| LITERAL+ | 7888 | T01 | verified |
| LITERAL- | 7888 | T01 | done |

[^rev1]: Most of rev1 is exercised across the interop matrix, but the ENVELOPE
    and BODYSTRUCTURE fetch items are covered only by unit and fuzz tests so
    far. Real servers disagree about them more than about anything else in the
    grammar, so the row stays at `done` until the matrix exercises both.

## Group A — core modern (task T08)

| Capability | RFC | Status |
|---|---|---|
| UIDPLUS | 4315 | in progress [^uidplus] |
| MOVE | 6851 | verified |
| ESEARCH | 4731 | verified |
| SEARCHRES | 5182 | verified |
| LIST-EXTENDED | 5258 | verified |
| LIST-STATUS | 5819 | verified |
| SPECIAL-USE | 6154 | verified [^specialuse] |
| CREATE-SPECIAL-USE | 6154 | verified |
| CHILDREN | 3348 | verified |
| WITHIN | 5032 | verified |

Verified against the servers that advertise each capability — Dovecot 2.4.3,
Stalwart 0.11.8 and Cyrus 3.x for most, plus Courier for CHILDREN — with the
emulated paths exercised on GreenMail 2.1.9 and Courier, which advertise only
UIDPLUS, MOVE and CHILDREN between them. SEARCHRES has exactly two independent
servers, Dovecot and Stalwart.

[^uidplus]: `UID EXPUNGE` is complete and verified on all five native servers,
    and the untagged `COPYUID` that RFC 6851 section 4.3 attaches to `UID MOVE`
    is read. `APPENDUID` and `COPYUID` for plain `APPEND` and `COPY` arrive in
    the **tagged** OK (RFC 4315 section 3), which the client core does not yet
    deliver to a command, so `AppendData.UID` and `CopyData.DestinationUIDs`
    remain zero. That needs an internal seam in `imapclient/client.go`, which
    T08 does not own; it is recorded as an escalation in `.state/progress/T08.md`
    and the row stays below `verified` until it lands.

[^specialuse]: Servers disagree about whether a use attribute appears on a plain
    `LIST` or only under `RETURN (SPECIAL-USE)`: Cyrus does the former, Stalwart
    the latter. The client asks explicitly wherever LIST-EXTENDED is available.

## Group B — synchronisation & identity (task T09)

| Capability | RFC | Status |
|---|---|---|
| CONDSTORE | 7162 | verified |
| QRESYNC | 7162 | verified |
| OBJECTID | 8474 | planned [^objectid] |
| SAVEDATE | 8514 | done [^rawfetch] |
| STATUS=SIZE | 8438 | verified |
| APPENDLIMIT | 7889 | done [^appendlimit] |
| PREVIEW | 8970 | done [^preview] |
| REPLACE | 8508 | done [^replace] |

CONDSTORE, QRESYNC and STATUS=SIZE are exercised against Dovecot, Stalwart and
Cyrus, including a disconnect/mutate/reconnect resynchronisation that asserts
the reported delta.

[^objectid]: The request side is reachable through the existing open FETCH and
    STATUS item sets, but two response parsers reject the OBJECTID grammar and
    fail the connection rather than the command: `EMAILID`/`THREADID` are
    `"(" objectid ")"` (RFC 8474 section 5.3) and the STATUS `MAILBOXID` value
    is likewise parenthesised (section 4.3), while both are read as an
    `astring`. Fixing either means editing a file T09 does not own, so it is
    escalated rather than worked around. No interop test requests these items:
    against Stalwart and Cyrus they would take the session down.

[^rawfetch]: The `SAVEDATE` item is requested and its value reaches the caller
    intact, verified against Dovecot and Cyrus, but as a raw `imap.FetchDataRaw`
    value rather than the typed `imap.FetchDataSaveDate` — the FETCH response
    reader has no case for it. No data is lost; the typed decode is a
    one-case addition to a file T09 does not own.

[^appendlimit]: Only Cyrus advertises `APPENDLIMIT` anywhere in the matrix, and
    only in the server-wide `APPENDLIMIT=4294967295` form, so the two-server
    bar for `verified` cannot be met. Both forms RFC 7889 defines are
    implemented and unit-tested, and the server-wide form is exercised live
    against Cyrus.

[^preview]: Requesting `PREVIEW` and receiving its value is verified against
    Dovecot, Stalwart and Cyrus, but as raw data for the same reason as
    `SAVEDATE`. The `LAZY` modifier is additionally unusable: it is encoded
    unparenthesised, which RFC 8970 section 6 does not allow, so the server
    sees two fetch items. Both are escalated.

[^replace]: Only Dovecot advertises `REPLACE`, so the two-server bar cannot be
    met. The native command is verified against Dovecot and the opt-in
    non-atomic fallback of RFC 8508 section 3.4 against Stalwart, GreenMail,
    Cyrus and Courier.

## Group C — content & structure (task T10)

| Capability | RFC | Status |
|---|---|---|
| BINARY | 3516 | planned |
| CATENATE | 4469 | planned |
| MULTIAPPEND | 3502 | planned |
| COMPRESS=DEFLATE | 4978 | planned |
| UTF8=ACCEPT | 9755 | planned |
| UTF8=ALL | 5738, 9755 | planned |
| UTF8=APPEND | 5738, 9755 | planned |
| UTF8=ONLY | 9755 | planned |
| UTF8=USER | 5738, 9755 | planned |
| SORT | 5256 | planned |
| SORT=DISPLAY | 5957 | planned |
| THREAD | 5256 | planned |
| MULTISEARCH | 7377 | planned |
| PARTIAL | 9394 | planned |
| SEARCH=FUZZY | 6203 | planned |

## Group D — administrative & server-side (task T11)

| Capability | RFC | Status |
|---|---|---|
| QUOTA | 9208 | planned |
| QUOTA= | 9208 | planned |
| QUOTASET | 9208 | planned |
| ACL | 4314 | planned |
| RIGHTS= | 4314 | planned |
| LIST-MYRIGHTS | 8440 | planned |
| METADATA | 5464 | planned |
| METADATA-SERVER | 5464 | planned |
| LIST-METADATA | 9590 | planned |
| NOTIFY | 5465 | planned |
| UNAUTHENTICATE | 8437 | planned |
| UIDONLY | 9586 | planned |
| INPROGRESS | 9585 | planned |
| MESSAGELIMIT= | 9738 | planned |
| SAVELIMIT= | 9738 | planned |
| JMAPACCESS | 9698 | planned |

## Group E — legacy & niche (task T11, lower priority)

Implement the parse path so responses from servers advertising these do not
break the client; full command support is best-effort.

| Capability | RFC | Status | Note |
|---|---|---|---|
| LOGIN-REFERRALS | 2221 | planned | referral response codes must parse |
| MAILBOX-REFERRALS | 2193 | planned | as above |
| URLAUTH | 4467 | planned | needed for CATENATE interop |
| URLAUTH=BINARY | 5524 | planned | |
| URL-PARTIAL | 5550 | planned | |
| LANGUAGE | 5255 | planned | |
| I18NLEVEL=1 | 5255 | planned | |
| I18NLEVEL=2 | 5255 | planned | |
| CONTEXT=SEARCH | 5267 | planned | |
| CONTEXT=SORT | 5267 | planned | |
| ESORT | 5267 | planned | |
| FILTERS | 5466 | planned | |
| CONVERT | 5259 | deferred | no known server support |
| IMAPSIEVE= | 6785 | deferred | server-side; parse only |
| ANNOTATE-EXPERIMENT-1 | 5257 | deferred | superseded by METADATA |

## Not in the registry but required

| Item | RFC | Task | Status | Note |
|---|---|---|---|---|
| Response codes | 5530 | T02 | done | ~20 codes; `ResponseCode` must stay open |
| SCRAM-SHA-1/-256 | 5802, 7677 | T04 | done | SASL |
| SCRAM-SHA-*-PLUS | 5802, 7677 | T04 | done | channel binding to TLS exporter |
| OAUTHBEARER | 7628 | T04 | done | |
| XOAUTH2 | — | T04 | done | de-facto, Gmail/Outlook |
| PLAIN | 4616 | T04 | verified | |
| CRAM-MD5 | 2195 | T04 | done | legacy, still common |
| SASLprep | 4013, 3454 | T04 | verified | opt-in; deployed servers compare raw octets |
| NFC/NFKC normalisation | UAX #15 | T04 | done | generated tables, no `x/text` |
| MIME header encoding | 2047, 2231 | T02 | done | envelope decoding |

## Watch list

`UIDBATCHES` is currently an IETF draft (`ietf-mailmaint-imap-uidbatches`), not
yet an RFC. Do not implement against a draft before v1.0 — a draft that changes
would force a post-v1.0 break. Re-check at each milestone.
