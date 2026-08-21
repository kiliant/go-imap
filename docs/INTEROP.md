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

## Testing our own server — milestone M6

Written as a runbook before `imapserver` existed, so the design in
`docs/SERVER-DESIGN.md` §6 was committed to something concrete rather than a
good intention. T24 has since implemented it; each section below records what
landed where.

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

`imapserver` + the in-memory backend is an entry with a `profile.go` declaring
expected capabilities, exactly like Dovecot and Stalwart.

It lives in `imapserver/interop/` rather than the `interop/servers/goimap/` this
section originally planned. The reason is a module cycle: a profile for our own
server has to import `imapserver`, and `interop/harness`'s registry imports
every profile, so putting it under `interop/servers/` would make the harness a
dependency of the thing it tests. The profile is passed to `harness.Run`
explicitly instead — see `imapserver/interop/main_test.go`.

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
written by people who have fielded every client bug there is.

This is the highest-value single external check available, and the one thing on
this list that can find a conformance bug our own code is blind to.

Landed in `imapserver/interop/imaptest_test.go`, behind `-tags=interop`, with
the image in `imapserver/interop/testdata/imaptest/`. Two invocations: the
scripted transcript corpus, which is the nearest thing IMAP has to an executable
conformance suite, and a bounded randomised stress run with concurrent clients,
which is where selection teardown and update delivery race.

**Dovecot is built from source in that image, and that is not gold-plating.**
`imaptest` compiles against Dovecot's *internal* headers, which carry no
stability promise across releases, so the distribution packages cannot be used
in either Debian release available:

| Base | Ships | Result |
|---|---|---|
| trixie | Dovecot 2.4 | `imaptest` main fails on changed `iostream-rawlog.h` signatures |
| bookworm | Dovecot 2.3.19.1 | patched `imap_parser_create` takes an argument no `imaptest` branch passes |

Building upstream Dovecot 2.3.21 and pointing `--with-dovecot` at its source
tree is what upstream documents, and it is the only combination that is a
matched pair by construction rather than by luck. **The Dovecot version and the
`imaptest` commit move together or neither moves.**

### 4. Real client software

`mbsync`/`isync`, scripted against our server. Real clients exercise long-tail
sequencing — UIDVALIDITY handling, resumed partial fetches, a second pass that
must recognise its own prior state — that no suite written alongside the server
thinks to exercise.

Landed in `imapserver/interop/mbsync_test.go`, with the image in
`imapserver/interop/testdata/mbsync/`. It runs a full sync and then a resync in
one container, so the second pass sees the first's Maildir and sync state. The
resync is the half that matters: a first pass only proves the server can be
read, while the second proves UIDVALIDITY, UIDNEXT and per-message UIDs were
reported consistently enough that a synchroniser recognises its own state
instead of re-downloading — the bug class that makes a server unusable with real
clients while every unit test still passes.

`offlineimap` is deliberately not a second entry. It would exercise the same
protocol surface as `mbsync` for a second Python runtime's worth of image build,
and the acceptance criterion asks for at least one real client completing a
sync/resync cycle. Add it if a bug is ever found that `mbsync` cannot express.

#### What imaptest found

It paid for itself on the first run that reached the server. All three are
things every test in this repository passed, because every test here was
written by the people who wrote the server.

**Fixed — `STORE` rejected the unparenthesised flag list.** RFC 3501 §9 and RFC
9051 §9 both define

```
store-att-flags = (["+" / "-"] "FLAGS" [".SILENT"]) SP
                  (flag-list / (flag *(SP flag)))
```

so `STORE 1 +FLAGS \Deleted` is as valid as `STORE 1 +FLAGS (\Deleted)`. Only
the parenthesised form was accepted, so imaptest — and any other client using
the bare form — could not set a flag at all. Nothing in this repository ever
generated the bare form, which is exactly why no test caught it. Fixed in
`cmd_store.go`, pinned by `cmd_store_test.go`.

**Fixed (T25) — `EXPUNGE` was delivered while a pipelined
`FETCH`/`STORE`/`SEARCH` was still in progress.** RFC 3501 §7.4.1 (RFC 9051 §7.5.1):

> An EXPUNGE response MUST NOT be sent when no command is in progress, nor
> while responding to a FETCH, STORE, or SEARCH command. This rule is necessary
> to prevent a loss of synchronization of message sequence numbers between
> client and server.

This is not virgin ground: `127e342` ("defer expunge updates past completion")
already made FETCH, STORE and SEARCH withhold expunges until after their tagged
response, and `imapserver/cmd_update_order_test.go` pins it. That fix is correct
for the case it tests — one command at a time.

Pipelining defeats it. Every command handler calls `drainUpdates` *after*
writing its tagged response, and "after the tagged OK of command *n*" is
simultaneously "while command *n+1* is in progress" when the client sent *n+1*
without waiting. Deferring past completion moves the expunge out of one
forbidden window and into the next one. A captured transcript of one session:

```
[5] S> 5.5 OK FETCH completed
[5] S> * 4 EXPUNGE          <- 5.6 FETCH is outstanding here
[5] S> * 3 EXPUNGE
[5] S> 5.6 OK FETCH completed
```

imaptest reports the consequence the RFC predicts — `Referenced message
expunged seq=4 uid=0` — and eventually asserts internally once its view has
desynchronised. The fix is to make the condition "no FETCH/STORE/SEARCH is in
flight" rather than "the command that just finished was one" — that is, withhold
expunges until the server is responding to a command that permits them, taking
the pipeline queue into account, instead of flushing after each tagged response.
`cmd_update_order_test.go` is the right place to extend, with a pipelined case
alongside its existing one-at-a-time cases. That is shared machinery owned by
the server-core tasks and is deliberately not being landed at the tail of T24.

**Resolved in T25.** The condition became the connection's own backlog rather
than the command that just finished: an unsolicited renumbering waits while a
command is queued, while input has been read but not yet parsed, and across the
pre-command drain of a sequence-sensitive command. Nothing is popped from the
update queue while deferred, so the framework's sequence view never runs ahead
of the client's — popping and withholding the responses would produce exactly
the desynchronisation being prevented. The complementary clause — no EXPUNGE
when no command is in progress — is modelled the same way, and removal commands
themselves apply the queued revision prefix through `drainUpdatesThrough`
before any sequence number is written, so an EXPUNGE or MOVE cannot map UIDs
through a snapshot that still trails a deferred older removal. Pinned by
`TestExpungeUpdateWaitsForPipelinedCommands`,
`TestExpungeUpdateWaitsForACommandToBeInProgress` and
`TestDeferredCommandUpdateKeepsItsAccounting`. The matching entries were
removed from `imaptest_test.go`'s triaged table rather than left behind, since a
triaged finding that outlives its bug is a permanent blindfold.
`TestImaptestStress` completes the full workload under that design.

**Fixed — a keyword created by `STORE` was not re-announced in `FLAGS`.** The
server reported `$Label1` in a `FETCH FLAGS` response although no untagged
`FLAGS` response had listed it for the mailbox. RFC 3501 §7.2.6 makes that
untagged response the mailbox's applicable flag set. Backends can now publish
the complete set through `UpdateMailboxFlags`; the framework orders it before
the STORE's first keyword-bearing FETCH response and delivers the same ordered
update to other selected sessions. The memory backend also keeps applicable
keywords in a persistent mailbox registry rather than withdrawing one when its
last current message reference disappears. A final wire-level guard announces
any still-unknown flag immediately before a FETCH FLAGS response; this covers a
solicited FETCH reading current backend state while an older EXPUNGE deliberately
keeps that connection's later mailbox update queued. The raw regressions pin
both creation/persistence and FETCH ordering. The matching imaptest triage entry
was removed, and repeated `TestImaptestStress` runs pass with no ignored server
findings.

The scripted corpus is a third, different case: it never ran at all. imaptest's
script runner aborts with `FIXME: Add support for sync literals` unless the
server advertises `LITERAL+`, and this server advertises `LITERAL-` (RFC 7888),
capping unsolicited non-synchronising literals at 4096 octets. That is a
limitation of the tool, not a finding about the server, and the test skips
loudly rather than silently — an earlier version reported PASS on that abort,
because a tool refusing to start is indistinguishable from a tool finding
nothing wrong unless you check.

#### Where these run

`.github/scripts/run-interop.sh` drives three suites sequentially, and
`./imapserver/interop/...` is the third: `imapclient`, then `interop`, then
`imapserver`. Sequential for the reason the other two already were — one test
process per package, each owning an independent container lifecycle — and last
because it is the slowest and because a failure there is our bug rather than a
container's, which reads better at the end of a log than buried mid-run.

That puts `imaptest`, `mbsync` and the `goimap` capability table on the same
nightly-and-push-to-main schedule as the client matrix, not on a developer's
memory. The native job's timeout went to 120 minutes to absorb the Dovecot
source build.

They are deliberately not a pull-request gate, for the reason the header of
`interop.yml` already gives about the rest of the matrix: starting images makes
the gate an order of magnitude slower than the review it gates.

#### Both of these invert the harness

Every profile in `interop/harness` is a *server*, usually in a container, dialled
from the test process. `imaptest` and `mbsync` are the other way round: the
client is in the container and the server is a value in this process. That does
not fit `definition.Profile`, so these tests do not go through the registry.

They keep its two standing rules. Absent tooling — no `podman`, no network for a
base image, a build that fails — **skips**, because a permanently red matrix is
a matrix nobody reads. A protocol failure once the client is actually running
**fails**, because unlike a third-party server container we control both halves.

The server is reached at `host.containers.internal`, so its listener binds every
interface rather than loopback; `startOn` in `profile.go` exists for exactly
that, and both shapes construct the same server the capability matrix measures
rather than a second one configured by hand.

### 5. Server-side fuzzing

The mirror of T13, and non-optional. The command parser faces hostile input from
*unauthenticated remote clients*, which is a larger exposure than the client's
hostile-server case. Bar unchanged: no panic, no hang, no unbounded allocation.
The corpus starts from real client traffic captured here and from `imaptest`,
not from invention.

The corpus rule is honoured literally: `imapserver/interop/capture_test.go` runs
the third-party clients through a recording proxy and writes their sessions into
`imapserver/testdata/fuzz/FuzzServeConnPreAuth/`. Capture is opt-in behind
`GOIMAP_CAPTURE_CORPUS`, because a test that rewrote checked-in corpus files on
every run would make the corpus a function of who last ran the interop suite.

That is not ceremony. A 45-second campaign over the enriched corpus found 84 new
interesting inputs, because captured traffic reaches command shapes nobody here
would have thought to write down — which is the entire argument against
hand-written seeds.

No separate runner was needed. `.github/scripts/fuzz.sh` **discovers** targets
rather than listing them — that is T13's standing policy, and the reason is that
a hand-maintained list is precisely how `FuzzParseSeqSet` went uncampaigned and
how extension groups C, D and E once shipped with no targets at all. Nothing
failed; the list simply did not mention them. So the server's targets joined the
nightly campaign the moment they existed, and `imapserver`'s whole-connection
targets (`FuzzServeConnPreAuth`, `FuzzServeConnAuthenticated`) cover command
decoding end to end rather than only the parser in isolation.
