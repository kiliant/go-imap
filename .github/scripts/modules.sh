#!/usr/bin/env bash
#
# Print each module directory in this repository, one per line, relative to the
# repository root.
#
# Discovered from the workspace rather than listed, for the same reason fuzz.sh
# discovers its targets: a module nobody remembered to add to a list is a module
# nobody builds, vets or tests, and nothing goes red to say so.
#
# fuzz.sh carries its own copy of this logic on purpose — it is kept
# byte-identical with go-smtp's copy and must not depend on a script that only
# exists here. Everything else in .github/scripts should call this.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

if [ -f go.work ]; then
  dirs="$(go work edit -json | sed -n 's/^[[:space:]]*"DiskPath": "\(.*\)".*/\1/p')"
  if [ -n "$dirs" ]; then
    printf '%s\n' "$dirs"
    exit 0
  fi
fi

echo "."
