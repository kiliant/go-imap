# Interoperability testing

Correctness against the RFC is necessary but not sufficient — real servers
disagree, and the disagreements are where client bugs live. Every feature is
verified against at least two independent implementations before its coverage
status becomes `verified`.

## Runtime

`podman` (the dev host has no Docker). The harness shells out to the CLI rather
than using a container SDK, to keep the dependency count at zero.

```bash
podman machine start                       # once
go test -race -tags=interop ./interop/...
```

## Server matrix

Probed on darwin/arm64, 2026-07-31.

| Server | Image | Arch | Tier | Why it is in the matrix |
|---|---|---|---|---|
| Dovecot | `docker.io/dovecot/dovecot` | arm64 native | 1 | The most deployed IMAP server; the de-facto conformance reference |
| Stalwart | `docker.io/stalwartlabs/stalwart` | arm64 native | 1 | Modern, aggressive RFC coverage incl. IMAP4rev2, OBJECTID, PARTIAL |
| GreenMail | `docker.io/greenmail/standalone` | arm64 native | 1 | Deliberately minimal — catches assumptions about optional capabilities |
| Cyrus IMAP | built from Debian packages, `servers/cyrus/Containerfile` | arm64 native | 2 | Large independent codebase; the ANNOTATE/METADATA and ACL reference |
| Courier | built from Debian packages, `servers/courier/Containerfile` | arm64 native | 2 | Older, quirky, rev1-only — the compatibility canary |
| Apache James | `docker.io/apache/james:demo-3.8.2` | **amd64 only** | 3 | JVM implementation, different bug class |

**Tier 1** runs by default. **Tier 2** is built locally on first run (slower, so
cached). **Tier 3** requires emulation and is opt-in:

```bash
go test -tags='interop interop_emulated' ./interop/...
```

There is no maintained Cyrus container image — the ones on Docker Hub are
unmaintained third-party builds. Building from Debian packages is both
arm64-native and reproducible, which is worth the extra build step.

## Capability profiles and skipping

Each server declares a profile in `interop/servers/<name>/profile.go`: the
capabilities it is *expected* to advertise. The harness then enforces two
different things, and the distinction matters:

- A test needing a capability the server does not advertise → **skip**.
  This is normal. GreenMail has no CONDSTORE and never will.
- A server not advertising a capability its profile *claims* → **fail**.
  This catches a broken container or a server downgrade, which would otherwise
  silently turn the whole suite into skips and look green.

A permanently red matrix is a matrix nobody reads, so the default is to skip. A
silently-all-skipping matrix is worse, so profiles are asserted.

## Adding a server

1. `interop/servers/<name>/Containerfile` (or an image reference) plus config
   that provisions a known account: `interop@example.test` / `interop-pw`.
2. `profile.go` with the expected capability list.
3. Register in `interop/harness/registry.go`.
4. Confirm the arch: `podman manifest inspect <image> | grep architecture`.
   If amd64-only, mark it Tier 3.

## Fixtures

Every server starts from the same seeded mailbox state so assertions are shared:
`INBOX` with 10 messages of known structure (plain, multipart/alternative,
multipart/mixed with attachment, 8-bit headers needing RFC 2047, a 5 MB message
for streaming, one `\Seen`, one `\Flagged`), plus `Archive`, `Sent`, and a
mailbox with a non-ASCII name to exercise modified-UTF-7 and `UTF8=ACCEPT`.

Fixtures live in `interop/harness/fixtures.go` and are installed over IMAP
`APPEND` after the server starts, not baked into images — otherwise each new
server needs its own mailbox-format tooling.
