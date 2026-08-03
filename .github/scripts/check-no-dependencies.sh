#!/usr/bin/env bash
#
# Supply-chain gate: the module must stay dependency-free.
#
# This assertion *is* the zero-dependency policy. Without it the policy erodes
# on the first convenient import, and every erosion is a `go.sum` entry — a
# stability liability this project does not control. See CLAUDE.md, "Zero
# external dependencies": the rule covers test-only dependencies too, which is
# why the check looks at the whole module graph and not just the build list.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

status=0

# 1. go.sum absent, or present but empty.
if [ -e go.sum ] && [ -s go.sum ]; then
  echo "FAIL: go.sum is non-empty; the module must have zero external dependencies" >&2
  cat go.sum >&2
  status=1
else
  echo "ok: go.sum absent or empty"
fi

# 2. No require directives. go.sum could lag a go.mod edit, so check both.
if grep -qE '^\s*require' go.mod; then
  echo "FAIL: go.mod contains a require directive" >&2
  grep -nE '^\s*require' -A5 go.mod >&2
  status=1
else
  echo "ok: go.mod has no require directives"
fi

# 3. The resolved module graph is this module and nothing else. This catches a
#    dependency arriving through a path the two checks above would miss.
mods="$(go list -m all)"
if [ "$mods" != "github.com/kiliant/go-imap" ]; then
  echo "FAIL: module graph is not just this module:" >&2
  echo "$mods" >&2
  status=1
else
  echo "ok: module graph is github.com/kiliant/go-imap alone"
fi

# 4. Nothing imports a package outside the standard library and this module.
#    Build tags matter here: an interop-only test dependency is still a
#    dependency, and a plain `go list ./...` never sees those files.
#
#    `.Standard` is the authoritative test, not a regexp on the import path:
#    the standard library vendors x/net and x/crypto under `vendor/`, and go1.26
#    added `crypto/internal/entropy/v1.0.0`, both of which a "contains a dot"
#    heuristic reports as external.
for tags in "" "interop" "interop interop_emulated"; do
  args=(-deps -test -f '{{if not .Standard}}{{.ImportPath}}{{end}}')
  [ -n "$tags" ] && args+=(-tags "$tags")
  external="$(go list "${args[@]}" ./... 2>/dev/null \
    | grep -v '^$' \
    | grep -v '^github\.com/kiliant/go-imap' || true)"
  if [ -n "$external" ]; then
    echo "FAIL: non-stdlib imports with tags '${tags:-<none>}':" >&2
    echo "$external" >&2
    status=1
  else
    echo "ok: stdlib-only imports with tags '${tags:-<none>}'"
  fi
done

exit "$status"
