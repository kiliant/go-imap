#!/usr/bin/env bash
#
# Run the interop suites sequentially and summarise their skips.
#
# The skip count is the point, not decoration. Interop tests skip on absent
# server capabilities rather than failing, which is what keeps the matrix
# readable — but it also means a suite can silently degrade to skipping
# everything and still report green. Surfacing the count and the lines makes
# that visible in the run summary instead of only in the raw log.
#
# Structurally identical to the sibling go-smtp repository's copy; only the
# suite list differs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

tags=${1:-interop}
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/go-imap-interop.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT
status=0

# These processes stay sequential because each owns an independent harness
# lifecycle: Go runs one test process per package, and combining the package
# lists starts several copies of every server image on one runner.
#
# imapserver is the inverse of the other two: rather than our client against
# real servers, it is our server measured as a matrix entry and driven by real
# third-party clients (imaptest, mbsync) running in containers. It is last
# because it is the slowest — the imaptest image builds Dovecot from source, for
# the reasons in docs/INTEROP.md — and because a failure there is our bug, which
# reads better at the end of a log than buried in the middle.
suites=(imapclient interop imapserver)
suite_packages_imapclient=./imapclient
suite_packages_interop=./interop/...
suite_packages_imapserver=./imapserver/interop/...

run_suite() {
  local name=$1
  shift
  echo "===== $name ====="
  set +e
  go test -v -count=1 -race -tags="$tags" "$@" 2>&1 | tee "$work_dir/$name.log"
  local command_status=${PIPESTATUS[0]}
  set -e
  printf '%d\n' "$command_status" >"$work_dir/$name.status"
  if (( command_status != 0 )); then status=1; fi
}

for name in "${suites[@]}"; do
  eval "packages=\$suite_packages_${name}"
  run_suite "$name" "$packages"
done

summary=${GITHUB_STEP_SUMMARY:-/dev/null}
{
  echo '### Interoperability matrix'
  echo
  echo "Build tags: \`$tags\`."
  echo
  echo '| Suite | Explicit skip lines | Result |'
  echo '|---|---:|---|'
  for name in "${suites[@]}"; do
    skips=$(grep -Ec -- '--- SKIP:|SKIP ' "$work_dir/$name.log" || true)
    command_status=$(<"$work_dir/$name.status")
    if (( command_status == 0 )); then result=PASS; else result=FAIL; fi
    printf '| `%s` | %d | %s |\n' "$name" "$skips" "$result"
  done
  for name in "${suites[@]}"; do
    skips=$(grep -Ec -- '--- SKIP:|SKIP ' "$work_dir/$name.log" || true)
    if (( skips > 0 )); then
      echo
      echo "<details><summary>$name skips</summary>"
      echo
      echo '```text'
      grep -E -- '--- SKIP:|SKIP ' "$work_dir/$name.log" || true
      echo '```'
      echo '</details>'
    fi
  done
} >>"$summary"

exit "$status"
