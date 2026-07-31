# Architecture

## Layering

```
github.com/kiliant/go-imap          package imap
    core vocabulary: flags, mailbox attributes, envelope, body structure,
    search criteria, fetch items, status items, response codes, *Error.
    NO I/O. NO imports of sibling packages.

    ├── internal/imapwire    lexer/decoder/encoder for the IMAP wire grammar
    ├── internal/imapsasl    SASL mechanisms
    ├── internal/saslprep    SASLprep (RFC 4013) credential preparation
    ├── internal/unicodenorm NFC/NFKC normalisation, generated tables
    └── imapclient           package imapclient — the client
```

`internal/unicodenorm` sits below `internal/saslprep` and imports nothing outside
the standard library, so the normalisation tables stay reusable by anything else
that needs them (`UTF8=ACCEPT` comparison, a future server framework) without
either package growing a dependency on the client.

Dependencies point downward only. The reason `package imap` holds the vocabulary
and does no I/O is forward-looking: the server framework (milestone M5) reuses
exactly these types. If envelope/body-structure types lived in `imapclient`, the
server would either import a client package or duplicate them — and fixing that
later is a breaking change. The split costs nothing now and buys the option.

## Decision: IMAP4rev1 wire compatibility, IMAP4rev2 behaviour when enabled

**Status: settled. Do not revisit without human approval.**

RFC 9051 (IMAP4rev2) is the current standard, but the deployed server population
is overwhelmingly RFC 3501 (IMAP4rev1). A rev2-only client cannot be tested
against most of the interop matrix, and cannot be used against most real
mailboxes.

Therefore:

- The parser accepts the rev1 grammar as its baseline, which is a superset of
  what rev2 servers send in practice.
- rev2-specific behaviour activates via `ENABLE IMAP4rev2` (RFC 5161) when the
  server advertises `IMAP4REV2`.
- Differences the client must absorb rather than expose: rev2 folds `ESEARCH`
  response syntax into base `SEARCH`; makes `UIDPLUS`, `MOVE`, `LIST-EXTENDED`,
  `SPECIAL-USE`, `ENABLE`, `IDLE`, `NAMESPACE`, `SASL-IR`, and `BINARY`
  mandatory; removes `LSUB` and `RECENT`; deprecates `CHECK`; and changes
  `STATUS` size semantics. The public API presents the rev2 shape and emulates
  it on rev1 servers where possible.

The cost of retrofitting rev1 support onto a rev2-only client is a breaking
change to response handling, so this belongs in the foundation.

## Decision: no external dependencies

Standard library only, including test code. Rationale: a v1.0 stability promise
cannot be stronger than the weakest dependency's. Everything needed is reachable
with stdlib — `crypto/tls`, `compress/flate` (COMPRESS=DEFLATE), `crypto/hmac`
+ `crypto/sha256` + `golang.org/x/crypto/pbkdf2`-equivalent (SCRAM, implementable
on stdlib), `mime` and `encoding/base64`.

The one case where the stdlib genuinely lacks the primitive is Unicode
normalisation, which SASLprep (RFC 4013) requires and which normally means
`golang.org/x/text`. Resolved by generating the NFC/NFKC tables into
`internal/unicodenorm` as Go source: generated code committed to the tree is not
a dependency, so the rule holds without an exception. The generators live in
`internal/{unicodenorm,saslprep}/gen/` and are run by hand, not at build time —
`go generate` reaching the network during a build would reintroduce exactly the
fragility the rule exists to prevent.

The table versions differ on purpose: normalisation tracks the toolchain's
`unicode.Version` (15.0.0), while the RFC 3454 tables stay frozen at Unicode 3.2
as that RFC requires. RFC 3454 §7's assigned/unassigned split exists precisely so
a stringprep profile need not follow new Unicode releases.

The interop harness shells out to `podman` rather than using a container SDK.

## Connection model

One goroutine reads the connection and demultiplexes; commands are pipelined.
IMAP interleaves tagged completions with unsolicited untagged data, so a
request/response lock-step model is wrong and cannot be fixed later without an
API break.

- **Reader goroutine** owns the decoder. It routes tagged responses to the
  pending-command map and untagged responses to either the in-flight command's
  collector or the connection-level `UnilateralDataHandler`.
- **Command handles** may be returned after synchronously writing a bounded
  command prelude; initiation never waits for the server response. `Wait(ctx)`
  blocks for completion. This makes pipelining expressible without a second API.
- **State machine**: not-authenticated → authenticated → selected → logout, with
  the state guarded so invalid commands fail locally rather than on the wire.
- **Continuation requests** (`+`) are handled by the writer for literals,
  `AUTHENTICATE` and `IDLE`. Literal handling must support `LITERAL+`/`LITERAL-`
  (RFC 7888) non-synchronising literals when advertised.

Cancellation: IMAP has no general command-abort. Cancelling an in-flight command
therefore poisons the connection rather than desynchronising the stream; the
client closes it and reports `context.Canceled`. `IDLE` cancels cleanly with
`DONE` only after the server continuation accepts it. Before that continuation,
normal connection-poisoning cancellation applies; cancelling `WaitReady` alone
leaves the IDLE command active. This is documented centrally in
`API-STABILITY.md` §2 and on `IdleCommand`.

## Parser

Hand-written recursive-descent over a byte-level lexer. Not a generated parser:
the IMAP grammar has context-sensitive productions (literals carry a byte count
that must be consumed exactly, `astring` vs `string` differs by position), and
error messages from generated parsers are poor.

Requirements:
- Streaming. `FETCH BODY[]` of a 200 MiB message must not buffer in memory; body
  sections are exposed as `io.Reader` and the client enforces that a section is
  drained before the next response is parsed.
- Total. Any byte sequence a hostile server can send returns an error without a
  panic; the public client boundary surfaces protocol failures as `*imap.Error`.
  Enforced by fuzzing (task T13).
- Zero-copy where cheap, but correctness first.

## Charset and encoding

- Header decoding (RFC 2047) and parameter continuations (RFC 2231) in the
  envelope/body-structure layer.
- `UTF8=ACCEPT` (RFC 9755) changes literal encoding rules on the wire; handled in
  the codec, not by callers.
- Non-UTF-8 charsets in SEARCH are advertised via `SEARCH CHARSET`; the client
  reports what the server supports rather than transcoding silently.

## Milestones

| Milestone | Content | Exit criterion |
|---|---|---|
| M0 | wire codec, core types, number sets | fuzz targets green, no network/session layer |
| M1 | connection, auth, mailbox + message commands, interop harness | Dovecot interop green |
| M2 | IDLE, ENABLE, capability negotiation, extension groups A+B | M2 acceptance matrix green |
| M3 | extension groups C+D+E — full IANA coverage | coverage doc has no gaps |
| M4 | fuzzing, API review, docs, examples | apidiff gate active |
| **v1.0** | **API freeze** | |
| M5 | server framework | separate design doc first |
