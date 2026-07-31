# T12 — Interop harness

**Agent:** `interop-harness` · **Milestone:** M1 · **Depends on:** T03 ·
**Status:** blocked

**Owns:** `interop/**`, plus write access to `internal/imapwire/testdata/**` for
captured golden responses

**Start this as soon as T03 lands**, in parallel with T04–T06. A matrix that
arrives after the code it was meant to validate has no value.

## Runtime facts — already verified, do not re-probe

Host is darwin/arm64 with **podman**; there is no `docker` binary. Probed
2026-07-31:

- arm64-native: `dovecot/dovecot`, `stalwartlabs/stalwart`, `greenmail/standalone`
- amd64-only: `apache/james:demo-3.8.2` → Tier 3, emulated, opt-in
- No maintained Cyrus or Courier image exists. Build both from Debian packages in
  `interop/servers/<name>/Containerfile` — arm64-native and reproducible.

## Deliverables

1. `interop/harness/` — container lifecycle over the `podman` CLI (no SDK; zero
   dependencies applies to test code too), readiness polling, per-package
   start/stop in `TestMain`.
2. `interop/servers/<name>/` — `Containerfile` or pinned image reference, server
   config provisioning `interop@example.test` / `interop-pw`, and `profile.go`
   with the expected capability list.
3. `interop/harness/fixtures.go` — identical seeded state on every server,
   installed over IMAP `APPEND` after startup (not baked into images, or each new
   server needs its own mailbox-format tooling). Required set is in
   `docs/INTEROP.md` §Fixtures.
4. `interop/harness/skip.go` — the capability gate.

## The skip/assert distinction is the whole point

- Test needs a capability the server does not advertise → **skip**. Normal;
  GreenMail has no CONDSTORE and never will. A permanently red matrix is a matrix
  nobody reads.
- Server does not advertise a capability its own `profile.go` claims → **fail**.
  Catches a broken container or a silent downgrade that would otherwise turn the
  suite into all-skips and look green.

Both halves are required. Neither alone is sufficient.

## Non-negotiables

- **Pin image tags.** Never `:latest` in committed code.
- **No `sleep` for readiness.** Poll the IMAP greeting with a timeout.
- **Parallel-safe.** Unique mailbox namespace per test, or a fresh container. No
  order dependence.
- **Diagnosable.** On failure dump the server log and the client wire trace. An
  interop failure nobody can diagnose from CI output gets ignored, and then the
  suite is decoration.
- **Cheap.** Containers start once per package, not per test.
- Build tags: `interop` for Tiers 1–2, `interop_emulated` additionally for Tier 3.

## Done when

`go test -race -tags=interop ./interop/...` brings up all Tier-1 and Tier-2
servers, seeds fixtures, runs the T03 smoke test against each, and reports a
per-server capability table. Cold-start time under 3 minutes, warm under 30 s.
