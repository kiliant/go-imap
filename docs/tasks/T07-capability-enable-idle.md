# T07 — CAPABILITY, ENABLE, IDLE, IMAP4rev2

**Agent:** `client-core` · **Milestone:** M2 · **Depends on:** T05, T06

**Owns:** `imapclient/capability.go`, `enable.go`, `idle.go`

## Goal

Capability negotiation and the rev1/rev2 bridge. This task gates every extension
task, because they all gate on advertised capabilities.

## CAPABILITY

- Parse from the greeting, from the `CAPABILITY` command, and from `[CAPABILITY]`
  response codes in `OK` replies.
- **Re-fetch after `STARTTLS` and after authentication.** The pre-TLS list is
  untrusted (downgrade vector) and the pre-auth list is incomplete.
- Store as an open string-keyed set — never a bitfield or enum. New capabilities
  appear constantly; an unknown one must be queryable, not discarded.
- Handle parameterised capabilities: `AUTH=`, `UTF8=`, `QUOTA=`, `RIGHTS=`,
  `CONTEXT=`, `APPENDLIMIT=`, `MESSAGELIMIT=`, `SAVELIMIT=`, `IMAPSIEVE=`. The
  value part matters and must be retrievable.

## ENABLE (RFC 5161)

- `ENABLE IMAP4rev2`, `ENABLE CONDSTORE`, `ENABLE QRESYNC`, `ENABLE UTF8=ACCEPT`.
- Only valid in the authenticated state, before selecting a mailbox. Enforce it.
- Track what was actually enabled — the server's `ENABLED` response may be a
  subset of what was requested.

## IMAP4rev2 (RFC 9051)

See `docs/ARCHITECTURE.md` — the settled decision is rev1 wire baseline, rev2
behaviour when enabled, **rev2 shape presented in the public API and emulated on
rev1 servers**. Differences to absorb:

- rev2 folds `ESEARCH` syntax into base `SEARCH` responses
- `UIDPLUS`, `MOVE`, `LIST-EXTENDED`, `SPECIAL-USE`, `ENABLE`, `IDLE`,
  `NAMESPACE`, `SASL-IR`, `BINARY` are all mandatory in rev2 — do not gate them
  on a capability that a rev2 server will not bother advertising
- `LSUB` and `RECENT` are removed; `CHECK` is deprecated
- `STATUS SIZE` semantics differ

## IDLE (RFC 2177)

- The one command with a clean cancellation path (`DONE`) after the server has
  sent its continuation. Before that continuation, cancellation follows T03 and
  poisons the connection; cancelling only `WaitReady` leaves IDLE active.
- **Re-issue before the 29-minute limit** the RFC recommends; many servers and
  more middleboxes drop idle connections sooner. Make the interval configurable
  with a sane default (~25 min) and handle the re-issue transparently.
- Unsolicited data during IDLE routes to the `UnilateralDataHandler`.
- Fall back to `NOOP` polling when `IDLE` is unadvertised — document the fallback
  and its latency cost.
- Racing `DONE` against an arriving untagged response is the classic IDLE bug:
  the response may arrive after `DONE` is sent but before the tagged completion.
  Test it explicitly.

## Done when

rev2 negotiated against Stalwart, rev1 against Dovecot and GreenMail, with the
same public API behaviour. IDLE receives a push notification within 1 s of a
message appended by a second connection. `-race` clean.
