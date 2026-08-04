#!/usr/bin/env bash
#
# CI fuzz runner.
#
# Targets are DISCOVERED, never listed. A hand-maintained list is how go-imap's
# FuzzParseSeqSet went uncampaigned and how its extension groups C/D/E shipped
# with no targets at all: nothing failed, the list simply did not mention them.
#
# This file is kept byte-identical in go-imap and go-smtp. It is
# repository-agnostic by construction — nothing here names a module, package or
# target — and the duplication is the zero-dependency policy's own consequence,
# since sharing it would mean a module dependency between the two. Diff the two
# copies before editing either. Three CI-specific properties:
#
#   1. sharding, so a large campaign fits inside a job's time budget;
#   2. FUZZ_PARALLEL defaults to 1, because the deadline artifact below is
#      caused by oversubscription and CI runners have few cores;
#   3. the retry-solo policy for that artifact.
#
# Usage: fuzz.sh [fuzztime]
#   SHARD_INDEX / SHARD_TOTAL   0-based shard selection (default 0 / 1)
#   FUZZ_PARALLEL               concurrent targets (default 1)
#   FUZZ_OUT                    artifact directory (default ./fuzz-out)
#   FUZZ_TAGS                   extra build tags
#
# Exit status is 0 only when every discovered target in the shard produced a
# PASS result. A target that fails to build is a hard failure, not a silent
# omission: `go test -list` compiles the test binary.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT" || exit 1

FUZZTIME="${1:-60s}"
SHARD_INDEX="${SHARD_INDEX:-0}"
SHARD_TOTAL="${SHARD_TOTAL:-1}"
PARALLEL="${FUZZ_PARALLEL:-1}"
# Default under .state/, which is gitignored, so a local run cannot leave
# untracked noise that someone later commits by accident. CI overrides this to
# a runner-temp path: actions/upload-artifact skips hidden directories.
OUT="${FUZZ_OUT:-$ROOT/.state/ci-fuzz}"
TAGS="${FUZZ_TAGS:-}"

# `set -u` treats an empty array expansion as unbound on bash < 4.4, so the
# no-tags case carries a harmless `-count=1`-style placeholder instead of an
# empty array. `-mod=readonly` is a no-op for a dependency-free module and is
# accepted by every go subcommand used here.
tagargs=(-mod=readonly)
[ -n "$TAGS" ] && tagargs+=(-tags "$TAGS")

mkdir -p "$OUT/logs"
: > "$OUT/results.txt"
: > "$OUT/deferred.txt"

# --- discovery -------------------------------------------------------------
all=()
for pkg in $(go list "${tagargs[@]}" ./... 2>/dev/null); do
  names=$(go test "${tagargs[@]}" -list '^Fuzz' "$pkg" 2>/dev/null | grep '^Fuzz' || true)
  for name in $names; do
    all+=("${pkg}:${name}")
  done
done

if [ "${#all[@]}" -eq 0 ]; then
  echo "no fuzz targets discovered" >&2
  exit 1
fi

# Sort so the shard assignment is deterministic across jobs and reruns.
# A read loop rather than `mapfile`, which needs bash 4 and macOS ships 3.2 —
# the dev host must be able to run this script too.
sorted=()
while IFS= read -r line; do
  sorted+=("$line")
done < <(printf '%s\n' "${all[@]}" | sort)
all=("${sorted[@]}")
printf '%s\n' "${all[@]}" > "$OUT/all-targets.txt"

targets=()
for i in "${!all[@]}"; do
  if [ $(( i % SHARD_TOTAL )) -eq "$SHARD_INDEX" ]; then
    targets+=("${all[$i]}")
  fi
done

if [ "${#targets[@]}" -eq 0 ]; then
  echo "shard $SHARD_INDEX/$SHARD_TOTAL selected no targets out of ${#all[@]}" >&2
  exit 1
fi

printf 'Discovered %d targets; shard %d/%d runs %d at %s each (parallel=%s)\n' \
  "${#all[@]}" "$SHARD_INDEX" "$SHARD_TOTAL" "${#targets[@]}" "$FUZZTIME" "$PARALLEL" \
  | tee "$OUT/campaign-start.txt"
printf '%s\n' "${targets[@]}" > "$OUT/targets.txt"

# --- the deadline artifact -------------------------------------------------
#
# Recorded and human-approved on 2026-08-03 (see .state/progress/T13.md): under
# FUZZ_PARALLEL contention a target can run its full fuzztime at full throughput
# and then exit `context deadline exceeded` at exactly the boundary, with no
# panic, no minimisation and no crasher written. A genuine finding cannot have
# that shape. The three occurrences all passed when rerun truly solo, at 3.4x to
# 4.2x the in-batch exec rate.
#
# A CI job that fails the branch on this is permanently red for reasons
# unrelated to the code — the failure mode the interop skip policy exists to
# avoid. So: classify, retry solo once, and only then call it red.
is_deadline_artifact() {
  local log="$1"
  grep -q 'context deadline exceeded' "$log" || return 1
  # Either of these means the engine found something; that is never an artifact.
  grep -q 'Failing input written to' "$log" && return 1
  grep -q '^panic:' "$log" && return 1
  grep -q 'minimizing' "$log" && return 1
  return 0
}

run_one() {
  local pkg="$1" name="$2" log="$3"
  # -run '^$' excludes the ordinary unit tests: they are covered by the test
  # job, and under fuzzing load the timing-sensitive ones fail for reasons that
  # have nothing to do with the target.
  go test "${tagargs[@]}" -count=1 -run '^$' "-fuzz=^${name}$" \
    -fuzztime="$FUZZTIME" "$pkg" >>"$log" 2>&1
}

pids=()
for entry in "${targets[@]}"; do
  pkg="${entry%%:*}"
  name="${entry##*:}"
  safe="$(echo "${pkg}_${name}" | tr '/:.' '___')"
  log="$OUT/logs/${safe}.log"
  (
    echo "BEGIN $name $pkg $(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$log"
    if run_one "$pkg" "$name" "$log"; then
      echo "PASS $name $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$OUT/results.txt"
    elif is_deadline_artifact "$log"; then
      # Deferred, not retried here: a rerun launched from inside the worker
      # would contend with the workers still running, which is the very
      # condition being ruled out. The main shell retries it after the shard
      # drains. (`wait` in this subshell would be a no-op — a subshell inherits
      # no job table.)
      echo "ARTIFACT ${pkg} ${name}" >> "$OUT/deferred.txt"
    else
      echo "FAIL $name $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$OUT/results.txt"
    fi
  ) &
  # Throttle on explicit PIDs. Do NOT use `$(jobs -r | wc -l)`: command
  # substitution runs in a subshell with an empty job table, so the count is
  # always 0 and the throttle silently does nothing. `wait -n` needs bash 4.3,
  # which macOS does not ship.
  pids+=("$!")
  if [ "${#pids[@]}" -ge "$PARALLEL" ]; then
    wait "${pids[0]}" 2>/dev/null
    pids=("${pids[@]:1}")
  fi
done
wait

# --- solo retries ----------------------------------------------------------
# Now that nothing else is running, each deferred target gets one truly solo
# rerun. Passing here confirms the contention diagnosis; failing again is real.
if [ -s "$OUT/deferred.txt" ]; then
  while read -r _ pkg name; do
    safe="$(echo "${pkg}_${name}" | tr '/:.' '___')"
    log="$OUT/logs/${safe}.log"
    echo "RETRY-SOLO $name (deadline artifact, no crasher) $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$log"
    if run_one "$pkg" "$name" "$log"; then
      echo "PASS $name (retried solo) $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$OUT/results.txt"
    else
      echo "FAIL $name (failed again solo) $(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$OUT/results.txt"
    fi
  done < "$OUT/deferred.txt"
fi

fail=$(grep -c '^FAIL ' "$OUT/results.txt" 2>/dev/null); fail=${fail:-0}
passed=$(grep -c '^PASS ' "$OUT/results.txt" 2>/dev/null); passed=${passed:-0}
retried=$(grep -c 'retried solo' "$OUT/results.txt" 2>/dev/null); retried=${retried:-0}
printf 'DONE shard=%d/%d targets=%d pass=%s fail=%s retried-solo=%s %s\n' \
  "$SHARD_INDEX" "$SHARD_TOTAL" "${#targets[@]}" "$passed" "$fail" "$retried" \
  "$(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee "$OUT/campaign-end.txt"

# Every target in the shard must appear in the results, or this is not a
# campaign over the shard — it is a campaign over whatever happened to run.
missing=0
for entry in "${targets[@]}"; do
  name="${entry##*:}"
  grep -qE "^(PASS|FAIL) ${name} " "$OUT/results.txt" || { echo "MISSING RESULT $name"; missing=1; }
done
[ "$missing" -eq 0 ] || echo "campaign incomplete: some targets produced no result" >&2

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Fuzz shard ${SHARD_INDEX}/${SHARD_TOTAL} — ${FUZZTIME} per target"
    echo
    echo "\`\`\`"
    cat "$OUT/campaign-end.txt"
    grep -E '^(FAIL|PASS .*retried)' "$OUT/results.txt" || true
    echo "\`\`\`"
  } >> "$GITHUB_STEP_SUMMARY"
fi

[ "$fail" -eq 0 ] && [ "$missing" -eq 0 ]
