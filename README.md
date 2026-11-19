# Cloud Harness

Cloud Harness is a standalone Go agent runtime. One run manager, one tool broker, and one permission check sit in front of one SQLite record of what happened.

This tree runs the tool loop, resumes a waiting run, retrieves history, polls, runs a pre-authorized Starlark worker, and delegates child runs. The default model is a mock and needs no API key.

Set `agent` in a `delegate_task` call to select `explorer`, `reviewer`, or a definition from `.agents/agents/*.md`. The definition can narrow the child's tools, cap its budget, and set its model and system instructions. [Child agent reference](docs/cli.md#select-an-agent-with-delegate_task)

## Requirements

- Go 1.27.3, pinned by the `go 1.27.3` line in `go.mod`. `go mod tidy` on this release deletes a redundant `toolchain go1.27.3` line, and `go test` then refuses the file until that line is gone. The `go` line is the pin that stays tidy.
- No C compiler. SQLite is the pure-Go driver `modernc.org/sqlite` v1.62.0 (SQLite 3.54.1, FTS5 enabled).

## Run

```bash
go build -o cloud-harness ./cmd/cloud-harness

./cloud-harness run --message "检索历史"
./cloud-harness run --worktree --message "Review this Git project in isolation"
./cloud-harness chat --worktree --message "Inspect this Git project"
./cloud-harness serve --worktree
./cloud-harness worktrees list --json
./cloud-harness worktrees diff RUN_ID
./cloud-harness worktrees merge RUN_ID
./cloud-harness worktrees discard RUN_ID --force
./cloud-harness eval suite
./cloud-harness isolation
./cloud-harness program check
./cloud-harness agents --workspace .
```

`serve` listens on `127.0.0.1:19421`; loopback addresses can start without a token.
A non-loopback address requires `--http-token TOKEN`, `CLOUD_HARNESS_HTTP_TOKEN`,
or JSON `http_token` (at least 16 characters), checked before binding a port.
Legacy insecure/allow-external settings cannot bypass this requirement.
With a configured token, `/v1/` routes require `Authorization: Bearer <token>`.
`/healthz` and the static `/approval` page are public. Open `/approval` on desktop
or mobile and paste the configured token once to view the pending queue, make
an approval decision, or answer a question. See the
[approval page reference](docs/api.md#get-approval).
Optional webhook, Bark, and ntfy notifications report approval waits, user
questions, completion, and failure or cancellation. Set `notify.approval_base_url`
to include a direct link to the waiting run. See
[notification configuration](docs/config.md#run-notifications).

`run`, `chat`, and `serve` optionally accept `--worktree` in a Git workspace. Each run
starts from its dispatch-time HEAD on `cloud-harness/<run-id>` under
`.cloud-harness/worktrees/<run-id>/`; files are isolated while the database
stays in the original workspace. A final `worktree.summary` event records the
base SHA, file list, stat, and capped diff (including pending edits). Worktrees
are retained for `worktrees list/diff/merge/discard`. Merge integrates main into
the agent branch first, allows two conflict-repair rounds, then gates integration
with tests and rolls main back on failure. Lifecycle mutations are serialized
within one process; do not run concurrent lifecycle CLI processes against one
repository. `doctor` reports orphans and `retain --prune-runs-days` can reclaim
them. See [worktree lifecycle](docs/cli.md#worktrees) for states and recovery.

Resume flags come before the run id:

```bash
cloud-harness runs resume --decision deny RUN_ID
cloud-harness runs resume --answer "the user's reply" RUN_ID
cloud-harness runs resume --extra-model-calls 10 RUN_ID
```

Putting the run id first makes the flag parser stop early and return `run id required`.

Scripted evals, no key:

```bash
cloud-harness eval run --task batch-approval-skip
cloud-harness eval run --task recover-mid-batch
cloud-harness eval run --task retrieve-chinese-term
cloud-harness eval run --task retrieve-path-and-id
cloud-harness eval run --task planner-compare
cloud-harness eval run --task cache-baseline
cloud-harness eval run --task incident-wait-for-ready
```

`eval suite` runs that list and prints completion, correctness, wall time, token totals the tasks actually reported, cache hit rate, planner arms, repeated tool calls, and recovery.

A live OpenAI-compatible call uses `OPENAI_API_KEY` only from the environment. See [docs/config.md](docs/config.md).

```bash
export OPENAI_API_KEY=sk-...
export OPENAI_BASE_URL=https://api.openai.com/v1
export OPENAI_MODEL=gpt-4.1-mini
./cloud-harness run --provider openai --message "Say hello in one sentence."
./cloud-harness eval run --task batch-three-reads --provider openai
```

## Trace and events

`cloud-harness trace RUN_ID` and `GET /v1/runs/{id}/trace` return Session → Run → Step → tool batch, plus child runs. `GET /v1/runs/{id}/events` replays persistent events after `Last-Event-ID`. A `stream.delta` has no id and is not stored. Reconnecting does not execute tools again.

`cloud-harness retain --days 30` deletes history-index rows older than 30 days. Run rows stay.

## Layout

```
cmd/cloud-harness          CLI and the worker subprocess entry
internal/domain            state machine, batches, continuation (no I/O)
internal/agent             run manager, resume, child runs, scripts
internal/provider          mock and OpenAI-compatible HTTP adapter
internal/context           prefix, memory, planner, compaction
internal/history           Chinese bigrams, paths, identifiers
internal/tools             broker, poll, MCP spec mapping
internal/security          scoped grants
internal/program           Starlark worker and isolation gate
internal/subagent          delegate contract; the live runner is the agent
internal/storage           store interface
internal/storage/sqlite    migrations, WAL, FTS5
internal/trace             persistent events, SSE, trace view
internal/transport         HTTP API
migrations                 schema scripts
tests/architecture         import-boundary test
docs                       reports, config, tools, errors, eval tasks
```

## Tests

```bash
go test ./...
go vet ./...
go test -race ./...
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/cloud-harness-darwin ./cmd/cloud-harness
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/cloud-harness-windows.exe ./cmd/cloud-harness
```

Non-Linux builds compile and refuse programmatic execution. They do not interpret Starlark in the harness process.

## Docs

- [docs/cli.md](docs/cli.md) rewind modes, preview, confirmation, and safety snapshots
- [docs/config.md](docs/config.md) configuration
- [docs/api.md](docs/api.md) approval queue and HTTP authentication
- [docs/tools.md](docs/tools.md) tool, poll, script, and MCP mapping
- [docs/errors.md](docs/errors.md) error codes
- [docs/state-machine.md](docs/state-machine.md) run and invocation transitions
- [docs/worker-starlark.md](docs/worker-starlark.md) worker isolation
- [docs/eval/tasks.json](docs/eval/tasks.json) task list
- [docs/m5-report.md](docs/m5-report.md) M5 evidence, after that milestone is recorded

Milestone reports `docs/m0-report.md` through `docs/m4-report.md` are the historical records. They are not rewritten when a later milestone lands.
