# go-imap

An IMAP client library for Go, built so that complete capability coverage and a
stable v1.0 are compatible goals rather than competing ones.

```
import "github.com/kiliant/go-imap/imapclient"
```

> **Status: root v1.1.0 and imapserver v0.1.0 released.** The exported API of
> `package imap` and
> `package imapclient` is frozen under the compatibility policy in
> `docs/API-STABILITY.md`; the separately versioned server framework remains
> deliberately v0.x. Both 2026-08-21 release tags are SSH-signed, and the server
> release passed standalone-module, dependency-graph, full CI and clean-consumer
> import gates. See `docs/RELEASING.md` and `docs/ROADMAP.md`.

## The design constraint

IMAP is not a finished protocol. FETCH items, SEARCH criteria, STATUS items and
capability names all grow with nearly every new RFC, so an API that models them
as closed sets has to break its callers to implement the next one. That is what
makes an IMAP client hard to freeze — not the volume of features.

So this library measures every public API decision against one question, written
down in `docs/API-STABILITY.md`:

> Can an extension nobody has written yet be added without a breaking change?

In practice that means open-ended sets get open-ended types. FETCH items are a
sum type over concrete items, not a struct of booleans. SEARCH criteria are an
expression tree that mirrors the grammar. STATUS items and response codes are
open string-backed types. A capability this library has never heard of can still
be requested, and data it cannot model is preserved rather than dropped:

```go
// Works today, before the library models the item.
imap.FetchItemKeyword("FUTURE-ITEM")   // request it
// ... comes back as *imap.FetchDataRaw, keyed by its wire name, not discarded.
```

When that RFC is implemented later, it adds a constant or a concrete type.
Nothing that already compiles stops compiling. `context.Context` is the first
parameter of every blocking call, options travel in structs, and protocol
failures all surface as one `*imap.Error` carrying the response code — for the
same reason.

## Goals

- **Complete.** Every capability in the IANA IMAP Capabilities registry, tracked
  in `docs/RFC-COVERAGE.md`.
- **Stable.** v1.0 with a real compatibility promise, enforced in CI by `apidiff`.
- **Verified against real servers.** Dovecot, Stalwart, GreenMail, Cyrus, Courier
  and Apache James under podman — not just against a mock. See `docs/INTEROP.md`.
- **Zero dependencies.** Standard library only, test code included. SASL,
  SASLprep, Unicode normalisation, DEFLATE and charset decoding are all reachable
  from the standard library, and a `go.sum` entry is a stability liability this
  project does not control.
- **Safe against hostile servers.** Every parser is fuzzed; malformed input
  returns an error, never a panic.

## Non-goals

- SMTP, POP3, JMAP, MIME composition. Use dedicated libraries.
- Mail *storage* — maildir, SQL, delivery, a user database. The server framework
  defines a backend interface and ships an in-memory reference backend for
  tests, examples and ephemeral servers, not production storage.

## Server framework

A server framework shipped as `imapserver/v0.1.0`. The core types were
split into a shared I/O-free package from the first commit precisely so this can
be added without an API break, and that has held up.

Its design was scoped before the tag for one reason: adding types
to the shared package after v1.0 is additive and always allowed, but *reshaping*
one is not — and a vocabulary that has only ever been exercised in the client
direction can contain a type a server can consume but cannot naturally produce.
No client-side review finds that. The design therefore ran before the freeze,
and a bidirectional review of the shared vocabulary became a v1.0 exit
criterion. That review completed before the tag.

See `docs/SERVER-DESIGN.md` — **approved on 2026-08-05**. The shared
server-direction codec, message-analysis foundation, server core, supported
memory backend, reusable backend conformance suite, base command set, extension
set and conformance harness (T18–T25) are complete. One `imaptest` conformance
finding remains recorded: a keyword first created by `STORE` is not re-announced
in an untagged `FLAGS` response. See `docs/INTEROP.md`.

## Documentation

| Document | Contents |
|---|---|
| `docs/tasks/BOARD.md` | **Start here to implement** — task specs, dependency order, file ownership |
| `docs/ARCHITECTURE.md` | Layering, connection model, settled design decisions |
| `docs/API-STABILITY.md` | The rules that make v1.0 possible |
| `docs/RFC-COVERAGE.md` | Capability → RFC → status, from the IANA registry |
| `docs/INTEROP.md` | Server matrix and how to run it |
| `docs/ROADMAP.md` | Milestones and exit criteria |
| `docs/SERVER-DESIGN.md` | Approved server framework design |
| `CLAUDE.md` | Working rules for AI agents contributing here |

## Testing

```bash
go test ./...                                  # unit, no network
go test -count=1 -race -tags=interop ./imapclient       # production client, needs podman
go test -count=1 -race -tags=interop ./interop/...      # harness packages, run after imapclient
go test -fuzz='^FuzzDecoder$' ./internal/imapwire        # parser robustness
```

## License

MIT
