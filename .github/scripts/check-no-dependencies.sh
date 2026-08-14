#!/usr/bin/env bash
#
# Supply-chain gate: every module here stays dependency-free.
#
# This assertion *is* the zero-dependency policy. Without it the policy erodes
# on the first convenient import, and every erosion is a `go.sum` entry — a
# stability liability this project does not control. See CLAUDE.md, "Zero
# external dependencies": the rule covers test-only dependencies too, which is
# why the check looks at the whole module graph and not just the build list.
#
# The repository holds more than one module since T25 (SERVER-DESIGN.md §9), so
# the gate runs per module and modules are discovered from the workspace rather
# than listed here — a module nobody added to a list is a module nobody checks.
#
# THE ONE SANCTIONED EXCEPTION: a nested module may depend on this repository's
# own root module, and only on that. §9 argues it is narrow and fully
# controlled — we publish both sides, so the failure modes that make a `go.sum`
# entry a liability (an upstream that disappears, breaks, or turns hostile) are
# ours to cause and ours to fix. It is not licence for any other dependency,
# which is why "self" is checked by exact module path below and not by prefix.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

status=0

# The root module path, read rather than written down, so a rename cannot leave
# this script quietly asserting something about a module that no longer exists.
# GOWORK=off: inside a workspace `go list -m` prints every module in it.
SELF="$(GOWORK=off go list -m 2>/dev/null || true)"
if [ -z "$SELF" ]; then
  echo "FAIL: could not determine the root module path" >&2
  exit 1
fi

module_dirs=(".")
if [ -f go.work ]; then
  module_dirs=()
  while IFS= read -r dir; do
    [ -n "$dir" ] && module_dirs+=("$dir")
  done < <(go work edit -json | sed -n 's/^[[:space:]]*"DiskPath": "\(.*\)".*/\1/p')
  [ "${#module_dirs[@]}" -eq 0 ] && module_dirs=(".")
fi

echo "modules: ${module_dirs[*]}"

for dir in "${module_dirs[@]}"; do
  echo
  echo "--- $dir"
  (
    cd "$ROOT/$dir"
    module="$(GOWORK=off go list -m)"
    # A nested module is one that is not the root module. Only it may carry the
    # self-dependency; the root module must stay at literally zero.
    if [ "$module" = "$SELF" ]; then
      allow_self=0
    else
      allow_self=1
    fi

    substatus=0

    # 1. go.sum holds nothing but the sanctioned self-dependency.
    if [ -e go.sum ] && [ -s go.sum ]; then
      if [ "$allow_self" -eq 1 ]; then
        foreign="$(grep -v "^${SELF} " go.sum || true)"
        if [ -n "$foreign" ]; then
          echo "FAIL: go.sum has entries other than $SELF" >&2
          echo "$foreign" >&2
          substatus=1
        else
          echo "ok: go.sum holds only the sanctioned $SELF entry"
        fi
      else
        echo "FAIL: go.sum is non-empty; the root module must have zero dependencies" >&2
        cat go.sum >&2
        substatus=1
      fi
    else
      echo "ok: go.sum absent or empty"
    fi

    # 2. Require directives: none at the root, only the self-dependency nested.
    #    go.sum could lag a go.mod edit, so check both.
    requires="$(go mod edit -json | sed -n 's/^[[:space:]]*"Path": "\(.*\)",$/\1/p' | grep -v "^${module}$" || true)"
    if [ -n "$requires" ]; then
      if [ "$allow_self" -eq 1 ] && [ "$requires" = "$SELF" ]; then
        echo "ok: requires only $SELF"
      else
        echo "FAIL: unexpected require directives:" >&2
        echo "$requires" >&2
        substatus=1
      fi
    else
      echo "ok: no require directives"
    fi

    # 3. The resolved module graph is this module, plus at most the root module.
    #    This catches a dependency arriving through a path 1 and 2 would miss.
    #
    #    GOWORK=off on purpose: inside the workspace the graph is the workspace's
    #    and would hide a require this module actually carries.
    #
    #    Resolving it needs every required module to be fetchable, which for a
    #    nested module means $SELF must already be published at the version it
    #    requires. Before that first release it is not, so the command fails —
    #    and an earlier version of this script fell back to "clean" on any
    #    error, which made a real dependency invisible. It now says which of the
    #    two happened, out loud, and leaves the check to the release gate rather
    #    than reporting a pass it did not perform. Check 2 above still bounds the
    #    requires exactly, so nothing is unguarded in the meantime.
    if mods="$(GOWORK=off go list -m all 2>&1)"; then
      unexpected="$(printf '%s\n' "$mods" | grep -v "^${module}$" | grep -v "^${SELF} \|^${SELF}$" || true)"
      if [ -n "$unexpected" ]; then
        echo "FAIL: module graph has more than this module and $SELF:" >&2
        echo "$unexpected" >&2
        substatus=1
      else
        echo "ok: module graph is $module (plus at most $SELF)"
      fi
    elif [ "$allow_self" -eq 1 ] && printf '%s\n' "$mods" | grep -q "$SELF"; then
      echo "SKIP: module graph unresolvable because $SELF is not published yet." >&2
      echo "      This is check 3 only; see docs/RELEASING.md, which runs it as a" >&2
      echo "      release gate once the root module is tagged." >&2
    else
      echo "FAIL: module graph could not be resolved:" >&2
      echo "$mods" >&2
      substatus=1
    fi

    # 4. Nothing imports a package outside the standard library and this
    #    repository. Build tags matter here: an interop-only test dependency is
    #    still a dependency, and a plain `go list ./...` never sees those files.
    #
    #    `.Standard` is the authoritative test, not a regexp on the import path:
    #    the standard library vendors x/net and x/crypto under `vendor/`, and
    #    go1.26 added `crypto/internal/entropy/v1.0.0`, both of which a
    #    "contains a dot" heuristic reports as external.
    for tags in "" "interop" "interop interop_emulated"; do
      args=(-deps -test -f '{{if not .Standard}}{{.ImportPath}}{{end}}')
      [ -n "$tags" ] && args+=(-tags "$tags")
      external="$(go list "${args[@]}" ./... 2>/dev/null \
        | grep -v '^$' \
        | grep -v "^${SELF}" || true)"
      if [ -n "$external" ]; then
        echo "FAIL: non-stdlib imports with tags '${tags:-<none>}':" >&2
        echo "$external" >&2
        substatus=1
      else
        echo "ok: stdlib-only imports with tags '${tags:-<none>}'"
      fi
    done

    exit "$substatus"
  ) || status=1
done

exit "$status"
