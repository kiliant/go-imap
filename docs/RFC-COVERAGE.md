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
| ID | 2971 | T08 | verified [^id] |
| NAMESPACE | 2342 | T05 | verified |
| UNSELECT | 3691 | T05 | verified |
| LITERAL+ | 7888 | T01 | verified |
| LITERAL- | 7888 | T01 | done |

[^rev1]: Most of rev1 is exercised across the interop matrix, but the ENVELOPE
    and BODYSTRUCTURE fetch items are covered only by unit and fuzz tests so
    far. Real servers disagree about them more than about anything else in the
    grammar, so the row stays at `done` until the matrix exercises both.

[^id]: Implemented as `Client.ID` (`imapclient/ext_a_id.go`). Previously listed
    under T08's base ownership without appearing in T08's scope table; the scope
    table now includes it. Verified on Dovecot, Stalwart and Cyrus.

## Group A — core modern (task T08)

| Capability | RFC | Status |
|---|---|---|
| UIDPLUS | 4315 | verified [^uidplus] |
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

[^uidplus]: `UID EXPUNGE` is verified on all five native servers. Untagged
    `COPYUID` on `UID MOVE` (RFC 6851 section 4.3) and tagged `APPENDUID` /
    `COPYUID` on plain `APPEND` / `COPY` (RFC 4315 section 3) are both read
    into `AppendData` / `CopyData`.

[^specialuse]: Servers disagree about whether a use attribute appears on a plain
    `LIST` or only under `RETURN (SPECIAL-USE)`: Cyrus does the former, Stalwart
    the latter. The client asks explicitly wherever LIST-EXTENDED is available.

## Group B — synchronisation & identity (task T09)

| Capability | RFC | Status |
|---|---|---|
| CONDSTORE | 7162 | verified |
| QRESYNC | 7162 | verified |
| OBJECTID | 8474 | verified [^objectid] |
| SAVEDATE | 8514 | verified [^savedate] |
| STATUS=SIZE | 8438 | verified |
| APPENDLIMIT | 7889 | done [^appendlimit] |
| PREVIEW | 8970 | verified [^preview] |
| REPLACE | 8508 | done [^replace] |

CONDSTORE, QRESYNC, OBJECTID, SAVEDATE, STATUS=SIZE and PREVIEW are exercised
against the servers that advertise them (Dovecot, Stalwart and/or Cyrus),
including a disconnect/mutate/reconnect QRESYNC resynchronisation and
CONDSTORE `MODIFIED` on tagged OK.

[^objectid]: `EMAILID`, `THREADID` and STATUS `MAILBOXID` use the RFC 8474
    parenthesised grammar. Verified live against Stalwart and Cyrus.

[^savedate]: Typed `imap.FetchDataSaveDate` decode verified against Dovecot
    and Cyrus.

[^appendlimit]: Only Cyrus advertises `APPENDLIMIT` anywhere in the matrix, and
    only in the server-wide `APPENDLIMIT=4294967295` form, so the two-server
    bar for `verified` cannot be met. Both forms RFC 7889 defines are
    implemented and unit-tested, and the server-wide form is exercised live
    against Cyrus.

[^preview]: Typed `imap.FetchDataPreview` decode and the parenthesised
    `PREVIEW (LAZY)` request form verified against Dovecot, Stalwart and Cyrus.

[^replace]: Only Dovecot advertises `REPLACE`, so the two-server bar cannot be
    met. The native command is verified against Dovecot and the opt-in
    non-atomic fallback of RFC 8508 section 3.4 against Stalwart, GreenMail,
    Cyrus and Courier.

## Group C — content & structure (task T10)

| Capability | RFC | Status |
|---|---|---|
| BINARY | 3516 | done |
| CATENATE | 4469 | done |
| MULTIAPPEND | 3502 | done |
| COMPRESS=DEFLATE | 4978 | done |
| UTF8=ACCEPT | 9755 | done |
| UTF8=ALL | 5738, 9755 | done |
| UTF8=APPEND | 5738, 9755 | done |
| UTF8=ONLY | 9755 | done |
| UTF8=USER | 5738, 9755 | done |
| SORT | 5256 | done |
| SORT=DISPLAY | 5957 | done |
| THREAD | 5256 | done |
| MULTISEARCH | 7377 | done |
| PARTIAL | 9394 | done |
| SEARCH=FUZZY | 6203 | done |

## Group D — administrative & server-side (task T11)

| Capability | RFC | Status |
|---|---|---|
| QUOTA | 9208 | done |
| QUOTA= | 9208 | done |
| QUOTASET | 9208 | done |
| ACL | 4314 | done |
| RIGHTS= | 4314 | done |
| LIST-MYRIGHTS | 8440 | done |
| METADATA | 5464 | done |
| METADATA-SERVER | 5464 | done |
| LIST-METADATA | 9590 | done |
| NOTIFY | 5465 | done |
| UNAUTHENTICATE | 8437 | done |
| UIDONLY | 9586 | done |
| INPROGRESS | 9585 | done |
| MESSAGELIMIT= | 9738 | done |
| SAVELIMIT= | 9738 | done |
| JMAPACCESS | 9698 | done |

## Group E — legacy & niche (task T11, lower priority)

Implement the parse path so responses from servers advertising these do not
break the client; full command support is best-effort.

| Capability | RFC | Status | Note |
|---|---|---|---|
| LOGIN-REFERRALS | 2221 | done | referral response codes parse |
| MAILBOX-REFERRALS | 2193 | done | as above |
| URLAUTH | 4467 | done | GENURLAUTH / URLFETCH / RESETKEY |
| URLAUTH=BINARY | 5524 | done | capability accepted with URLAUTH |
| URL-PARTIAL | 5550 | done | `;PARTIAL=` in IMAP URLs |
| LANGUAGE | 5255 | done | |
| I18NLEVEL=1 | 5255 | done | capability probe |
| I18NLEVEL=2 | 5255 | done | COMPARATOR command |
| CONTEXT=SEARCH | 5267 | done | CANCELUPDATE + RETURN keywords |
| CONTEXT=SORT | 5267 | done | as above |
| ESORT | 5267 | done | capability + RETURN keywords; SORT cmd is T10 |
| FILTERS | 5466 | done | UNDEFINED-FILTER parse; SearchFilter escalated to T02 |
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
