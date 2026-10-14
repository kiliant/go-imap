# T29 — LIST extended data, the SELECT LIST claim, opt-in password-only SASL

**Agent:** `extensions` + `client-core` + `api-guardian` · **Milestone:** post-M6 (root `v1.2.0` / `imapserver/v0.3.0`) · **Depends on:** T27

**Owns:**
- the `ListData` declaration and a new `ListExtendedItem` type in the root package (`mailbox.go`)
- `imapclient/list.go` (`readListExtendedData`, its helpers, and the `ListExtendedItem` alias), `selectCollector` in `imapclient/select.go`, and the SASL selection in `imapclient/auth.go`
- `imapserver/cmd_list.go`, `imapserver/ext_a_list.go`, the `ListOptions`/`ListWriter` documentation in `imapserver/backend.go`, the RECURSIVEMATCH parts of `imapserver/memory/**` and `imapserver/backendtest/**`
- new test files named below, the LIST-EXTENDED rows in `docs/RFC-COVERAGE.md`, and the matching `CHANGELOG.md` entries
- the v2-candidates note in `docs/ROADMAP.md`, which records item 3's default flip
- by hand-over from completed tasks (BOARD's lock-passing rule, named in both
  specs): `cmd/imapcli/auth.go` and its mechanism-choice test in
  `cmd/imapcli/backup_test.go` from [T28](T28-imapcli-backup.md), and
  `TestUnilateralListOldName` in `imapclient/conn_test.go` and the
  `UnilateralDataHandler.List` doc comment from
  [T27](T27-client-gaps-from-t11.md)
- the T29 precedent notes in `docs/API-STABILITY.md` (rules 1 and 9, and the
  versioning policy), which api-guardian required

T27 left these three items open after its review. All three change library
behaviour or API, so all three go through `api-guardian` before merge. T27
(PR #17) and T28 (PR #18) are both merged into `main`, and this task builds
on both.

## 1. SELECT claims a LIST only on an IMAP4rev2 session

**Today (T27).** `selectCollector` treats as the SELECT's own response any
LIST whose name, or OLDNAME, normalises to the selected mailbox. It forwards
every other LIST to `UnilateralDataHandler.List`.

**Problem.** Only RFC 9051 section 6.3.2 makes a server send a LIST during
SELECT, and it does so only once IMAP4rev2 is enabled. On a rev1 session,
a LIST that arrives during SELECT can only be an event, such as a NOTIFY
MailboxName or SubscriptionChange (RFC 5465 sections 5.4 and 5.5). The name
match swallows exactly those events for the selected mailbox. The guardian
recorded this as T27's last observation.

**Change.**
- Read whether IMAP4rev2 is enabled when a LIST arrives. Claim a LIST only
  on a rev2 session, by name or OLDNAME, and at most once. (This spec first
  said to snapshot it when the command is issued. The api-guardian review
  found that a snapshot can race ENABLE's completion, which records
  IMAP4rev2 after ENABLE has left the pending queue. The reader goroutine
  runs that completion before it decodes any later response, so reading on
  arrival is exact.)
- On a rev1 session, forward every LIST.
- Document the ambiguity rev2 leaves: an event for the selected mailbox
  that arrives before the SELECT's own LIST looks identical to it, for
  example a SubscriptionChange. No client can tell the two apart.

**Verify before relying on it.** For every native matrix server, check
whether it sends a LIST during SELECT on a rev1 session. Dovecot and
Stalwart advertise IMAP4rev2 and are the likely ones.
- A server that does would now produce a spurious `List` event. Record it
  as a server deviation in a test, and decide whether a per-server
  tolerance is needed. Do not assume.
- Test: an interop test that SELECTs with a `List` handler installed, with
  and without `ENABLE IMAP4rev2`, and checks what reaches the handler.

**Unit tests.**
- Rev1: a LIST for the selected mailbox during SELECT reaches `List`.
- Rev2: the existing `TestSelectClaimsItsOwnList*` tests keep passing.

## 2. LIST extended data in both directions

### What the RFCs say

Checked 2026-10-14 against the text and the IANA registry; nothing here is
from memory.

- **RFC 5258 section 3.5, CHILDINFO.**
  - "MUST NOT be returned unless the client has specified the
    RECURSIVEMATCH selection option."
  - It "describes the selection criteria that has caused it to be
    returned".
  - Servers "SHOULD ONLY return a non-matching mailbox name along with
    CHILDINFO if at least one matching child is not also being returned".
- **RFC 5258 section 6, grammar.**
  - `childinfo-extended-item = "CHILDINFO" SP "(" list-select-base-opt-quoted *(SP list-select-base-opt-quoted) ")"`.
    The values are quoted base selection options, e.g. `("SUBSCRIBED")`.
  - `mbox-list-extended-item = mbox-list-extended-item-tag SP tagged-ext-val`.
    `tagged-ext-val` is defined in the same section.
- **RFC 5258 section 3.**
  - "The client MUST ignore all extended fields it doesn't recognize."
  - The server "MUST NOT return any extended data item unless the client
    has expressed its ability", e.g. by using extended LIST syntax.
- **IANA "LIST-EXTENDED extended data item" registry** (RFC 5258 section 9):
  CHILDINFO (RFC 5258 section 3.5) and OLDNAME (RFC 5465 section 5.4;
  RFC 9051). Those are the only two entries.

### What the code does today

- **Client.**
  - `ListOptions` can send RECURSIVEMATCH (`ListSelectRecursiveMatch`).
  - `readListExtendedData` keeps only OLDNAME and drops CHILDINFO, the one
    thing RECURSIVEMATCH exists to deliver. The client half of
    RECURSIVEMATCH is therefore unusable.
  - The LIST-EXTENDED client cell reads `verified`, which overstates this.
- **Server.**
  - `imapserver/ext_a_list.go` accepts RECURSIVEMATCH and sets
    `ListOptions.SelectRecursiveMatch`.
  - Nothing ever writes CHILDINFO. `imapserver/memory` ignores the option
    entirely, so it returns no parent whose descendant matches.
  - `ext_a_list.go:155` also accepts RECURSIVEMATCH beside REMOTE alone.
    Check that against the section 6 grammar: REMOTE is an independent
    option, not a base option.

### Proposed API (additive; for api-guardian)

```go
// in package imap, on ListData:

	// ChildInfo is the CHILDINFO extended data item: the selection options a
	// descendant of this mailbox matched, which is why a mailbox that does
	// not match itself was returned. Upper-cased as sent, e.g. "SUBSCRIBED".
	// Only a LIST with the RECURSIVEMATCH selection option carries it.
	// RFC 5258 section 3.5.
	ChildInfo []string

	// Extended holds the extended data items this library does not model,
	// in wire order, so a future item is kept rather than dropped.
	// RFC 5258 section 6 (mbox-list-extended-item).
	Extended []ListExtendedItem

// ListExtendedItem is one LIST extended data item kept as sent.
//
// Construct with keyed fields only; fields may be added in a future release.
type ListExtendedItem struct {
	// Tag is the item tag, upper-cased.
	Tag string
	// Value is the tagged-ext-val exactly as it appeared on the wire.
	Value []byte
	_     struct{}
}
```

Decisions for the guardian, with the reasoning so far:

- **`ChildInfo []string` or a typed root vocabulary.** The client's
  `imapclient.ListSelectOptionKeyword` cannot become an alias of a new root
  type: that changes type identity, which apidiff rejects (API-STABILITY on
  aliases). A plain open `[]string` avoids a second parallel vocabulary.
  Prefer it unless the guardian sees a reason a server needs typed values.
- **Is `Extended` worth having?** The RFC lets a client ignore unknown
  items. The README's promise is that data the library cannot model is
  preserved, not dropped, as `FetchDataRaw` and `ESearchData.Values` do.
  Keeping raw bytes costs one capture per unknown item.
  `imapwire.Decoder.CaptureValue` already exists; FETCH uses it.
- **Server direction for `Extended`.** It is never written. Raw bytes from a
  backend cannot be validated or gated on negotiation, the guardian's rule
  from T27 round two. Document it on `ListWriter.WriteList`, as `OldName`
  already is.

### Client

- **`readListExtendedData`** keeps every item raw in `Extended`, OLDNAME and
  CHILDINFO included, and decodes `OldName` and `ChildInfo` from the
  captured bytes. Keep T27's leniency: anything unparseable costs the
  extended data, never the response. (This spec first proposed `Extended`
  for unmodelled items only. The api-guardian review rejected that: when a
  later release learns to decode an item, it would vanish from `Extended`
  for exactly the callers reading it there.)
- **Repeats.** Of repeated or malformed OLDNAME and CHILDINFO items, the
  first that decodes is used. Every one is kept raw.
- **Fuzz.** Extend `FuzzUnilateralStream`'s seeds, or add a LIST-response
  fuzz target, with CHILDINFO, an unknown item containing a literal, nested
  lists, and malformed values. The fuzz files are T13's pattern, so name
  the request in the progress notes.

### Server

- **Framework (`cmd_list.go`, `ext_a_list.go`).** `writeListData` writes
  CHILDINFO only when the LIST command used RECURSIVEMATCH. The values
  written are only the base selection options that were requested (RFC 5258
  section 3.5). This is decided from the parsed arguments, not from the
  backend.
  - LSUB, the RFC 9051 SELECT LIST and a plain rev1 LIST never carry it.
  - `writeListData` therefore needs the command's arguments, or a computed
    "allowed extended items" value, passed in.
- **Backend contract (`backend.go` docs).** With
  `ListOptions.SelectRecursiveMatch`, a backend returns a parent that does
  not match but has a matching descendant. It sets `ListData.ChildInfo` to
  the options the descendant matched, and the framework intersects that
  with what was requested.
- **`imapserver/memory`.** Implement RECURSIVEMATCH together with SUBSCRIBED
  (RFC 5258's own examples) and with SPECIAL-USE, if RFC 6154 allows that
  pairing. Check the RFC first.
- **`backendtest`.** Add a RECURSIVEMATCH conformance case, so a backend
  that ignores the option fails it.
- **Fix** the REMOTE-plus-RECURSIVEMATCH acceptance if the grammar check
  above confirms it is wrong.

### Interop

Use the RFC 5258 section 3.5 scenario, on every server that advertises
LIST-EXTENDED (Dovecot, Stalwart and Cyrus are likely):
1. Subscribe to a child whose parent is not subscribed.
2. Issue `LIST (SUBSCRIBED RECURSIVEMATCH) "" "*"`.
3. Expect the parent with `CHILDINFO ("SUBSCRIBED")`.

A server that omits the item is recorded, not failed. `goimap`, our own
server in the matrix, must pass once the server half lands.

### Release coupling

`ChildInfo` and `Extended` are root fields. The server half needs root
`v1.2.0`, so it cannot ship in an imapserver release against `v1.1.0`. The
order is root `v1.2.0`, then the `require` bump, then imapserver
(`docs/RELEASING.md`). If the root release should not wait for this task,
land the client half first and the server half in a later imapserver-only
PR.

## 3. Opt-in password-only SASL selection

**Today.**
- `Client.Authenticate` with no `Mechanism` picks from `saslPreference`
  (`imapclient/auth.go`):
  SCRAM-SHA-256-PLUS, SCRAM-SHA-256, SCRAM-SHA-1-PLUS, SCRAM-SHA-1,
  OAUTHBEARER, XOAUTH2, CRAM-MD5, PLAIN, LOGIN.
- With `Token` empty, the password is used as the bearer token. That is
  documented on `AuthenticateOptions.Token`.
- On a server offering OAUTHBEARER or XOAUTH2 beside PLAIN, a caller with a
  real password is therefore refused. This hits Stalwart's default
  configuration, and likely Gmail and Microsoft 365 with app passwords.
- T28 worked around it inside imapcli (`cmd/imapcli/auth.go`).

**Why not change the default.**
- The fallback is documented v1 behaviour, and callers who pass a token as
  the password with nil options rely on it.
- Flipping it would break their logins without any compile error.
- Record the flip as a v2 candidate instead; `docs/ROADMAP.md` already
  defers incompatible changes to v2.

**Proposed API (additive; for api-guardian).**

```go
// Credential says what Authenticate's password argument holds. It is an
// open, string-backed type, so a future kind — a client certificate for
// EXTERNAL, say — is a new constant, not a new field.
type Credential string

const (
	// CredentialPassword restricts the choice to mechanisms that take a
	// password: SCRAM-*, CRAM-MD5, PLAIN and LOGIN.
	CredentialPassword Credential = "password"
	// CredentialBearerToken restricts it to OAUTHBEARER and XOAUTH2. The
	// token is Token if set, otherwise the password argument.
	CredentialBearerToken Credential = "bearer-token"
)

// on AuthenticateOptions:
	// Credential, when set, restricts automatic mechanism selection to
	// mechanisms that take this kind of credential. Empty keeps the v1
	// behaviour, which can send a password as a bearer token.
	Credential Credential
```

**Semantics to decide and pin in tests.**
- **Unset (`""`):** exactly today's behaviour, byte for byte. A test pins
  this.
- **`CredentialPassword`:** automatic selection takes `saslPreference`
  filtered by the existing `passwordMechanisms` set.
  - An explicit `Mechanism` naming a built-in OAuth mechanism is an error.
  - A non-built-in `Mechanism` with a `SASL` callback is allowed: the caller
    supplied that mechanism.
  - A non-empty `Token` is an error, since the two settings contradict each
    other.
- **`CredentialBearerToken`:** OAUTHBEARER, then XOAUTH2. An explicit
  password `Mechanism` is an error.
- **Unknown value:** an error, never a guess. An older library must not
  silently treat a newer kind as something else. `searchReturnKeywords`
  takes the same stance: it refuses a `SearchReturnOption` implementation it
  does not model.
- **No matching mechanism advertised:** an `*imap.Error`, as today with
  `authProtocolError`. Say in the doc that `Login` (the LOGIN command)
  remains the fallback.

**Docs.**
- A warning on `Client.Authenticate` and `AuthenticateOptions.Token` that,
  unset, a password can go out as a bearer token. Point to `Credential`.
- CHANGELOG. A note in `docs/API-STABILITY.md` if the guardian thinks
  "opt-in now, default flip at v2" sets a precedent.

**Tests.**
- **Unit:** each kind against capability sets — OAuth+PLAIN, SCRAM+OAuth,
  OAuth only, password only — and every conflict case.
- **Interop:**
  - On Stalwart, with OAUTHBEARER and PLAIN advertised,
    `CredentialPassword` logs in with the interop password.
  - The existing OAUTHBEARER test passes with `CredentialBearerToken`.

**Follow-up.** `cmd/imapcli/auth.go` drops its own selection and sets
`Credential: CredentialPassword`. That file belonged to T28, which is done;
this spec and T28's both name the hand-over (BOARD's lock-passing rule).

## Follow-ups outside this task

Found by the api-guardian review, recorded here so a later task picks them up.
Each is tracked as a GitHub issue; the bugs found along the way are #22 and
#23, and the missing `imapserver/v0.2.0` tag is #24.

- **imapserver: witness the LIST-EXTENDED selection options** (#20). LIST-EXTENDED
  is witnessed by the framework alone (`imapserver/capability.go`), so
  `ListOptions.SelectRecursiveMatch` and `SelectRemote` reach every backend,
  whether or not it implements them. RECURSIVEMATCH is something only a
  backend can evaluate, and API-STABILITY section 10 says such behaviour must
  be witnessed. The pressure is the LIST-EXTENDED option registry (RFC 5258
  section 9.3): every option registered there would land on an always-active
  feature and be silently ignored by older backends. A `CapabilitySupport`
  witness, with RECURSIVEMATCH refused when it is missing, is the likely
  shape. This predates T29. imapserver is on v0.x, so the change is allowed.
  `backendtest` already checks RECURSIVEMATCH, so a backend that claims the
  witness and ignores the option fails there.
- **imapclient: a code for "no mechanism takes this credential"** (#21).
  `Authenticate` refuses locally when no advertised mechanism takes the
  `Credential` kind, but the `*imap.Error` it returns has no `Code`. A caller
  can tell it apart only by its text. So `cmd/imapcli/auth.go` keeps its own
  copy of the password-mechanism list to decide when to fall back to LOGIN.
  If the library adds a password mechanism, such copies fall out of date:
  the caller then uses LOGIN where SASL was available, which degrades the
  login but does not break it. `imap.Error.Code` is an open string, so a
  code can be added later without a break. Worth doing if a second caller
  needs the same distinction.

## Done when

- **Item 1:** rev1 forwards and rev2 claims. Unit tests cover both, and the
  per-server rev1 SELECT behaviour is recorded from a live run.
- **Item 2:**
  - `ChildInfo` and `Extended` are filled by the client.
  - CHILDINFO is written by the server only under RECURSIVEMATCH.
  - `memory` implements RECURSIVEMATCH, and `backendtest` checks it.
  - Interop covers the RFC 5258 section 3.5 scenario on the servers that
    support it, `goimap` included.
  - Fuzz seeds cover the new parse paths.
  - The LIST-EXTENDED coverage cells say what is true.
- **Item 3:** `Credential` is in place with the semantics above. The unset
  behaviour is pinned unchanged, and Stalwart password login is verified.
- **All items:**
  - `apidiff` reports only compatible changes in both modules.
  - The guardian has approved.
  - Each item is a separate commit that builds on its own.
