#!/usr/bin/env bash
#
# Report which container engine is available to the interop harness.
#
# interop/harness/engine.go now discovers the engine itself (preferring
# podman, falling back to docker, translating the couple of CLI spellings
# that differ) via IMAP_INTEROP_ENGINE or exec.LookPath, so this script no
# longer needs to install a compatibility shim — it only surfaces which
# engine the harness will pick, for the job summary and test output.
set -euo pipefail

if command -v podman >/dev/null 2>&1; then
  engine="podman"
  version="$(podman --version 2>&1 || true)"
elif command -v docker >/dev/null 2>&1; then
  engine="docker"
  version="$(docker --version 2>&1 || true)"
else
  echo "no container engine found: install podman or docker" >&2
  exit 1
fi

echo "interop container engine: ${engine} — ${version}"
if [ -n "${GITHUB_ENV:-}" ]; then
  echo "GO_IMAP_CONTAINER_ENGINE=${engine}" >> "$GITHUB_ENV"
fi
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  echo "Container engine: \`${engine}\` — ${version}" >> "$GITHUB_STEP_SUMMARY"
fi
