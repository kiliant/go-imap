#!/usr/bin/env bash
#
# Background resource log for fuzz campaigns.
#
# fuzz.sh writes each target's output to a file that is uploaded only after the
# campaign. If the runner dies first, the job log is all that remains (#192).
# This prints one line per interval to the job log, which GitHub streams as it
# is written: available memory, swap, free disk, the fuzz target currently
# running, the data-size cap fuzz-limit.sh set on it, and the processes using
# the most memory. The last lines before a shutdown then name the target and
# show whether memory or disk ran out.
#
# That holds for "The runner has received a shutdown signal" only. When the
# runner loses communication with GitHub instead, the job log is lost as well
# (#233); fuzz-long.yml says what to do then.
#
# Linux only (procfs, procps). Stop it by killing the process.
set -u

interval="${FUZZ_WATCHDOG_INTERVAL:-30}"
while :; do
  mem=$(awk '/^(MemAvailable|SwapFree):/ {printf "%s=%dMiB ", $1, $2 / 1024}' /proc/meminfo)
  disk=$(df -Pm / | awk 'NR == 2 {printf "disk-free=%dMiB", $4}')
  target=$(ps -eo args= | grep -o -- '-test\.fuzz=[^ ]*' | sort -u | tr '\n' ' ')
  # Read back from the running binary rather than recomputed, so the line shows
  # the cap that is in force, not the one the formula was meant to produce.
  cap=""
  pid=$(pgrep -f -- '-test\.fuzz=' | head -n 1)
  if [ -n "$pid" ]; then
    cap=$(awk '/^Max data size/ {
      printf " data-cap=%s", ($4 == "unlimited" ? "unlimited" : int($4 / 1048576) "MiB")
    }' "/proc/$pid/limits" 2>/dev/null)
  fi
  top=$(ps -eo rss=,comm= --sort=-rss | head -n 3 | awk '{printf "%s:%dMiB ", $2, $1 / 1024}')
  echo "watchdog $(date -u +%H:%M:%S) ${mem}${disk} target=[${target% }]${cap} top=[${top% }]"
  sleep "$interval"
done
