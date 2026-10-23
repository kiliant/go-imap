# RFC / capability coverage

Source of truth: the **IANA IMAP Capabilities registry**
(<https://www.iana.org/assignments/imap-capabilities/>), retrieved 2026-07-31.
Rechecked on 2026-10-15. Since then, the only change that affects this table is
UIDBATCHES: RFC 10022 was published on the day of retrieval, and the
capability has moved off the watch list.

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
| IMAP4REV2 | 9051 | T07 | done [^srvrev2] |
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

[^srvrev2]: Server side gated off until T24. `SERVER-DESIGN.md` §1 sets the bar:
    `IMAP4REV2` is advertised only when **every** behaviour RFC 9051
    incorporates is implemented, "advertising it otherwise is a lie the client
    cannot detect" — so `frameworkRev2` was hardcoded false and no client could
    `ENABLE` the rev2 paths the package already had. T24 closed the three gaps
    that kept it false and turned it on:

    - UIDPLUS, the last incorporated behaviour with no advertisement and no
      `UID EXPUNGE`. See [^srvuidplus].
    - A bare `SEARCH` answered in the rev1 shape. See [^srvesearch].
    - `SELECT`/`EXAMINE` sent no untagged `LIST`, which RFC 9051 §6.3.2 adds and
      RFC 3501 does not have. It is now emitted for rev2 sessions from the
      backend's own `List`, so the attributes and delimiter agree with what a
      `LIST` command would report; rev1 sessions still get no such response.

    The advertisement is asserted behaviour rather than a claim:
    `imapserver/rev2_test.go` probes one command form per incorporated
    behaviour over the wire, and asserts each rev1/rev2 difference in both
    directions so enabling rev2 cannot silently change what a rev1 client sees.
    The `goimap` interop entry lists `IMAP4REV2` in its expected set.

    Those probes run against `imapserver/memory`, which implements everything —
    so they show the behaviour exists, not that the *gate* holds. It initially
    did not: the capability was armed on the atomic-MOVE witness alone, so a
    backend implementing MOVE and nothing else advertised rev2 and was held to
    the rest of the incorporated set. The gate is now the conjunction of the
    incorporated capabilities' own witnesses, listed as data in
    `rev2Incorporated`; see `API-STABILITY.md` §10, "An umbrella capability is
    witnessed by its members".

## Group A — core modern (task T08)

| Capability | RFC | Client | Server |
|---|---|---|---|
| UIDPLUS | 4315 | verified [^uidplus] | done [^srvuidplus] |
| MOVE | 6851 | verified | done |
| ESEARCH | 4731 | verified | done [^srvesearch] |
| SEARCHRES | 5182 | verified | done [^srvesearch] |
| LIST-EXTENDED | 5258 | verified [^listext] | done [^srvlistext] |
| LIST-STATUS | 5819 | verified | done [^srvliststatus] |
| SPECIAL-USE | 6154 | verified [^specialuse] | done |
| CREATE-SPECIAL-USE | 6154 | verified | done |
| CHILDREN | 3348 | verified | done |
| WITHIN | 5032 | verified | done [^srvwithin] |

[^srvuidplus]: T22 delivered the `APPENDUID` and `COPYUID` response codes with
    the base command set, through `imap.AppendData` and `imap.CopyData`. That
    was two of RFC 4315's three parts, and this row read `done` while the
    capability was never advertised and `UID EXPUNGE` was absent from the UID
    subcommand table — so no conforming client could use any of it. T24
    completed it after the `goimap` interop entry surfaced the gap: `UID
    EXPUNGE` is implemented (the `SelectedMailbox.Expunge` contract already
    carried the UID-set filter), and the capability is advertised behind a
    `CapabilitySupport` witness, since the two response codes are real only if
    the backend returns UIDs.

[^srvesearch]: Framework-owned: derived from the SEARCH result the backend
    already returns, so no backend interface and no witness. The saved result
    for `$` is framework state scoped to the selection.
    Which response shape a SEARCH gets is a function of the session, not of the
    command: a `RETURN` clause always selects `ESEARCH`, and since T24 so does
    an enabled IMAP4rev2, because RFC 9051 appendix E removes the untagged
    `SEARCH` response outright. Until then a rev2 session answered a bare
    `SEARCH` in the rev1 shape, which a strict rev2 client has no production
    for. rev1 sessions are unchanged.

[^srvliststatus]: Framework-owned through the mandatory `Session.Status`, issued
    after `Session.List` returns rather than during it, so the backend is not
    re-entered mid-stream.

[^srvwithin]: `OLDER` and `YOUNGER` reach the backend through the open search
    criteria tree with no framework translation, so advertisement is gated on the
    backend witnessing `WITHIN`.

Verified against the servers that advertise each capability — Dovecot 2.4.3,
Stalwart 0.11.8 and Cyrus 3.10 for most, plus Courier for CHILDREN — with the
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

[^listext]: Until T29 the client dropped the CHILDINFO extended data item
    (RFC 5258 section 3.5), so RECURSIVEMATCH could be sent but its answer not
    read. It now fills `ListData.ChildInfo` and keeps every unmodelled item raw
    in `ListData.Extended`. The section 3.5 scenario, an unsubscribed parent of
    a subscribed child under `LIST (SUBSCRIBED RECURSIVEMATCH)`, returns
    `CHILDINFO ("SUBSCRIBED")` on Dovecot, Stalwart and Cyrus; Courier and
    GreenMail do not offer LIST-EXTENDED.

[^srvlistext]: CHILDINFO is written by the framework, only under
    RECURSIVEMATCH and only for requested base selection options. The
    backend, or the framework's own evaluation, supplies `ListData.ChildInfo`.
    RECURSIVEMATCH is accepted only beside SUBSCRIBED, the one base option
    implemented; REMOTE and SPECIAL-USE are independent options. A backend
    witnessing `"LIST-EXTENDED"` evaluates it, as `imapserver/memory` does and
    `backendtest` checks. The framework evaluates it for any other backend,
    from plain List calls. `goimap` passes the section 3.5 interop scenario.

## Group B — synchronisation & identity (task T09)

| Capability | RFC | Client | Server |
|---|---|---|---|
| CONDSTORE | 7162 | verified | done [^srvcondstore] |
| QRESYNC | 7162 | verified | done [^srvqresync] |
| OBJECTID | 8474 | verified [^objectid] | done [^srvitems] |
| SAVEDATE | 8514 | verified [^savedate] | done [^srvitems] |
| STATUS=SIZE | 8438 | verified | done [^srvitems] |
| APPENDLIMIT | 7889 | verified [^appendlimit] | done [^srvitems] |
| PREVIEW | 8970 | verified [^preview] | done [^srvitems] |
| REPLACE | 8508 | verified [^replace] | done [^srvreplace] |

[^srvcondstore]: Backend-delegated through the optional `CondStoreMailbox`.
    Conditional STORE reports rejected messages via MODIFIED on a successful
    tagged OK, never as an error. A successful conditional STORE.SILENT still
    returns MODSEQ for each stored message; the framework fetches it when the
    backend honoured Silent. After any CONDSTORE enabling command, untagged
    FETCH from STORE, implicit `\Seen`, MODSEQ/CHANGEDSINCE FETCH and other
    sessions carries UID (RFC 7162 sections 3.1.3, 3.2.4). (#54, R2/R3)

[^srvqresync]: Backend-delegated through the optional `QResyncMailbox`. The
    reference `memory` backend implements it exactly within one process
    lifetime and documents that its removal record does not survive a restart —
    which is the case QRESYNC exists for. A durable backend must persist it.
    Resync serves QRESYNC SELECT/EXAMINE and repeated UID FETCH with CHANGEDSINCE
    and VANISHED. The full requested UID set bounds VANISHED (EARLIER), written
    before FETCH results; PARTIAL does not narrow this historical report.

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

[^appendlimit]: The server-wide `APPENDLIMIT=<n>` form is verified against
    Cyrus 3.10 and Dovecot 2.4.3: the interop test requires the returned limit
    to equal the advertised value and be marked server-wide. Dovecot only
    advertises it when the quota plugin reports a per-message limit, so the
    harness sets `quota_mail_size`; until 2026-10-03 nothing did, and Cyrus was
    the only advertising server. The per-mailbox `STATUS (APPENDLIMIT)` form is
    unit-tested only — no server in the default matrix uses it.

[^preview]: Typed `imap.FetchDataPreview` decode and the parenthesised
    `PREVIEW (LAZY)` request form verified against Dovecot, Stalwart and Cyrus.

[^replace]: The native command is verified against Dovecot 2.4.3 and Cyrus
    3.10.2, which added REPLACE after the 3.6 series the harness ran until
    2026-10-03. The opt-in non-atomic fallback of RFC 8508 section 3.4 is
    exercised against Stalwart, GreenMail and Courier.

## Group C — content & structure (task T10)

| Capability | RFC | Client | Server |
|---|---|---|---|
| BINARY | 3516 | verified [^t26c] | done [^srvbinary] |
| CATENATE | 4469 | verified [^t26c] | done [^srvcatenate] |
| MULTIAPPEND | 3502 | verified [^t26c] | done [^srvmultiappend] |
| COMPRESS=DEFLATE | 4978 | verified [^t26c] | done [^srvcompress] |
| UTF8=ACCEPT | 9755 | verified [^t26c] | done [^srvcompress] |
| UTF8=ALL | 5738, 9755 | done | — [^srvutf8] |
| UTF8=APPEND | 5738, 9755 | done | obsolete; not advertised [^srvutf8] |
| UTF8=ONLY | 9755 | done | — [^srvutf8] |
| UTF8=USER | 5738, 9755 | done | — [^srvutf8] |
| SORT | 5256 | verified [^t26c] | done |
| SORT=DISPLAY | 5957 | verified [^t26c] | done |
| THREAD | 5256 | verified [^t26c] | done [^srvthread] |
| MULTISEARCH | 7377 | done | done [^srvmultisearch] |
| PARTIAL | 9394 | done | done [^srvpartial] |
| SEARCH=FUZZY | 6203 | done | done [^srvfuzzy] |

[^srvcompress]: Framework-owned and already delivered by T19/T22, not by T23.

[^srvthread]: The command, the response tree encoding and the number-space
    translation are complete. The reference backend implements ORDEREDSUBJECT
    and refuses REFERENCES rather than answering it with ORDEREDSUBJECT
    results, which would silently mis-thread a client's view; REFERENCES needs
    a Message-ID graph the backend does not retain.
    The capability is advertised per algorithm — `THREAD=ORDEREDSUBJECT`,
    `THREAD=REFERENCES` — as RFC 5256 section 1 defines it. Until T24 the
    server sent a bare `THREAD` token, which the RFC does not define and which
    told a client nothing about which tree it would get back; a client looking
    for `THREAD=ORDEREDSUBJECT` never matched. An unwitnessed algorithm is now
    refused by the capability gate with a tagged `BAD` before the backend is
    reached, instead of by the backend with a `NO`.

[^srvbinary]: FETCH side. Both memory and Maildir use the public message
    helper for `BINARY[]`, `BINARY.PEEK[]` and `BINARY.SIZE[]`, decoding CTE,
    normalising text lines to CRLF and slicing decoded partial ranges. Unknown
    or malformed CTEs fail with UNKNOWN-CTE. IMAP4REV2 requires the backend-only
    BINARY-FETCH witness, independently of full BINARY advertisement. (#126)
    Whole-message literal8 APPEND, MULTIAPPEND and REPLACE are accepted only
    with full BINARY advertised. CATENATE TEXT uses ordinary literals, as
    specified by RFC 4469.

[^srvmultiappend]: Parsing continues between literals: the parser reads to the
    first literal's announcement, the handler consumes it, then asks the reader
    for the next chunk over the same path continuations and IDLE use. The whole
    command is staged to a temporary file before backend calls. MULTIAPPEND is
    advertised only for `MultiAppendSession`, whose batch must commit atomically
    or leave the mailbox unchanged. Memory implements it; Maildir currently does
    not. RFC 3502's UID-set form of APPENDUID is reported.

[^srvcatenate]: Client text and server-side URLs assembled into one message
    through optional `CatenateSession`. Parts spool to bounded temporary files;
    aggregate APPEND bytes are limited by `Limits.MaxLiteralBytes`. Over-limit
    input returns NO [TOOBIG] without committing, and remaining literals drain
    without resolving further URLs.

[^srvutf8]: UTF8=APPEND is obsolete and is not advertised. RFC 9755's
    UTF8=ACCEPT replaces the obsolete APPEND syntax. Unenabled rev1 clients'
    8-bit APPEND-family headers are rejected by the framework before mutation;
    ENABLE UTF8=ACCEPT or IMAP4rev2 permits them.
    RFC 9755 deprecates UTF8=ALL and UTF8=USER outright — implementing a
    deprecated capability would be a mistake, not an omission. UTF8=ONLY asserts
    the server *refuses* ASCII-only clients, which is a deployment policy this
    framework does not enforce; advertising it while still serving them would be
    a false claim. Everything else in T23's scope is implemented.

[^srvmultisearch]: The ESEARCH command, searching mailboxes the connection has
    not selected. Each result carries its mailbox and UIDVALIDITY, without which
    a UID from another mailbox is meaningless. RFC 7377's source filters
    (`mailboxes`, `subtree`, `subtree-one`, `personal`, `subscribed`,
    `inboxes`, `selected`) are expanded by the framework through LIST and
    NAMESPACE; `inboxes` uses RFC 5465 section 6.3's fallback to `personal`.
    Bare mailbox names are not RFC syntax and receive BAD. The framework
    evaluates extended SEARCH's result options per mailbox, SAVE and
    CONTEXT=SEARCH UPDATE for the selected mailbox only, and MODSEQ from the
    backend's per-UID clocks. Message sequence numbers and `$` are
    resolved against the selection when no IN clause is given and refused when
    one is: a sequence number indexes into a mailbox the client has not selected,
    so there is nothing to resolve it against and answering anyway would return a
    wrong result that looks like a correct empty one.

[^srvpartial]: Framework-owned windowing of an ESEARCH result, including RFC
    9394's negative ranges. It saves wire bytes and client memory, not backend
    work — the backend still evaluates the whole search, and RFC 9394 defines no
    interface that would let it stop early. UID FETCH also supports PARTIAL,
    windowing existing UIDs before CHANGEDSINCE regardless of modifier order.

[^srvfuzzy]: The FUZZY modifier reaches the backend through the open search
    criteria tree with no framework translation, so the capability is purely a
    claim about the backend and is gated on its witness. The RELEVANCY return
    option (sections 4 and 6) is accepted by SEARCH with ESEARCH, SORT with
    ESORT, and MULTISEARCH, and is refused with BAD when the criteria contain no
    FUZZY key. A backend asked through `ReturnRelevancy` may supply scores in
    `SearchResult.Relevancy` or `MultiSearchMailboxResult.Relevancy`; SORT takes
    them from a Search of the same query. Without scores every match is reported
    as 100. The list covers the messages the response returns, as MODSEQ does
    (RFC 4731 section 3.2). Memory matches FUZZY exactly and scores every match
    100. (#202)

[^t26c]: Verified by T26's live tests (`imapclient/ext_cde_interop_test.go`),
    each asserting the data the appended messages imply, not just a tagged OK.
    BINARY (server-decoded base64 with a NUL, and BINARY.SIZE) and MULTIAPPEND
    on Dovecot, Stalwart and Cyrus; CATENATE (client TEXT plus a server-side
    RFC 5092 URL) and COMPRESS=DEFLATE (a 140 KiB literal each way) on Dovecot
    and Cyrus; UTF8=ACCEPT (a non-ASCII mailbox name round-tripped through LIST
    and a non-ASCII BODY search) on Dovecot, Stalwart and Courier; SORT on all
    five, SORT=DISPLAY on Dovecot, Stalwart and Cyrus; THREAD on Dovecot,
    Stalwart, Cyrus and Courier. The COMPRESS run found a deadlock in
    `Client.Close` on an idle compressed connection, fixed with it. Stalwart
    0.11.8 and GreenMail 2.1.9 sort `FROM` by display name rather than by
    address as RFC 5256 requires; that one ordering is logged, not asserted, on
    those two servers.

## Group D — administrative & server-side (task T11)

| Capability | RFC | Client | Server |
|---|---|---|---|
| QUOTA | 9208 | verified [^t26d] | done |
| QUOTA= | 9208 | done | done [^srvquotares] |
| QUOTASET | 9208 | done | done |
| ACL | 4314 | verified [^t26d] | done |
| RIGHTS= | 4314 | done | done [^srvrights] |
| LIST-MYRIGHTS | 8440 | done | done |
| METADATA | 5464 | done | done |
| METADATA-SERVER | 5464 | done | done |
| LIST-METADATA | 9590 | done | done |
| NOTIFY | 5465 | verified [^t26d] [^t27notify] | done [^srvnotify] |
| UNAUTHENTICATE | 8437 | done | done |
| UIDONLY | 9586 | done | done [^srvuidonly] |
| INPROGRESS | 9585 | done [^t27inprogress] | done [^srvinprogress] |
| MESSAGELIMIT= | 9738 | done | deferred [^srvmsglistlimit] |
| SAVELIMIT= | 9738 | done | deferred [^srvmsglistlimit] |
| JMAPACCESS | 9698 | done | done [^srvjmap] |
| UIDBATCHES | 10022 | done [^t31uidbatches] | done [^t31uidbatches] |

[^srvquotares]: Quota resource data uses open `imap.QuotaResourceName` strings.
    Backends declare registered resources with spoken `QUOTA=RES-*` witnesses;
    each registry addition needs a descriptor row, without changing an interface.
    The framework advertises each unfiltered token alongside QUOTA. QUOTASET
    requires at least one resource token; bare QUOTA retains RFC 2087 semantics.
    RES-STORAGE includes STATUS DELETED-STORAGE; RES-MESSAGE includes DELETED.

[^srvrights]: Rights are open `imap.ACLRights` strings in both directions; a
    letter this library does not know passes through verbatim. The framework
    advertises RIGHTS=texk with ACL for the RFC 4314 standard rights; filtering
    ACL also withdraws RIGHTS=.

[^srvuidonly]: All-or-nothing, as the RFC requires. Sequence-number commands are
    refused rather than reinterpreted as UIDs, unilateral FETCH becomes
    UIDFETCH, and EXPUNGE becomes VANISHED because an untagged EXPUNGE carries a
    sequence number and cannot be sent at all. The wire form is RFC 9586
    section 7's `* <uid> UIDFETCH (...)`. `WriteUIDFetchResponse` was added
    to `internal/imapcodec`, sharing the item grammar with `WriteFetchResponse`
    rather than duplicating it.

[^srvnotify]: Delivered through `SessionUpdater`, a session-scoped channel added
    alongside the selection-scoped `Updater` rather than by widening it — which
    SERVER-DESIGN.md §3 names as the trap, since it would let any
    selection-scoped push outlive its selection. Events name a mailbox and carry
    no sequence numbers, because the client has not selected the mailbox and has
    no sequence-number view of it; RFC 5465 §6 reports them as STATUS for that
    reason. The queue coalesces per mailbox. Overflow ends the registration and
    reports NOTIFICATIONOVERFLOW without closing the connection; like NOTIFY
    NONE it then silences the selected mailbox too. The framework validates
    every registration before the backend or the current registration is
    touched: event pairing, one selected specifier and selected-only message
    events are BAD; events it cannot deliver (LIST/METADATA/ANNOTATION) are
    `NO [BADEVENT (MessageNew MessageExpunge FlagChange)]`. It enforces the
    selected mailbox's policy itself — NONE, omitted, per-event and
    SELECTED's immediate removals — withholding suppressed additions and
    removals so sequence numbers stay stable, and a NOTIFY SET returns what
    accumulated. A CONTEXT ADDTO/REMOVEFROM for a message the client knows
    belongs to the event that caused it: suppressed with it, sent after its
    FETCH and before its EXPUNGE, never ahead of a change the session has not
    applied. A new message enters, as it is now, after its EXISTS. Only the
    waiting message's update waits, except that a sorted context keeps a
    message whose removal is still withheld in its place until that EXPUNGE
    is reported. Until then, a match that would enter between that message's
    nearest still-sortable neighbours waits too, and after a COMPARATOR
    command that selects a comparator any match may wait.
    Selected FlagChange carries UID. NONE groups request no
    events, SUBSCRIBED uses current subscriptions, and selected mailboxes
    receive no STATUS. MessageNew fetch attributes on a SELECTED or
    SELECTED-DELAYED group are delivered by the framework (#195). It gates
    them as FETCH items and refuses those that would set \Seen with
    `NO [CANNOT]`; MODSEQ among them enables CONDSTORE. When a drain announces
    new messages in the selected mailbox, at most one `SelectedMailbox.Fetch`
    call supplies their data, sent as unsolicited FETCH responses after the EXISTS,
    numbered in the client's view when written and bounded like FETCH. Their
    order against ESEARCH ADDTO is not guaranteed. Additions carrying the
    origin of the connection's own command are not fetched. A Fetch error or
    an exceeded bound ends the registration with NOTIFICATIONOVERFLOW;
    responses already sent stand. (#54, independent review B3, R3/R5/R6)

[^srvmsglistlimit]: Server advertisements and the incorrect mailbox-size/retention
    API were removed after the independent review (#54, A1). RFC 9738 limits
    messages processed per command; support is deferred until the framework
    enforces that contract, including the associated SEARCH keys and partial
    completion responses.

[^t27notify]: Events for other mailboxes are the unsolicited STATUS and LIST
    responses of RFC 5465 section 5. The client discarded them until T27 added
    `UnilateralDataHandler.Status` and `.List`. They are live-verified: a
    MessageNew in a mailbox the watcher has not selected arrives as STATUS,
    and a CREATE arrives as LIST, on Dovecot and Cyrus. A rename carries the
    previous name in `imap.ListData.OldName`, from the `OLDNAME` extended data
    (section 5.4). This is verified on Cyrus. Dovecot 2.4 sends no MailboxName
    event for RENAME at all, so that check skips there. SubscriptionChange
    (section 5.5) arrives through `.List` as well. T30's decode-once routing
    delivers STATUS for a different mailbox even during an active STATUS
    command, and LIST during legacy LSUB. Scripted regressions cover this
    interleaving; it is not yet live-verified. Metadata delivery and ownership limits:
    - MailboxMetadataChange and ServerMetadataChange arrive as names-only
      METADATA (RFC 5464 §4.4.2) through `UnilateralDataHandler.Metadata`.
      These carry changed entry names, distinct from GETMETADATA's unset,
      empty and present values. Scripted tests cover mailbox/server changes,
      retained callback data, nil handlers, and interleaving with GETMETADATA
      and LIST-METADATA. The live probe found no supporting matrix server:
      Cyrus 3.10.2 advertises METADATA and NOTIFY but omits METADATA from
      ENABLED and explicitly refuses MailboxMetadataChange with BADEVENT;
      James does not enable METADATA notifications. Other profiles lack
      METADATA. Callback delivery remains scripted-only, not live-verified.
    - STATUS for the command's own mailbox is indistinguishable from a result
      and stays with that command. LIST-STATUS still claims STATUS while pending.
    - A LIST event that arrives during a LIST command joins the listing;
      there is no tag or reliable local pattern matcher to distinguish it.

[^t27inprogress]: Until T27 the response code was parsed but never delivered:
    an untagged `OK [INPROGRESS ...]` no command claimed was dropped, and so
    was every other unclaimed status response, including connection-level
    `[ALERT]` text RFC 3501 section 7.1 says must reach the user.
    `UnilateralDataHandler.StatusResponse` now receives them all, with the
    code and its arguments, for `ParseInProgressArgs` to decode. No server in
    the matrix emits INPROGRESS on the short commands the tests issue, so it
    stays `done`.

[^srvinprogress]: The untagged OK progress response shape is framework-owned and
    advertised; no backend surface is required to emit one.

[^srvjmap]: Advertisement and response code only, witnessed by name through
    `CapabilitySupport`. The reference backend serves no JMAP endpoint and so
    does not witness it.

[^t26d]: Verified by T26's live tests. QUOTA (GETQUOTAROOT, and GETQUOTA
    where the server permits it) on Stalwart, GreenMail, Cyrus and Courier.
    Courier-IMAP sends `* QUOTA "ROOT"` without the resource list RFC 9208
    requires, and Apache James 3.8 answers GETQUOTA with no QUOTA response at
    all; the client now reads both as the root with no resources they mean
    rather than failing the command. ACL (MYRIGHTS on the user's own mailbox, GETACL where
    the user holds `a`) on Stalwart, Cyrus and Courier. NOTIFY (an EXISTS pushed
    to a watching session after another session's APPEND, with no command in
    between) on Dovecot and Cyrus; Cyrus needs its `idled` daemon for that,
    which the harness now starts.

[^t31uidbatches]: Published as RFC 10022 on 2026-07-31, the day this table was
    retrieved, so it stayed on the watch list as a draft until the 2026-10-15
    recheck. [T31](tasks/T31-uidbatches.md) implements both directions. The
    client is covered by scripted tests, including pipelined correlators,
    UIDONLY and malformed responses, and loopback tests against our server.
    The server computes batches without a backend interface and tests exact
    boundaries, paging, limits, resend accounting and SEARCHRES preservation.
    The 2026-10-16 native/emulated matrix probe found no UIDBATCHES capability
    on Dovecot, Stalwart, GreenMail, Cyrus, Courier or James; live tests skip
    explicitly. This is not a claim of independently verified interop. The
    client offers no fallback in that release. The UID SEARCH equivalent in
    Appendix A needs sequence numbers, which UIDONLY forbids, and a message
    count the client does not keep. An opt-in fallback would be additive.

## Group E — legacy & niche (task T11, lower priority)

Implement the parse path so responses from servers advertising these do not
break the client; full command support is best-effort.

| Capability | RFC | Client | Server | Note |
|---|---|---|---|---|
| LOGIN-REFERRALS | 2221 | done | done [^srvreferral] | referral response codes parse |
| MAILBOX-REFERRALS | 2193 | done | done [^srvreferral] | as above |
| URLAUTH | 4467 | done | done [^srvurlauth] | GENURLAUTH / URLFETCH / RESETKEY |
| URLAUTH=BINARY | 5524 | done | deferred | server extended URLFETCH syntax is not implemented or advertised |
| URL-PARTIAL | 5550 | done | done | `;PARTIAL=` in IMAP URLs |
| LANGUAGE | 5255 | done | done | |
| I18NLEVEL=1 | 5255 | done | done | capability probe |
| I18NLEVEL=2 | 5255 | done | done [^srvi18n2] | COMPARATOR command |
| CONTEXT=SEARCH | 5267 | done [^t27context] | done [^srvcontext] | CANCELUPDATE + RETURN keywords |
| CONTEXT=SORT | 5267 | done [^t27context] | done [^srvcontext] | as above |
| ESORT | 5267 | verified [^esort] | done [^srvesort] | `SortExtended` / `SortExtendedUID` |
| FILTERS | 5466 | done | done [^srvfilters] | UNDEFINED-FILTER parse |
| CONVERT | 5259 | deferred | deferred | no known server support |
| IMAPSIEVE= | 6785 | deferred | deferred | server-side; parse only |
| ANNOTATE-EXPERIMENT-1 | 5257 | deferred | deferred | superseded by METADATA |

[^srvreferral]: No code beyond the advertisement. Referrals are response *codes*,
    and `imap.ResponseCode` is an open string type carried on `*imap.Error`, so
    a backend returning one is already relayed verbatim. API rule 5 paying off:
    the extension is a data change, not a type change.

[^srvurlauth]: GENURLAUTH, URLFETCH and RESETKEY, with tokens minted and verified
    by the backend since only it holds the per-mailbox secret. Memory validates
    rump/full URL structure, key owners, access identifiers, expiry and
    UIDVALIDITY. HMAC verification uses exact original rump bytes, with a
    constant-time comparison of decoded hex tokens. Validated URLs resolve
    through the owner's account, including stored BODY sections and partial
    ranges. Keys cannot survive a deleted/recreated mailbox identity. The
    backend grants no submit role. Scripted/direct and own-client coverage;
    independent live implementations are not claimed. (#162)

[^srvi18n2]: The COMPARATOR command, through the optional `ComparatorSession`.
    Comparator names are an open RFC 4790 registry and cross the boundary as
    strings. A request naming nothing servable returns BADCOMPARATOR rather than
    a generic NO, so a client can tell it apart and fall back.

[^t27context]: Until T30 the client sent RETURN (UPDATE) and CANCELUPDATE but
    discarded the unsolicited ESEARCH responses with ADDTO and REMOVEFROM that
    carry the updates (RFC 5267 §4.3). One that arrived while another extended
    SEARCH or SORT was pending was claimed by that command and failed it, so
    `SortExtended` refused UPDATE. A searching command now registers its tag
    before the request is written, and updates are routed by that tag before
    any command sees them. They arrive at `UnilateralDataHandler.ContextUpdate`
    as ordered items, repeated and unknown keys included, with typed
    position/set pairs. `ESearchData.ContextTag` names the context for
    `CancelUpdate`. `SortExtended` accepts UPDATE under CONTEXT=SORT, and
    MULTISEARCH registers it for the selected mailbox (RFC 7377 §2.2).
    `CancelUpdate` documents when a registration ends. Scripted tests
    interleave old updates with extended SEARCH, extended SORT, MULTISEARCH
    and other commands, and cover cancellation, NOUPDATE, selection changes,
    logout and disconnect. Live, `TestContextUpdateRemovalInterop` receives
    REMOVEFROM after an expunge on Dovecot 2.4.3, the only matrix server that
    advertises CONTEXT=SEARCH. One server is not two, so the row stays `done`.
    No matrix server advertises CONTEXT=SORT; sorted contexts are covered by
    scripted tests only. Not covered: `SearchPartial` does not register UPDATE
    (#48), and `*` in an update set fails in the generic value capture (#49).

[^srvcontext]: `RETURN (UPDATE)` registers the complete SEARCH or SORT result,
    including when PARTIAL limits the initial response. After queued mailbox
    changes, the event loop refreshes it through the existing SEARCH or SORT
    operation, after mutations return. ADDTO and REMOVEFROM preserve the query's
    order and UID/sequence-number mode; sorted results insert and remove each
    entry at its own position, and a reorder, as after a COMPARATOR change,
    replaces the list from position one. An expunged match is removed before
    its EXPUNGE; other changes follow the FETCH or EXISTS that caused them.
    CANCELUPDATE and selection teardown end the registration. Reusing an active
    UPDATE tag gets BAD; refresh failure sends untagged NO [NOUPDATE "tag"] and
    drops the context. No additional backend interface is required.

[^esort]: Until T27 this row overstated: the client detected the capability
    but could not send `SORT RETURN (...)` at all. `Client.SortExtended` and
    `SortExtendedUID` now issue it and return ESEARCH data, falling back to a
    plain SORT for MIN, MAX, ALL and COUNT when only SORT is advertised.
    Verified live on Dovecot 2.4.3 and Cyrus 3.10: ALL arrives in the order a
    plain SORT returns. RFC 5267 section 3.1 calls MIN and MAX "the
    lowest/highest sorted message", which Dovecot reads as positions in the
    sort order and Cyrus and Stalwart as numeric extremes; the client passes
    either through. Stalwart 0.11.8 also answers ALL with fewer numbers than
    its own COUNT, which the test records as a server deviation.

[^srvesort]: The ESEARCH-shaped response for SORT. MIN and MAX are the ends of
    the *sorted* order rather than the numerically smallest and largest, and ALL
    preserves that order rather than collapsing it into ranges, which would
    re-sort it.

[^srvfilters]: Closed the gap the client's FILTERS work escalated to T02:
    `imap.SearchFilter` now exists, which is an *additive* root-package change
    and therefore permitted after v1.0 — nothing was reshaped. The framework
    substitutes a named filter into the criteria tree before the backend
    evaluates it, so a backend implements a lookup and nothing else. An
    undefined name returns UNDEFINED-FILTER rather than an empty result, since
    a client cannot otherwise tell "no such filter" from "nothing matched".
    `imapmessage.Match` errors on an unsubstituted FILTER rather than matching
    nothing, so a substitution bug surfaces instead of hiding.

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

Nothing is waiting here. A capability whose specification is still a draft
stays on this list. Do not implement against a draft: if it changes before
publication, following it would break the frozen v1 API. Recheck the registry
before each release.

UIDBATCHES left the list when it was published as RFC 10022. Its row is in
group D.

[^srvscram]: Server side added by T23 through the optional `SCRAMCredentials`
    backend interface. The exchange, nonces and proof verification are
    framework-owned; the backend supplies salt, iteration count, StoredKey and
    ServerKey, and never sees a password. Verified end to end against the real
    client, which checks the server's signature — so a passing exchange is
    mutual authentication, not just a server that said OK. SCRAM is permitted on
    cleartext without the `AllowInsecureAuth` opt-in PLAIN needs, because no
    password crosses the wire.

[^srvscramplus]: Implemented, binding to RFC 9266's tls-exporter. Both
    directions of RFC 5802 §6's downgrade defence are enforced: a `y` GS2 header
    is refused whenever `-PLUS` is advertised, since that belief can only come
    from tampering, and a `-PLUS` mechanism must present binding data matching
    the connection. The stored derivation is shared with the unbound form —
    `-PLUS` changes the exchange, not the credential.
