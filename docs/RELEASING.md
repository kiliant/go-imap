# Releasing

Two modules, two version lines, one repository.

| Module | Path | Line | Promise |
|---|---|---|---|
| root | `github.com/kiliant/go-imap` | `v1.x.y` | frozen; an incompatible change fails CI |
| server | `github.com/kiliant/go-imap/imapserver` | `imapserver/v0.a.b` | may break between minors, deliberately |

The split and its reasoning are `SERVER-DESIGN.md` §9, approved with the design.
The short version: one `go.mod` cannot carry two different compatibility
promises, and the root API froze at v1.0 while the server's has not.

## The ordering constraint

**The root module must be released before the server module can be, and this is
not a preference.**

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

Afterwards the two lines move independently. A server release that wants a newer
root module bumps its `require` deliberately — that bump is a commit, which is
the point.

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
  && go get github.com/kiliant/go-imap/imapserver@v0.1.0
```

Do this from outside the repository. Inside it, the workspace answers instead of
the proxy, and the workspace is the thing being bypassed.

## Tagging

```sh
git tag -a v1.1.0        -m 'go-imap v1.1.0'
git tag -a imapserver/v0.1.0 -m 'imapserver v0.1.0'
git push github v1.1.0 imapserver/v0.1.0
```

`imapserver/v0.1.0` is Go's own convention for a nested module's tag, not a
local invention: the module's directory prefix, then the version. `apidiff.sh`
matches baselines by that prefix for the same reason.

## What each tag means

- **Root `v1.x.y`.** Additive only. `apidiff` runs in enforcing mode against the
  previous `v*` tag and an incompatible change fails the build; overriding it
  takes an explicit human decision recorded on the pull request, plus a
  `CHANGELOG.md` entry naming the symbols. See `API-STABILITY.md`.
- **Server `imapserver/v0.a.b`.** Not covered by the root module's promise, and
  the package doc says so. `apidiff` runs in reporting mode against the previous
  `imapserver/v*` tag. A break is allowed and must be deliberate: name the
  affected exported symbols in `CHANGELOG.md`.

`API-STABILITY.md` §10 records the changes that are free only until
`imapserver` v1.0 — the `MoveSupport`/`CapabilitySupport` collapse and the
THREAD witness-token rename. They are cheap now and permanent afterwards, so
they belong in a v0.x release rather than after one.
