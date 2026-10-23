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
# FUZZ_MEMORY_LIMIT_MB sets the cap per process (default 3072). A hosted runner
# has 16 GiB and go test -fuzz starts one worker per CPU, so the default leaves
# room for four workers, the coordinator and the build.
set -u

limit_mb="${FUZZ_MEMORY_LIMIT_MB:-3072}"
if [ "$(uname -s)" = Linux ]; then
  ulimit -d $((limit_mb * 1024)) || exit 1
fi
exec "$@"
