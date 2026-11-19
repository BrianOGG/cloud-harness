#!/usr/bin/env bash
set -u

ROOT_DIR="$(cd "$(dirname "$0")/../.." && pwd)"
ARTIFACT_DIR="$ROOT_DIR/.cloud-harness/m11-15"
LOG_FILE="$ARTIFACT_DIR/acceptance-final.log"
mkdir -p "$ARTIFACT_DIR"

set +e
{
  failed=0
  run_check() {
    label="$1"
    shift
    printf '\n$'
    printf ' %s' "$@"
    printf '\n'
    "$@"
    result=$?
    printf 'EXIT_CODE=%s (%s)\n' "$result" "$label"
    if [ "$result" -ne 0 ]; then failed=1; fi
  }

  cd "$ROOT_DIR/web"
  run_check 'npm ci' npm ci
  run_check 'npm run lint' npm run lint
  run_check 'npm test' npm test
  run_check 'npm run build' npm run build

  cd "$ROOT_DIR"
  run_check 'go build ./...' go build ./...
  run_check 'go vet ./...' go vet ./...
  printf '\n$ gofmt -l .\n'
  gofmt_output=$(gofmt -l .)
  gofmt_result=$?
  if [ -n "$gofmt_output" ]; then printf '%s\n' "$gofmt_output"; fi
  if [ "$gofmt_result" -eq 0 ] && [ -z "$gofmt_output" ]; then
    printf 'EXIT_CODE=0 (gofmt -l .; no files need formatting)\n'
  else
    printf 'EXIT_CODE=1 (gofmt -l .; formatting issues found)\n'
    failed=1
  fi
  run_check 'go test -p 1 ./...' go test -p 1 ./...
  run_check 'CGO_ENABLED=1 go test -race ./internal/agent/ ./internal/snapshot/ ./internal/agents/ ./internal/worktree/ ./internal/notify/ ./internal/transport/ ./internal/storage/sqlite/' env CGO_ENABLED=1 go test -race ./internal/agent/ ./internal/snapshot/ ./internal/agents/ ./internal/worktree/ ./internal/notify/ ./internal/transport/ ./internal/storage/sqlite/
  run_check 'serve binary build' go build -o /tmp/cloud-harness-m11-15 ./cmd/cloud-harness
  if [ ! -x "${M11_FIREFOX_BINARY:-/tmp/m11-8-firefox/usr/lib/firefox/firefox}" ] || [ ! -x "${M11_GECKODRIVER:-/tmp/m11-8-gecko/usr/bin/geckodriver}" ]; then
    browser_dir=/tmp/m11-15-browser-install
    run_check 'headless Firefox install for browser screenshots' python3 web/scripts/install-headless-firefox.py "$browser_dir"
    export M11_FIREFOX_BINARY="$browser_dir/usr/lib/firefox/firefox"
    export M11_GECKODRIVER="$browser_dir/usr/bin/geckodriver"
  fi
  run_check 'real serve scripted-provider browser smoke' python3 web/scripts/serve-acceptance.py /tmp/cloud-harness-m11-15

  if [ "$failed" -eq 0 ]; then
    printf '\nACCEPTANCE_RESULT=PASS\n'
  else
    printf '\nACCEPTANCE_RESULT=FAIL\n'
  fi
  exit "$failed"
} 2>&1 | tee "$LOG_FILE"
exit "${PIPESTATUS[0]}"
