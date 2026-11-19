# Cloud Harness

Cloud Harness is a standalone Go agent runtime. One run manager, one tool broker, and one permission check sit in front of one SQLite record of what happened.

This tree is milestone M5. It runs the tool loop, resumes a waiting run, retrieves history, polls, runs a pre-authorized Starlark worker, and delegates a child run. The default model is a mock and needs no API key.

## Requirements

- Go 1.27.3, pinned by the `go 1.27.3` line in `go.mod`. `go mod tidy` on this release deletes a redundant `toolchain go1.27.3` line, and `go test` then refuses the file until that line is gone. The `go` line is the pin that stays tidy.
- No C compiler. SQLite is the pure-Go driver `modernc.org/sqlite` v1.62.0 (SQLite 3.54.1, FTS5 enabled).

## Run

```bash
go build -o cloud-harness ./cmd/cloud-harness

./cloud-harness run --message "检索历史"
./cloud-harness serve
./cloud-harness eval suite
./cloud-harness isolation
./cloud-harness program check
```

`serve` listens on `127.0.0.1:19421`. A non-loopback address is refused unless `CLOUD_HARNESS_ALLOW_EXTERNAL=1`.

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
- [docs/tools.md](docs/tools.md) tool, poll, script, and MCP mapping
- [docs/errors.md](docs/errors.md) error codes
- [docs/state-machine.md](docs/state-machine.md) run and invocation transitions
- [docs/worker-starlark.md](docs/worker-starlark.md) worker isolation
- [docs/eval/tasks.json](docs/eval/tasks.json) task list
- [docs/m5-report.md](docs/m5-report.md) M5 evidence, after that milestone is recorded

Milestone reports `docs/m0-report.md` through `docs/m4-report.md` are the historical records. They are not rewritten when a later milestone lands.
