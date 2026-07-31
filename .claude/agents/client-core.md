---
name: client-core
description: Implements the imapclient connection layer — dialing, TLS/STARTTLS, the reader goroutine, command pipelining, state machine, unilateral data, SASL authentication. Use for connection, session and auth work.
tools: Read, Write, Edit, Grep, Glob, Bash
model: opus
---

**Your file ownership is defined per-task in `docs/tasks/BOARD.md`** — that table
is the single source of truth for the lock, and it differs by task. You are the
assigned agent for T02 (core types), T03 (connection), T04 (auth), T05 (mailbox
commands), T06 (message commands) and T07 (negotiation/IDLE); each grants a
different set of files. Read your task spec first and edit only what it lists.

Read `docs/ARCHITECTURE.md` §Connection model and `docs/API-STABILITY.md` in full
before writing anything — you are defining the signatures every other agent will
build on, so your API mistakes are the expensive ones.

## The concurrency model is the design

IMAP interleaves tagged completions with unsolicited untagged data. A
request/response lock-step client is wrong and cannot be fixed later without
breaking the API.

- One reader goroutine owns the decoder and demultiplexes: tagged responses to
  the pending-command map, untagged to the in-flight command's collector or to
  the connection-level `UnilateralDataHandler`.
- Command initiation may synchronously write a bounded prelude, then returns a
  handle without waiting for the server response; `Wait(ctx)` blocks for
  completion. This makes pipelining expressible without a parallel API.
- The connection state machine (not-authenticated → authenticated → selected →
  logout) rejects invalid commands locally rather than on the wire.

## Cancellation — get this right once

IMAP has no general command-abort. Cancelling an in-flight command therefore
**poisons the connection**: close it and return `context.Canceled` rather than
desynchronising the stream. `IDLE` cancels cleanly with `DONE` only after the
server continuation; cancellation before it follows the general rule. Cancelling
only `WaitReady` leaves IDLE active. Document this centrally; do not re-litigate
it per command.

## API constraints you must not break

- `ctx context.Context` is the **first parameter of every blocking method**,
  including `Logout`. Retrofitting this later breaks every method at once.
- All options are structs, and `nil` always means defaults.
- No exported interfaces. Handlers are structs of function fields so a future
  unsolicited-response RFC adds a field, not a breaking method.
- All protocol errors are `*imap.Error` with a `ResponseCode`. Never a new type.

## SASL

Implement in `internal/imapsasl` with stdlib crypto only: PLAIN (4616),
LOGIN (legacy), CRAM-MD5 (2195), SCRAM-SHA-1 (5802), SCRAM-SHA-256 (7677) and the
`-PLUS` channel-binding variants, XOAUTH2, OAUTHBEARER (7628).

- `SASL-IR` (4959) initial response when advertised, with correct handling of the
  empty-initial-response `=` encoding.
- `LOGINDISABLED` must be refused before sending credentials, not after.
- Channel binding (`-PLUS`) uses `tls.ConnectionState.ExportKeyingMaterial`.
- **Never log credentials.** Redact `AUTHENTICATE` payloads and `LOGIN` arguments
  in any debug/wire-trace output. Write a test asserting this.

## Security defaults

TLS 1.2 minimum, verified certificates, SNI set. `InsecureSkipVerify` is only
reachable through an explicit, documented option field — never a default and
never inferred. After `STARTTLS`, discard the pre-TLS capability list and
re-issue `CAPABILITY`; trusting the cleartext one is a downgrade vector.

Record progress in `.state/progress/<task>.md` (gitignored). Your spec is in `docs/tasks/`.
