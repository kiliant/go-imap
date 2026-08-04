#!/usr/bin/env bash
#
# Post the apidiff report to a pull request, editing the previous report rather
# than appending a new one.
#
# `gh pr comment` always creates a comment, so a pull request that gets ten
# pushes ends up with ten API reports, nine of them stale, and the reviewer has
# to work out which one is current. A hidden marker makes the comment findable,
# so subsequent runs PATCH it instead.
#
# The marker is prepended here rather than written by apidiff.sh: the report is
# also `cat`ed into the job summary, and keeping the marker out of the report
# body keeps that plumbing detail out of the shared artefact.
set -euo pipefail

report=${1:?usage: comment-apidiff.sh REPORT PR_NUMBER}
pr=${2:?usage: comment-apidiff.sh REPORT PR_NUMBER}
marker='<!-- go-imap-apidiff -->'

body=$(printf '%s\n%s\n' "$marker" "$(cat "$report")" | jq -Rs '{body: .}')

comment_id=$(gh api --paginate "repos/$GITHUB_REPOSITORY/issues/$pr/comments" \
  --jq ".[] | select(.body | contains(\"$marker\")) | .id" | head -n 1)

if [ -n "$comment_id" ]; then
  gh api --method PATCH \
    "repos/$GITHUB_REPOSITORY/issues/comments/$comment_id" --input - <<<"$body"
else
  gh api --method POST \
    "repos/$GITHUB_REPOSITORY/issues/$pr/comments" --input - <<<"$body"
fi
