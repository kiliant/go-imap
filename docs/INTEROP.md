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
go test -count=1 -race -tags=interop ./imapclient
go test -count=1 -race -tags=interop ./interop/...
```

The first command names `./imapclient` explicitly so interop-tagged
production-client tests run. The commands must remain separate and sequential:
Go builds and runs one test process per package, and every process with a
`TestMain` using the harness owns an independent container lifecycle. Combining
the package lists starts those lifecycles concurrently, which means several
copies of every server image competing for the same host resources.

Three packages are harness-backed today: `imapclient` (interop-tagged),
`interop/smoke` and `interop/saslprep`. Keep future package invocations separate
too.

Container names embed the process ID as well as a timestamp and a per-process
counter. That is load-bearing rather than decorative: two packages starting the
same profile within the same wall-clock second previously generated identical
names and `podman run` failed outright, which is how the second harness-backed
package announced itself. Name collisions are therefore fixed, but the sequential
rule above still stands for the resource reason.

By default the harness starts every native profile. Restrict a local run to a
comma-separated subset when iterating, for example:

```bash
export GO_IMAP_INTEROP_SERVERS=dovecot,greenmail
go test -count=1 -race -tags=interop ./imapclient
go test -count=1 -race -tags=interop ./interop/...
```

## Server matrix

Probed on darwin/arm64, 2026-07-31.

| Server | Image | Arch | Tier | Why it is in the matrix |
|---|---|---|---|---|
| Dovecot | local build from `docker.io/dovecot/dovecot:2.4.3` | arm64 native | 2 | The most deployed IMAP server; the local layer enables the T04 SASL matrix |
| Stalwart | local build: `interop/servers/stalwart/Containerfile` | arm64 native | 1 | Modern, aggressive RFC coverage incl. IMAP4rev2, OBJECTID, PARTIAL |
| GreenMail | `docker.io/greenmail/standalone:2.1.9` | arm64 native | 1 | Deliberately minimal — catches assumptions about optional capabilities |
| Cyrus IMAP | local build: `interop/servers/cyrus/Containerfile` | arm64 native | 2 | Large independent codebase; the ANNOTATE/METADATA and ACL reference |
| Courier | local build: `interop/servers/courier/Containerfile` | arm64 native | 2 | Older, quirky, rev1-only — the compatibility canary |
| Apache James | `docker.io/apache/james:demo-3.8.2` | **amd64 only** | 3 | JVM implementation, different bug class |

Tiers 1 and 2 run by default; local build contexts are cached by the container
runtime. Tier 3 (Apache James) requires emulation and is opt-in:

```bash
go test -count=1 -race -tags='interop interop_emulated' ./imapclient
go test -count=1 -race -tags='interop interop_emulated' ./interop/...
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

1. `interop/servers/<name>/Containerfile` (or a pinned image reference) plus config
   that provisions a known account: `interop@example.test` / `interop-pw`.
2. `profile.go` with the expected capability list.
3. Register in `interop/harness/registry.go`.
4. Confirm the arch: `podman manifest inspect <image> | grep architecture`.
   If amd64-only, mark it Tier 3.

Optionally provision the two SASLprep diagnostic accounts, which store the same
password in the two forms that discriminate Unicode normalisation:

| Account | Stored password | Bytes |
|---|---|---|
| `interop-prep@example.test` | `interop-pw-µ` (U+00B5 MICRO SIGN) | `…c2 b5` |
| `interop-prep-nfkc@example.test` | `interop-pw-μ` (U+03BC GREEK SMALL MU) | `…ce bc` |

`interop/saslprep` skips cleanly on a server that lacks them, so they are not a
prerequisite for adding a server. Verify the bytes with `xxd` after editing —
these are the one fixture in the tree where an editor silently normalising a
source file would make the test assert nothing while still passing. The
NFKC-stored account exists to emulate a server that prepares credentials at
enrollment, which none of the matrix servers do.

If the server uses a Dovecot-style `passwd-file` passdb, leave `result_failure`
at its default (`continue`). An absent user and a wrong password are reported
identically, so `return-fail` would break fallthrough to the passdb holding
`interop@example.test`.

## Fixtures

Every server starts from the same seeded mailbox state so assertions are shared:
`INBOX` with 10 messages of known structure (plain, multipart/alternative,
multipart/mixed with attachment, 8-bit headers needing RFC 2047, a 5 MiB message
for streaming, one `\Seen`, one `\Flagged`), plus `Archive`, `Sent`, and a
mailbox with a non-ASCII name to exercise modified-UTF-7 and `UTF8=ACCEPT`.

Fixtures live in `interop/harness/fixtures.go` and are installed over IMAP
`APPEND` after the server starts, not baked into images — otherwise each new
server needs its own mailbox-format tooling.

## Testing our own server — planned, milestone M6

No `imapserver` code exists yet; this section is the runbook the server work
(T24) implements against, recorded now so the design in `docs/SERVER-DESIGN.md`
§6 is committed to something concrete rather than a good intention.

The client's rule — verified against at least two independent implementations
before a capability is `verified` — has an obvious problem in the other
direction: our client testing our server is one implementation talking to
itself. A shared misreading of an RFC passes both sides and neither notices. So
loopback is the inner loop, and the validation is external.

### 1. Loopback — fast, hermetic, not validation

Our client against our server over `net.Pipe`, in-process, no containers. Runs in
the default `go test ./...` because it needs no runtime. Catches regressions;
proves nothing about RFC conformance.

### 2. The matrix, pointed at ourselves — highest value per unit of work

`imapserver` + the in-memory backend becomes an entry in
`interop/servers/goimap/`, exactly like Dovecot and Stalwart: a `profile.go`
declaring expected capabilities, registered in `interop/harness/registry.go`.

Everything in this document then applies unchanged — same fixtures installed over
`APPEND`, same skip/assert distinction, same per-capability table. The result is
our server's coverage reported in the same units as Dovecot's, which is the
comparison that actually means something.

It differs from every other entry in one way worth noting: the profile assertion
("a server not advertising a capability its profile claims → fail") becomes a
real regression test rather than a container-health check, because we control
both halves. That is a feature — it is the one entry where the assertion can
catch our own bug.

Being in-process rather than containerised, it needs no image and no `podman`;
the harness must not assume every profile has a container.

### 3. `imaptest` — the external check that matters

Dovecot's `imaptest` is the de-facto IMAP server conformance and stress tool,
written by people who have fielded every client bug there is. Run it against our
server in a container, as a Tier 2 entry.

This is the highest-value single external check available, and the one thing on
this list that can find a conformance bug our own code is blind to.

### 4. Real client software

`mbsync`/`isync` and `offlineimap`, scripted against our server. They exercise
long-tail sequencing — UIDVALIDITY changes mid-sync, partial fetches resumed,
CONDSTORE replay — that no suite written alongside the server thinks to
exercise.

### 5. Server-side fuzzing

The mirror of T13, and non-optional. The command parser faces hostile input from
*unauthenticated remote clients*, which is a larger exposure than the client's
hostile-server case. Bar unchanged: no panic, no hang, no unbounded allocation.
The corpus starts from real client traffic captured here and from `imaptest`,
not from invention.
