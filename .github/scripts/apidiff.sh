#!/usr/bin/env bash
#
# Compare the exported API of the working tree against the previous git tag.
#
# Why this exists at all: the reference implementation in this ecosystem stayed
# in beta for years because every new extension forced a breaking change to the
# public API. This gate makes each such break visible on the pull request that
# introduces it, while it is still cheap to reconsider. Its value comes from
# having been running long enough to be trusted, so it is wired up before v1.0
# rather than at the tag.
#
# Policy (docs/tasks/T15-release-engineering.md):
#   pre-v1.0   report the diff; do not fail the build
#   post-v1.0  an incompatible change fails the build; overriding it takes an
#              explicit human decision recorded in the pull request
#
# The switch is the previous tag's major version, so the flip happens by tagging
# v1.0.0 and needs no workflow edit. APIDIFF_ENFORCE=1/0 overrides it for a
# deliberate one-off.
#
# Zero-dependency note: apidiff is a CI tool, never a module requirement.
# `go install pkg@version` runs in module-aware mode ignoring any go.mod in the
# working directory, and this script installs from a scratch directory as well
# as verifying afterwards that go.mod and go.sum are untouched. A go.sum that
# appears here would silently repeal the policy in check-no-dependencies.sh.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

APIDIFF_VERSION="${APIDIFF_VERSION:-latest}"
# Under .state/ (gitignored) by default so a local run leaves nothing untracked;
# CI overrides it to a runner-temp path.
OUT="${APIDIFF_OUT:-$ROOT/.state/ci-apidiff}"
mkdir -p "$OUT"
REPORT="$OUT/report.md"
: > "$REPORT"

previous_tag="$(git describe --tags --abbrev=0 2>/dev/null || true)"
if [ -z "$previous_tag" ]; then
  {
    echo "### apidiff"
    echo
    echo "No previous tag in this repository, so there is no baseline to compare"
    echo "against. This is the expected state before the first release tag; the"
    echo "gate becomes meaningful with the first \`v0.x\` tag and enforcing with"
    echo "\`v1.0.0\`."
  } > "$REPORT"
  cat "$REPORT"
  [ -n "${GITHUB_STEP_SUMMARY:-}" ] && cat "$REPORT" >> "$GITHUB_STEP_SUMMARY"
  exit 0
fi

# Enforcing mode is decided by the baseline tag's major version.
if [ -n "${APIDIFF_ENFORCE:-}" ]; then
  enforce="$APIDIFF_ENFORCE"
elif [[ "$previous_tag" =~ ^v([0-9]+)\. ]] && [ "${BASH_REMATCH[1]}" -ge 1 ]; then
  enforce=1
else
  enforce=0
fi

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

echo "installing apidiff@${APIDIFF_VERSION}" >&2
( cd "$scratch" && GOFLAGS= go install "golang.org/x/exp/cmd/apidiff@${APIDIFF_VERSION}" ) || {
  echo "failed to install apidiff" >&2
  exit 1
}
APIDIFF="$(go env GOPATH)/bin/apidiff"
[ -x "$APIDIFF" ] || { echo "apidiff not found at $APIDIFF" >&2; exit 1; }

# The tool must not have touched the module files.
if ! git diff --quiet -- go.mod || [ -s go.sum ]; then
  echo "FAIL: installing apidiff modified go.mod or created a go.sum" >&2
  exit 1
fi

old_tree="$scratch/old"
git worktree add --detach "$old_tree" "$previous_tag" >/dev/null 2>&1 || {
  echo "failed to check out $previous_tag" >&2
  exit 1
}
cleanup() { git worktree remove --force "$old_tree" >/dev/null 2>&1; rm -rf "$scratch"; }
trap cleanup EXIT

# Exported packages only. `internal/` is unreachable by consumers by
# construction (API rule 6) and `interop/` is test scaffolding, so a change in
# either is not an API change; including them would bury the real diff.
list_packages() {
  ( cd "$1" && go list ./... 2>/dev/null \
      | grep -v '/internal/\|/internal$' \
      | grep -v '/interop' )
}

old_pkgs="$(list_packages "$old_tree")"
new_pkgs="$(list_packages "$ROOT")"
all_pkgs="$(printf '%s\n%s\n' "$old_pkgs" "$new_pkgs" | sort -u)"

incompatible=0
{
  echo "### apidiff vs \`${previous_tag}\`"
  echo
  if [ "$enforce" = "1" ]; then
    echo "Mode: **enforcing** — an incompatible change fails this build."
  else
    echo "Mode: **reporting** (pre-v1.0) — breaks are allowed but must be"
    echo "deliberate. Anything listed as incompatible below needs a line in"
    echo "\`CHANGELOG.md\` saying so."
  fi
  echo
} >> "$REPORT"

for pkg in $all_pkgs; do
  in_old=0; in_new=0
  printf '%s\n' "$old_pkgs" | grep -qx "$pkg" && in_old=1
  printf '%s\n' "$new_pkgs" | grep -qx "$pkg" && in_new=1

  if [ "$in_old" = "1" ] && [ "$in_new" = "0" ]; then
    echo "- \`$pkg\`: **package removed** (incompatible)" >> "$REPORT"
    incompatible=1
    continue
  fi
  if [ "$in_old" = "0" ]; then
    echo "- \`$pkg\`: new package (compatible)" >> "$REPORT"
    continue
  fi

  safe="$(echo "$pkg" | tr '/.' '__')"
  ( cd "$old_tree" && "$APIDIFF" -w "$OUT/${safe}.old" "$pkg" ) >/dev/null 2>"$OUT/${safe}.olderr" || {
    echo "- \`$pkg\`: could not read the baseline API (see log)" >> "$REPORT"
    cat "$OUT/${safe}.olderr" >&2
    continue
  }

  diff_text="$("$APIDIFF" "$OUT/${safe}.old" "$pkg" 2>&1)"
  if [ -z "$diff_text" ]; then
    echo "- \`$pkg\`: no exported API change" >> "$REPORT"
    continue
  fi

  # apidiff groups its output under "Incompatible changes:" and
  # "Compatible changes:" headings.
  if printf '%s' "$diff_text" | grep -q 'Incompatible changes:'; then
    incompatible=1
  fi
  {
    echo
    echo "<details><summary><code>$pkg</code></summary>"
    echo
    echo '```'
    printf '%s\n' "$diff_text"
    echo '```'
    echo
    echo "</details>"
  } >> "$REPORT"
done

if [ "$incompatible" = "1" ]; then
  {
    echo
    if [ "$enforce" = "1" ]; then
      echo "**Incompatible changes present.** Post-v1.0 this fails the build."
      echo "Overriding it requires an explicit human decision recorded in this"
      echo "pull request, plus a \`CHANGELOG.md\` entry naming the symbols."
    else
      echo "**Incompatible changes present.** Allowed before v1.0, but they must"
      echo "be deliberate: name the affected exported symbols in \`CHANGELOG.md\`"
      echo "and, if the change sets a precedent, in \`docs/API-STABILITY.md\`."
    fi
  } >> "$REPORT"
fi

cat "$REPORT"
[ -n "${GITHUB_STEP_SUMMARY:-}" ] && cat "$REPORT" >> "$GITHUB_STEP_SUMMARY"

if [ "$enforce" = "1" ] && [ "$incompatible" = "1" ]; then
  exit 1
fi
exit 0
