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
| IMAP4REV1 | 3501 | T01,T02,T05,T06 | verified |
| IMAP4REV2 | 9051 | T07 | planned |
| STARTTLS | 3501, 9051 | T03 | done |
| LOGINDISABLED | 3501, 9051 | T04 | done |
| AUTH= | 3501, 9051 | T04 | verified |
| SASL-IR | 4959 | T04 | verified |
| ENABLE | 5161 | T07 | planned |
| IDLE | 2177 | T07 | planned |
| ID | 2971 | T08 | planned |
| NAMESPACE | 2342 | T05 | verified |
| UNSELECT | 3691 | T05 | verified |
| LITERAL+ | 7888 | T01 | verified |
| LITERAL- | 7888 | T01 | done |

## Group A — core modern (task T08)

| Capability | RFC | Status |
|---|---|---|
| UIDPLUS | 4315 | planned |
| MOVE | 6851 | planned |
| ESEARCH | 4731 | planned |
| SEARCHRES | 5182 | planned |
| LIST-EXTENDED | 5258 | planned |
| LIST-STATUS | 5819 | planned |
| SPECIAL-USE | 6154 | planned |
| CREATE-SPECIAL-USE | 6154 | planned |
| CHILDREN | 3348 | planned |
| WITHIN | 5032 | planned |

## Group B — synchronisation & identity (task T09)

| Capability | RFC | Status |
|---|---|---|
| CONDSTORE | 7162 | planned |
| QRESYNC | 7162 | planned |
| OBJECTID | 8474 | planned |
| SAVEDATE | 8514 | planned |
| STATUS=SIZE | 8438 | planned |
| APPENDLIMIT | 7889 | planned |
| PREVIEW | 8970 | planned |
| REPLACE | 8508 | planned |

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
| MIME header encoding | 2047, 2231 | T02 | done | envelope decoding |

## Watch list

`UIDBATCHES` is currently an IETF draft (`ietf-mailmaint-imap-uidbatches`), not
yet an RFC. Do not implement against a draft before v1.0 — a draft that changes
would force a post-v1.0 break. Re-check at each milestone.
