# go-imap

An IMAP client library for Go, designed to reach complete capability coverage
and a stable v1.0 without freezing extension-hostile APIs.

```
import "github.com/kiliant/go-imap/imapclient"
```

> **Status: early development.** The wire codec, core protocol vocabulary,
> connection/session layer, authentication, base mailbox and message commands,
> capability negotiation, ENABLE, IDLE, the interoperability harness, and the
> deterministic adversarial-hardening regressions are implemented. Extension
> coverage and long-run/release verification remain in progress. See
> `docs/ROADMAP.md`.

## Why another one

The existing Go IMAP libraries have spent years in beta. The cause is structural,
not effort: their public APIs model FETCH items, SEARCH criteria and STATUS items
as closed sets, so every newly implemented RFC forces a breaking change, and the
API can never be frozen.

This library treats that as the primary design constraint. The rule every API
decision is measured against is written down in `docs/API-STABILITY.md`:

> Can an extension nobody has written yet be added without a breaking change?

## Goals

- **Complete.** Every capability in the IANA IMAP Capabilities registry, tracked
  in `docs/RFC-COVERAGE.md`.
- **Stable.** v1.0 with a real compatibility promise, enforced in CI by `apidiff`.
- **Verified against real servers.** Dovecot, Stalwart, GreenMail, Cyrus, Courier
  and Apache James under podman — not just against a mock. See `docs/INTEROP.md`.
- **Zero dependencies.** Standard library only, test code included.
- **Safe against hostile servers.** Every parser is fuzzed; malformed input
  returns an error, never a panic.

## Non-goals

- A mail *server* framework — deferred to milestone M5, after v1.0 of the client.
  The core types are already split into a shared package so this can be added
  without an API break.
- SMTP, POP3, JMAP, MIME composition. Use dedicated libraries.

## Documentation

| Document | Contents |
|---|---|
| `docs/tasks/BOARD.md` | **Start here to implement** — task specs, dependency order, file ownership |
| `docs/ARCHITECTURE.md` | Layering, connection model, settled design decisions |
| `docs/API-STABILITY.md` | The rules that make v1.0 possible |
| `docs/RFC-COVERAGE.md` | Capability → RFC → status, from the IANA registry |
| `docs/INTEROP.md` | Server matrix and how to run it |
| `docs/ROADMAP.md` | Milestones and exit criteria |
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
