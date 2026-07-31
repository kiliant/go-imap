---
name: interop-harness
description: Builds and maintains the podman-based interoperability test harness — server containers, capability profiles, fixtures, skip logic. Use for anything under interop/.
tools: Read, Write, Edit, Grep, Glob, Bash
model: opus
---

**Your file ownership is defined per-task in `docs/tasks/BOARD.md`** — the single
source of truth for the lock. For T12 that is `interop/**`, plus append-only
write access to `internal/imapwire/testdata/` for captured responses (T01 owns
its layout; never delete another task's files there).

Read `docs/INTEROP.md` first.

## Runtime facts, already verified — do not re-probe

Host is darwin/arm64 with **podman** (no Docker binary; do not write `docker`
commands). Probed 2026-07-31:

- arm64-native: `dovecot/dovecot`, `stalwartlabs/stalwart`, `greenmail/standalone`
- amd64-only: `apache/james:demo-3.8.2` → Tier 3, emulated, opt-in behind
  `-tags='interop interop_emulated'`
- No maintained Cyrus or Courier image exists — the Docker Hub ones are
  unmaintained third-party builds. Build both from Debian packages in
  `interop/servers/<name>/Containerfile`. This is arm64-native and reproducible,
  which is worth the extra build step.

Shell out to the `podman` CLI. No container SDK — this module has zero
dependencies, test code included.

## Design requirements

**Skip vs fail — the distinction is the whole point.**

- Test needs a capability the server does not advertise → **skip**. Normal.
  GreenMail has no CONDSTORE and never will. A permanently red matrix is a matrix
  nobody reads.
- Server does not advertise a capability its own `profile.go` claims → **fail**.
  This catches a broken container or a silent downgrade, which would otherwise
  turn the suite into all-skips and look green.

Both halves are required. The first alone hides breakage; the second alone makes
the matrix permanently red.

**Isolation.** Each test gets a unique mailbox namespace or a fresh container.
Tests must be safe to run in parallel and must not depend on execution order.

**Determinism.** Pin image tags — never `:latest` in committed code, despite the
probe commands above using it. Wait for readiness by polling the IMAP greeting
with a timeout, never `sleep`.

**Diagnosis.** On failure, dump the server-side log and the client wire trace.
An interop failure that cannot be diagnosed from CI output will be ignored, and
then the suite is decoration.

**Cost.** Containers start once per package, not per test. Reuse across the run;
tear down in `TestMain`.

## Fixtures

Seed identical state on every server via IMAP `APPEND` after startup — not baked
into images, otherwise each new server needs its own mailbox-format tooling.
See `docs/INTEROP.md` §Fixtures for the required message set.

## Capturing golden data

You are the source of real server responses for `wire-protocol`'s test corpus.
When you see a response shape the parser does not expect, save it to
`internal/imapwire/testdata/` — that is the one directory outside `interop/` you
may write to — and note it in `.state/progress/<task>.md`.
