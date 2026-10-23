# Releasing

Two modules, one version number, one repository.

| Module | Path | Line | Promise |
|---|---|---|---|
| root | `github.com/kiliant/go-imap` | `v1.x.y` | frozen; an incompatible change fails CI |
| server | `github.com/kiliant/go-imap/imapserver` | `imapserver/v1.x.y` | frozen; an incompatible change fails CI |

The split and its reasoning are `SERVER-DESIGN.md` §9, approved with the design.
The short version: one `go.mod` could not carry two different compatibility
promises while the root API was frozen at v1.0 and the server's was not. The
server line was v0.x until milestone M7 (`ROADMAP.md`) was met, and has been
v1 since `imapserver/v1.3.1`.

## One version number — decided 2026-10-23

**Decided by the human on 2026-10-23**, at the server's first v1 tag. The
reason is one version number for users to think about. This replaces the
earlier plan for `imapserver/v1.0.0` with independent version lines.

- **The first stable server tag is `imapserver/v1.3.1`.** It matches root
  `v1.3.1`, which it requires and which had no code change since. The root
  module was therefore not tagged again.
- **From then on, every release tags both modules with the same version,** root
  first. A module that did not change still gets the tag, and its CHANGELOG
  section says so. Those empty releases are the accepted cost.
- **Lockstep holds within major version 1 only.** If one module needs v2, the
  lockstep ends there. That module moves to v2 with its new import path, and
  the other continues its own v1 line. Neither module is forced into a major
  version, and so onto a new import path, by the other. Record the end of the
  lockstep in this file when it happens.
- **A root v2 does not carry the server along.** The server imports the root
  module's `internal/imapcodec`, `internal/imapmessage` and `internal/imapwire`,
  and Go's internal-package rule stops `github.com/kiliant/go-imap/imapserver`
  from importing `github.com/kiliant/go-imap/v2/internal/...`. A server on v1
  therefore stays on root v1, and fixes to those packages need root v1.x
  maintenance releases. A server that wants root v2 needs a path under it, such
  as `github.com/kiliant/go-imap/v2/imapserver`.

## Graduating the server to v1

This is how the server reached v1, kept for the record.

Maildir supplies the second backend required by M7. It does not by itself
freeze the server API. Before opening the stability window, resolve or
explicitly defer the storage contract gaps and complete the full review in
`tasks/T33-imapserver-v1.md`, including the message helper, backendtest and
documented defaults. Land any breaking corrections while still on v0.x.
Then complete a second full review of the integrated candidate, covering
every exported symbol and documented behavioural contract in `imapserver`,
`backendtest`, `memory`, `message` and `maildir`, including unchanged API.
Record its commit, coverage, evidence and verdict on #54, and resolve blocking
findings before the stability window opens. The second pass is required by
the human's 2026-10-19 instruction; reviewing only the corrections does not
satisfy it.

Then publish two consecutive substantive server minor releases that preserve
the reviewed contract. Every release between them, including patches, must
have no incompatible API diff and no BREAKING behaviour entry. A minor that
only bumps the root dependency does not count; a break restarts the window.
Record the API-diff and changelog evidence on issue #54 for each release.
The existing pre-Maildir releases cannot count toward that window.

Immediately before the v1 tag, review every addition since the second full review
and compare against the second quiet release. Update the module promise in
API-STABILITY, this file, CLAUDE and ROADMAP only in the actual release PR.
The standalone build, clean-consumer and signed-tag gates below remain
required. No tag or release is created automatically by this plan. T33 (#54)
completed these steps with `imapserver/v1.3.1` on 2026-10-23.

## The ordering constraint

**The root module must be released before the server module can be, and this is
not a preference.**

Root `v1.3.0` carries the T33 `Client.Unauthenticate` COMPRESS correction and
the shared `internal/imapdeflate` boundary reader. The server's corrected
teardown requires that client behavior, and server production code imports the
helper. `imapserver/v0.4.0` therefore requires a root release no older than
`v1.3.0`; it pins `v1.3.1`, which also carries the shared message-analysis
corrections the server relies on. Each later server release pins the root
release containing what it imports and runs the standalone and clean-consumer
gates below.

`imapserver`'s production code imports `internal/imapcodec`, `internal/imapmessage`
and `internal/imapwire` from the root module. The first two landed after
`v1.0.0` was tagged, so they exist in no released version of the root module.
`imapserver/go.mod` must require a root version that actually contains them, and
§9 rules out shipping a `replace` directive to get around it — a `replace` in a
published module is not a workaround, it is a broken module.

So the first server release is a two-tag sequence and the order is fixed:

1. Land the server module's `go.mod` on `main` (done — it excludes `imapserver/`
   from the root module, which is what makes the two modules distinct).
2. Tag the **root** module `v1.1.0` from that commit. It is a minor, not a
   patch: the root package gained `SearchFilter`, the NOTIFY vocabulary and
   `Envelope.RawDate` since `v1.0.0`, all additive.
3. In `imapserver/`, run `go mod tidy` to write the `go.sum` pinning that
   version, and commit it.
4. Run the release gates below.
5. Tag the server module `imapserver/v0.1.0`.

Steps 2 and 5 are the human's; nothing in CI tags anything.

Afterwards the two lines moved independently until `imapserver/v1.3.1`, and
have been tagged together since. A server release that wants a newer root
module still bumps its `require` deliberately — that bump is a commit, which is
the point. With both modules tagged at every release, the server's `require`
can name the root tag of the same release, because the root is tagged first.

## Release gates

Everything in `ci.yml` runs per pull request and must be green. These are the
additional checks that only make sense at a release, because they need the tags
to exist.

### The server module builds standalone

Inside the workspace, `imapserver` builds against the root module's *working
tree*, not against the version its `go.mod` requires. That is what a workspace
is for, and it means the require line is unverified until you turn the workspace
off:

```sh
cd imapserver && GOWORK=off go build ./... && GOWORK=off go test ./...
```

Before the root tag exists this fails with a missing `go.sum` entry, which is
the expected state — it is step 3 above that fixes it, and this command is how
you confirm step 3 actually worked.

### The supply-chain gate's third check

`check-no-dependencies.sh` verifies the resolved module graph, which needs every
required module to be fetchable. Before the root module is published at the
required version it cannot run, and the script says so out loud rather than
reporting a pass it did not perform:

```
SKIP: module graph unresolvable because github.com/kiliant/go-imap is not published yet.
```

**After tagging the root module, run it again and confirm that line is gone.**
A release where it is still printed has not had its module graph checked.

### A consumer can actually import it

The gate that catches what the others cannot — that the tag is fetchable, the
`go.sum` is right, and the published module is self-contained:

```sh
cd "$(mktemp -d)" && go mod init example.com/check \
  && go get github.com/kiliant/go-imap/imapserver@v1.3.1
```

Do this from outside the repository. Inside it, the workspace answers instead of
the proxy, and the workspace is the thing being bypassed.

## Tagging

Both modules are tagged at every release, root first. For example:

```sh
git tag -s v1.4.0            -m 'go-imap v1.4.0'
git tag -s imapserver/v1.4.0 -m 'imapserver v1.4.0'
git tag -v v1.4.0 imapserver/v1.4.0
git push origin v1.4.0 imapserver/v1.4.0
```

Every release tag is signed (`-s`, with the SSH key `gpg.format = ssh`
configures) and verified before it is pushed. An unsigned `-a` tag looks the
same in `git tag` and in the module proxy, which is how `imapserver/v0.2.0`
first went out without a signature.

`imapserver/v0.1.0` is Go's own convention for a nested module's tag, not a
local invention: the module's directory prefix, then the version. `apidiff.sh`
matches baselines by that prefix for the same reason.

## What each tag means

- **Root `v1.x.y`.** Additive only. `apidiff` runs in enforcing mode against the
  previous `v*` tag and an incompatible change fails the build; overriding it
  takes an explicit human decision recorded on the pull request, plus a
  `CHANGELOG.md` entry naming the symbols. See `API-STABILITY.md`.
- **Server `imapserver/v1.x.y`.** The same promise as the root module, since
  `imapserver/v1.3.1`. `apidiff.sh` chooses enforcing mode from the baseline
  tag's major version, so an incompatible change against the previous
  `imapserver/v*` tag fails the build with no workflow edit.
- **Server `imapserver/v0.a.b`**, until `v0.6.0`. Not covered by the root
  module's promise. `apidiff` ran in reporting mode, and every break was named
  in `CHANGELOG.md`.

`API-STABILITY.md` §10 recorded changes that were free only until the server's
v1 tag. The `MoveSupport`/`CapabilitySupport` collapse and the context/options
upgrade landed after v0.1.0, and none was left open at `imapserver/v1.3.1`.
