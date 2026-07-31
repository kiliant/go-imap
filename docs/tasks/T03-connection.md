# T03 — Connection & session

**Agent:** `client-core` · **Milestone:** M1 · **Depends on:** T01, T02

**Owns:** `imapclient/client.go`, `imapclient/conn.go`, `imapclient/state.go`

## Goal

The connection layer and the command dispatch model. You are defining the method
shape every later task copies, so your API mistakes are the expensive ones. Read
`docs/API-STABILITY.md` in full first.

## Deliverables

- `Dial`, `DialTLS`, `DialStartTLS`, `NewClient(net.Conn, *Options)`
- Greeting handling: `OK`, `PREAUTH`, `BYE`, with the capability list that
  servers often include in the greeting
- Reader goroutine that demultiplexes tagged vs untagged responses
- Command handles: issued immediately, `Wait(ctx)` blocks for completion
- Connection state machine: not-authenticated → authenticated → selected →
  logout, rejecting invalid commands locally rather than on the wire
- `UnilateralDataHandler` as a struct of function fields — never an interface, so
  a future unsolicited-response RFC adds a field rather than breaking implementers
- Wire-trace/debug hook with credential redaction
- `Close`, `Logout(ctx)`

## The concurrency model is the design

IMAP interleaves tagged completions with unsolicited untagged data. A lock-step
request/response client is wrong and cannot be fixed later without an API break.
Commands pipeline; one reader goroutine owns the decoder and routes:

- tagged → pending-command map
- untagged, command-scoped → in-flight command's collector
- untagged, connection-scoped (`EXISTS`, `EXPUNGE`, `RECENT`, `FETCH` flag
  updates) → `UnilateralDataHandler`

The split between the last two is not always obvious and depends on which command
is in flight. Document the rule you implement.

## Cancellation — settle it here, once

IMAP has no general command-abort. Cancelling an in-flight command **poisons the
connection**: close it and return `context.Canceled` rather than desynchronising
the stream. `IDLE` is the only clean cancel (`DONE`, handled in T07). Write this
into the package doc; later tasks must not re-litigate it per command.

## Security defaults

- TLS 1.2 minimum, certificate verification on, SNI set.
- `InsecureSkipVerify` only via an explicit documented option field. Never a
  default, never inferred.
- After `STARTTLS`: **discard the pre-TLS capability list and re-issue
  `CAPABILITY`.** Trusting the cleartext list is a downgrade vector. Test it.
- Never log credentials. Assert redaction in a test.

## API constraints

`ctx` first on every blocking method including `Logout`. Options structs, `nil`
valid. No exported interfaces. All protocol errors are `*imap.Error`.

## Done when

Connects to Dovecot in the interop harness, completes the greeting, issues
`CAPABILITY`, `NOOP` and `LOGOUT`, handles a mid-command `BYE` cleanly, and
passes `-race`. `api-guardian` approves the method shape.
