#!/usr/bin/env bash
#
# Test-binary wrapper for fuzz campaigns, installed with
# GOFLAGS=-exec=<path to this file>.
#
# Why: a target that drives the code under test into unbounded memory use takes
# the whole hosted runner down. The job then ends with "The runner has received
# a shutdown signal" and nothing after the campaign step runs, so neither the
# logs nor the input that caused it survive (#192). Capping each test binary
# turns that into an ordinary fuzz failure: the Go runtime aborts with "out of
# memory", the engine records the failing input, and the upload step keeps it.
#
# The cap is RLIMIT_DATA, which Linux applies to private writable mappings such
# as the Go heap. Fuzz workers are re-executions of the test binary, so they
# inherit it. Address-space reservations and file mappings do not count, which
# keeps an ordinary target well inside the limit. fuzz.sh is deliberately not
# touched: it has a copy in go-smtp, kept in step by hand.
#
# The limit is per process, but one target runs several: the coordinator and
# one worker per CPU, times FUZZ_PARALLEL targets at once. A fixed 3 GiB per
# process added up to 15 GiB on a 4-CPU runner with about 14.5 GiB available,
# so a leak that grew every worker together could push the runner into swap
# until it stopped answering, which leaves no evidence at all (#233). The default
# therefore shares what is available, less a reserve for the runner agent and
# the go command, between every process that can be running:
#
#   (MemAvailable - 2048 MiB) / ((workers + 1) * FUZZ_PARALLEL)
#
# clamped to 1024..3072 MiB. workers is GOMAXPROCS when set, else nproc: the
# engine's default worker count. On a 4-CPU, 16 GiB hosted runner this comes to
# about 2.5 GiB per process. The floor keeps headroom over ordinary targets,
# which peak near 640 MiB resident; a machine whose share falls below it cannot
# be bounded this way.
# FUZZ_MEMORY_LIMIT_MB replaces the computed value.
set -u

if [ "$(uname -s)" = Linux ]; then
  limit_mb="${FUZZ_MEMORY_LIMIT_MB:-}"
  if [ -z "$limit_mb" ]; then
    reserve_mb=2048
    avail_mb=$(awk '/^MemAvailable:/ {print int($2 / 1024)}' /proc/meminfo)
    procs=$(( (${GOMAXPROCS:-$(nproc)} + 1) * ${FUZZ_PARALLEL:-1} ))
    limit_mb=$(( (${avail_mb:-0} - reserve_mb) / procs ))
    [ "$limit_mb" -gt 3072 ] && limit_mb=3072
    [ "$limit_mb" -lt 1024 ] && limit_mb=1024
  fi
  ulimit -d $((limit_mb * 1024)) || exit 1
fi
exec "$@"
