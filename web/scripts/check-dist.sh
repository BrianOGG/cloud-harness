#!/usr/bin/env bash
# Rebuilds the Web UI and compares it with the git index (HEAD in CI).
# Staged build output can therefore be checked before committing it.
set -euo pipefail

cd "$(dirname "$0")/.."
npm run build

untracked="$(git ls-files --others -- dist)"
if ! git diff --quiet -- dist || [ -n "$untracked" ]; then
  echo "web/dist does not match a fresh build of the Web UI source:" >&2
  git diff --stat -- dist >&2
  if [ -n "$untracked" ]; then echo "$untracked" >&2; fi
  echo "Run 'npm run build' in web/ with Node.js 22, stage and commit web/dist." >&2
  exit 1
fi
echo "web/dist matches a fresh build."
