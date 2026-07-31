# T15 — Release engineering

**Agent:** `docs-release` · **Milestone:** M4 · **Depends on:** T13, T14

**Owns:** `.github/**`, `CHANGELOG.md`

## CI (GitHub Actions)

| Job | Runs |
|---|---|
| `test` | `go test -race ./...` on the two most recent Go majors, linux + macOS |
| `vet` | `go vet`, `staticcheck`, `gofmt -l` |
| `interop` | Run `go test -count=1 -race -tags=interop ./imapclient`, then separately `go test -count=1 -race -tags=interop ./interop/...` — native profiles, on push to main and nightly; separate lifecycles prevent container-name collisions |
| `interop-emulated` | Run `go test -count=1 -race -tags='interop interop_emulated' ./imapclient`, then separately `go test -count=1 -race -tags='interop interop_emulated' ./interop/...` — Apache James (amd64), nightly only |
| `fuzz-smoke` | 60 s per fuzz target on every PR |
| `fuzz-long` | 30 min per target, nightly |
| `apidiff` | Compare exported API against the previous tag |

Note the interop job needs a container runtime; GitHub runners have Docker, the
dev host has podman. The harness must work with either — abstract the binary name
rather than hardcoding `podman`, and record which was used in the test output.

## The apidiff gate

`golang.org/x/exp/cmd/apidiff` against the previous tag on every PR.

- **Pre-v1.0:** post the diff as a PR comment. Breaks are allowed, but must be
  deliberate rather than discovered later.
- **Post-v1.0:** an incompatible change fails the build. Overriding it requires an
  explicit human decision recorded in the PR.

Wire this up *before* v1.0, not at the tag. Its value comes from having run long
enough to be trusted.

## CHANGELOG.md

Keep a Changelog format. Every entry touching an exported symbol says so
explicitly. This is what makes the apidiff output reviewable rather than noise.

## Release checklist

1. `go test ./...` and the full interop matrix green
2. `go vet`, `staticcheck`, `gofmt` clean
3. `apidiff` reviewed against the previous tag
4. `docs/RFC-COVERAGE.md` statuses match reality — spot-check three rows against
   the code rather than trusting the table
5. CHANGELOG updated; examples compile
6. Tag; from a clean temporary consumer module, run `go mod init`,
   `go get github.com/kiliant/go-imap@<tag>`, then compile and test a small
   import of the library
7. Verify the pkg.go.dev entry renders

## Supply chain

`go.mod` must stay dependency-free. A CI check asserts `go.sum` is absent or
empty — that assertion *is* the zero-dependency policy, and without it the policy
erodes on the first convenient import.

## Done when

All jobs green on main, the apidiff gate has run on at least one PR, and a
release-candidate tag has been cut and verified end to end.
