# RFC / capability coverage

Source of truth: the **IANA IMAP Capabilities registry**
(<https://www.iana.org/assignments/imap-capabilities/>), retrieved 2026-07-31.

**Do not add a row from memory.** Several widely-cited RFC numbers are stale —
`UTF8=ACCEPT` is RFC 9755 (not 6855, which it obsoletes) and `QUOTA` is RFC 9208
(not 2087). If a capability is missing here, check the registry, then add it.

Status: `planned` → `in progress` → `done` → `verified` (exercised against at
least two independent servers in the interop matrix).

Rows carry a status per protocol direction. **Client** is the status of
`imapclient`; **Server** is the status of `imapserver` and is `—` where the
server framework has not reached that capability yet. A server row cannot be
`verified` until T24 exercises it against an external implementation, so
everything landed by T23 stops at `done`.

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

| Capability | RFC | Client | Server |
|---|---|---|---|
| UIDPLUS | 4315 | verified [^uidplus] | done [^srvuidplus] |
| MOVE | 6851 | verified | done |
| ESEARCH | 4731 | verified | done [^srvesearch] |
| SEARCHRES | 5182 | verified | done [^srvesearch] |
| LIST-EXTENDED | 5258 | verified | done |
| LIST-STATUS | 5819 | verified | done [^srvliststatus] |
| SPECIAL-USE | 6154 | verified [^specialuse] | done |
| CREATE-SPECIAL-USE | 6154 | verified | done |
| CHILDREN | 3348 | verified | done |
| WITHIN | 5032 | verified | done [^srvwithin] |

[^srvuidplus]: Server side delivered by T22 with the base command set, through
    `imap.AppendData` and `imap.CopyData`, not by T23.

[^srvesearch]: Framework-owned: derived from the SEARCH result the backend
    already returns, so no backend interface and no witness. The saved result
    for `$` is framework state scoped to the selection.

[^srvliststatus]: Framework-owned through the mandatory `Session.Status`, issued
    after `Session.List` returns rather than during it, so the backend is not
    re-entered mid-stream.

[^srvwithin]: `OLDER` and `YOUNGER` reach the backend through the open search
    criteria tree with no framework translation, so advertisement is gated on the
    backend witnessing `WITHIN`.

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

| Capability | RFC | Client | Server |
|---|---|---|---|
| CONDSTORE | 7162 | verified | done [^srvcondstore] |
| QRESYNC | 7162 | verified | done [^srvqresync] |
| OBJECTID | 8474 | verified [^objectid] | done [^srvitems] |
| SAVEDATE | 8514 | verified [^savedate] | done [^srvitems] |
| STATUS=SIZE | 8438 | verified | done [^srvitems] |
| APPENDLIMIT | 7889 | done [^appendlimit] | done [^srvitems] |
| PREVIEW | 8970 | verified [^preview] | done [^srvitems] |
| REPLACE | 8508 | done [^replace] | done [^srvreplace] |

[^srvcondstore]: Backend-delegated through the optional `CondStoreMailbox`.
    Conditional STORE reports rejected messages via MODIFIED on a successful
    tagged OK, never as an error.

[^srvqresync]: Backend-delegated through the optional `QResyncMailbox`. The
    reference `memory` backend implements it exactly within one process
    lifetime and documents that its removal record does not survive a restart —
    which is the case QRESYNC exists for. A durable backend must persist it.

[^srvitems]: No framework machinery and no backend interface: these are FETCH
    and STATUS items, and both sets are open types in `package imap`. The
    server-side cost is one capability descriptor witnessing that the backend
    produces the item.

[^srvreplace]: Backend-delegated through the optional `ReplaceMailbox`. Never
    synthesised from APPEND plus EXPUNGE — a client using REPLACE is asking for
    atomicity, which a synthesised version is precisely unable to give.

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

| Capability | RFC | Client | Server |
|---|---|---|---|
| BINARY | 3516 | done | done [^srvbinary] |
| CATENATE | 4469 | done | done [^srvcatenate] |
| MULTIAPPEND | 3502 | done | done [^srvmultiappend] |
| COMPRESS=DEFLATE | 4978 | done | done [^srvcompress] |
| UTF8=ACCEPT | 9755 | done | done [^srvcompress] |
| UTF8=ALL | 5738, 9755 | done | — [^srvutf8] |
| UTF8=APPEND | 5738, 9755 | done | done |
| UTF8=ONLY | 9755 | done | — [^srvutf8] |
| UTF8=USER | 5738, 9755 | done | — [^srvutf8] |
| SORT | 5256 | done | done |
| SORT=DISPLAY | 5957 | done | done |
| THREAD | 5256 | done | done [^srvthread] |
| MULTISEARCH | 7377 | done | done [^srvmultisearch] |
| PARTIAL | 9394 | done | done [^srvpartial] |
| SEARCH=FUZZY | 6203 | done | done [^srvfuzzy] |

[^srvcompress]: Framework-owned and already delivered by T19/T22, not by T23.

[^srvthread]: The command, the response tree encoding and the number-space
    translation are complete. The reference backend implements ORDEREDSUBJECT
    and refuses REFERENCES rather than answering it with ORDEREDSUBJECT
    results, which would silently mis-thread a client's view; REFERENCES needs
    a Message-ID graph the backend does not retain.

[^srvbinary]: FETCH side. `BINARY[]` and `BINARY.SIZE[]` decode the
    content-transfer-encoding, and an encoding that cannot be undone fails with
    UNKNOWN-CTE rather than handing the client base64 it believes is binary.
    Binary APPEND (a literal8 payload) is blocked with CATENATE and MULTIAPPEND
    below.

[^srvmultiappend]: Parsing continues between literals: the parser reads to the
    first literal's announcement, the handler consumes it, then asks the reader
    for the next chunk over the same path continuations and IDLE use. Not atomic
    across messages — `Session.Append` stores one at a time, so a failure leaves
    the messages already stored, and the framework says so rather than
    pretending. RFC 3502's UID-set form of APPENDUID is reported.

[^srvcatenate]: Client text and server-side URLs assembled into one message
    through the optional `CatenateSession`. Parts are read in order because each
    literal's length is only knowable once the previous is off the wire; that
    buffering is the grammar's price, not a design choice.

[^srvutf8]: RFC 9755 deprecates UTF8=ALL and UTF8=USER. UTF8=ONLY is a statement
    that the server *refuses* ASCII-only clients, which this framework cannot
    honour, and advertising it while still serving them would be a false claim.

[^srvmultisearch]: The ESEARCH command, searching mailboxes the connection has
    not selected. Each result carries its mailbox and UIDVALIDITY, without which
    a UID from another mailbox is meaningless. RFC 7377's scope options
    (`subtree`, `mailboxes`, `personal`) are refused rather than misread as
    mailbox names; plain names are supported.

[^srvpartial]: Framework-owned windowing of an ESEARCH result, including RFC
    9394's negative ranges. It saves wire bytes and client memory, not backend
    work — the backend still evaluates the whole search, and RFC 9394 defines no
    interface that would let it stop early.

[^srvfuzzy]: The FUZZY modifier reaches the backend through the open search
    criteria tree with no framework translation, so the capability is purely a
    claim about the backend and is gated on its witness.

## Group D — administrative & server-side (task T11)

| Capability | RFC | Client | Server |
|---|---|---|---|
| QUOTA | 9208 | done | done |
| QUOTA= | 9208 | done | done [^srvquotares] |
| QUOTASET | 9208 | done | done |
| ACL | 4314 | done | done |
| RIGHTS= | 4314 | done | done [^srvrights] |
| LIST-MYRIGHTS | 8440 | done | done |
| METADATA | 5464 | done | done |
| METADATA-SERVER | 5464 | done | done |
| LIST-METADATA | 9590 | done | done |
| NOTIFY | 5465 | done | done [^srvnotify] |
| UNAUTHENTICATE | 8437 | done | done |
| UIDONLY | 9586 | done | done [^srvuidonly] |
| INPROGRESS | 9585 | done | done [^srvinprogress] |
| MESSAGELIMIT= | 9738 | done | done |
| SAVELIMIT= | 9738 | done | done |
| JMAPACCESS | 9698 | done | done [^srvjmap] |

[^srvquotares]: Resource names cross the backend boundary as open
    `imap.QuotaResourceName` strings, so a server serving a resource this
    library has not heard of needs no change. The `QUOTA=RES-*` advertisement
    tokens themselves are a backend concern.

[^srvrights]: Rights are open `imap.ACLRights` strings in both directions; a
    letter this library does not know passes through verbatim.

[^srvuidonly]: All-or-nothing, as the RFC requires. Sequence-number commands are
    refused rather than reinterpreted as UIDs, unilateral FETCH becomes
    UIDFETCH, and EXPUNGE becomes VANISHED because an untagged EXPUNGE carries a
    sequence number and cannot be sent at all. `WriteUIDFetchResponse` was added
    to `internal/imapcodec`, sharing the item grammar with `WriteFetchResponse`
    rather than duplicating it.

[^srvnotify]: Delivered through `SessionUpdater`, a session-scoped channel added
    alongside the selection-scoped `Updater` rather than by widening it — which
    SERVER-DESIGN.md §3 names as the trap, since it would let any
    selection-scoped push outlive its selection. Events name a mailbox and carry
    no sequence numbers, because the client has not selected the mailbox and has
    no sequence-number view of it; RFC 5465 §6 reports them as STATUS for that
    reason. The queue coalesces per mailbox and drops oldest on overflow rather
    than killing the connection — the opposite of the selection queue's rule,
    because a dropped notification costs a refresh while a dropped selection
    update desynchronises a view in active use. Overflow is reported rather than
    hidden.

[^srvinprogress]: The untagged OK progress response shape is framework-owned and
    advertised; no backend surface is required to emit one.

[^srvjmap]: Advertisement and response code only, witnessed by name through
    `CapabilitySupport`. The reference backend serves no JMAP endpoint and so
    does not witness it.

## Group E — legacy & niche (task T11, lower priority)

Implement the parse path so responses from servers advertising these do not
break the client; full command support is best-effort.

| Capability | RFC | Client | Server | Note |
|---|---|---|---|---|
| LOGIN-REFERRALS | 2221 | done | done [^srvreferral] | referral response codes parse |
| MAILBOX-REFERRALS | 2193 | done | done [^srvreferral] | as above |
| URLAUTH | 4467 | done | done [^srvurlauth] | GENURLAUTH / URLFETCH / RESETKEY |
| URLAUTH=BINARY | 5524 | done | done | capability accepted with URLAUTH |
| URL-PARTIAL | 5550 | done | done | `;PARTIAL=` in IMAP URLs |
| LANGUAGE | 5255 | done | done | |
| I18NLEVEL=1 | 5255 | done | done | capability probe |
| I18NLEVEL=2 | 5255 | done | — [^srvi18n2] | COMPARATOR command |
| CONTEXT=SEARCH | 5267 | done | — [^srvcontext] | CANCELUPDATE + RETURN keywords |
| CONTEXT=SORT | 5267 | done | — [^srvcontext] | as above |
| ESORT | 5267 | done | done [^srvesort] | capability + RETURN keywords |
| FILTERS | 5466 | done | — [^srvfilters] | UNDEFINED-FILTER parse |
| CONVERT | 5259 | deferred | deferred | no known server support |
| IMAPSIEVE= | 6785 | deferred | deferred | server-side; parse only |
| ANNOTATE-EXPERIMENT-1 | 5257 | deferred | deferred | superseded by METADATA |

[^srvreferral]: No code beyond the advertisement. Referrals are response *codes*,
    and `imap.ResponseCode` is an open string type carried on `*imap.Error`, so
    a backend returning one is already relayed verbatim. API rule 5 paying off:
    the extension is a data change, not a type change.

[^srvurlauth]: GENURLAUTH, URLFETCH and RESETKEY, with tokens minted and verified
    by the backend since only it holds the per-mailbox secret. The reference
    backend uses an HMAC and a constant-time comparison, and its test asserts
    that a forged token is refused — the one property here that is a security
    question rather than a formatting one.

[^srvi18n2]: I18NLEVEL=2 requires the COMPARATOR command, which is not
    implemented. Advertising level 2 without it would fail the first client that
    used it, so only I18NLEVEL=1 is advertised.

[^srvcontext]: **Not advertised, deliberately.** CONTEXT asks the server to hold
    a search result open and push incremental updates as the mailbox changes —
    a notification lifetime that outlives the command, like NOTIFY's. Claiming
    it and then never sending an update is worse than not offering it: a client
    using CONTEXT reads silence as "nothing changed" rather than "not
    implemented".

[^srvesort]: The ESEARCH-shaped response for SORT. MIN and MAX are the ends of
    the *sorted* order rather than the numerically smallest and largest, and ALL
    preserves that order rather than collapsing it into ranges, which would
    re-sort it.

[^srvfilters]: Server-side saved search filters, which only a backend that
    stores them can serve. No optional interface is defined: the extension's
    value is in the filters existing, and inventing surface no backend asked for
    would be cost without a caller.

## Not in the registry but required

| Item | RFC | Task | Status | Note |
|---|---|---|---|---|
| Response codes | 5530 | T02 | done | ~20 codes; `ResponseCode` must stay open |
| SCRAM-SHA-1/-256 | 5802, 7677 | T04, T23 | done | SASL; server side in T23 [^srvscram] |
| SCRAM-SHA-*-PLUS | 5802, 7677 | T04 | done (client) | channel binding to TLS exporter; **not** server-side [^srvscramplus] |
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

[^srvscram]: Server side added by T23 through the optional `SCRAMCredentials`
    backend interface. The exchange, nonces and proof verification are
    framework-owned; the backend supplies salt, iteration count, StoredKey and
    ServerKey, and never sees a password. Verified end to end against the real
    client, which checks the server's signature — so a passing exchange is
    mutual authentication, not just a server that said OK. SCRAM is permitted on
    cleartext without the `AllowInsecureAuth` opt-in PLAIN needs, because no
    password crosses the wire.

[^srvscramplus]: The `-PLUS` variants are deliberately not advertised
    server-side. Advertising them commits the server to rejecting a client that
    downgrades to the non-PLUS form, and getting that wrong converts a downgrade
    defence into a downgrade vector. Absent is better than approximate.
