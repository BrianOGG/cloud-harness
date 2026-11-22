#!/usr/bin/env bash
# Rebuilds the Web UI and fails when the result differs from the checked-in
# web/dist, which is what the Go binary embeds.
set -euo pipefail

cd "$(dirname "$0")/.."
npm run build

changes="$(git status --porcelain -- dist)"
if [ -n "$changes" ]; then
  echo "web/dist does not match a fresh build of the Web UI source:" >&2
  echo "$changes" >&2
  echo "Run 'npm run build' in web/ with Node.js 22 and commit web/dist." >&2
  exit 1
fi
echo "web/dist matches a fresh build."
