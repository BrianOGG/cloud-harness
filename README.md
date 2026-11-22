# Cloud Harness

Cloud Harness is a standalone Go agent runtime. One run manager, one tool broker, and one permission check sit in front of one SQLite record of what happened.

This tree runs the tool loop, resumes a waiting run, retrieves history, polls, runs a pre-authorized Starlark worker, and delegates child runs. The default model is a mock and needs no API key.

Set `agent` in a `delegate_task` call to select `explorer`, `reviewer`, or a definition from `.agents/agents/*.md`. The definition can narrow the child's tools, cap its budget, and set its model and system instructions. [Child agent reference](docs/cli.md#select-an-agent-with-delegate_task)

## Requirements

- Go 1.27.4, pinned by the `go 1.27.4` line in `go.mod`. `go mod tidy` on this release deletes a redundant `toolchain go1.27.4` line, and `go test` then refuses the file until that line is gone. The `go` line is the pin that stays tidy. Go 1.27.3 has a `net/http` server vulnerability ([GO-2026-6859](https://pkg.go.dev/vuln/GO-2026-6859)) that `govulncheck` reports for `serve`.
- No C compiler. SQLite is the pure-Go driver `modernc.org/sqlite` v1.62.0 (SQLite 3.54.1, FTS5 enabled).
- Node.js 22 (22.12 or later) and npm to rebuild the Web UI. Go builds use the checked-in output in `web/dist/`.

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
./cloud-harness repomap --dir .
```

`serve` listens on `127.0.0.1:19421`; loopback addresses can start without a token.
A non-loopback address requires `--http-token TOKEN`, `CLOUD_HARNESS_HTTP_TOKEN`,
or JSON `http_token` (at least 16 characters), checked before binding a port.
Legacy insecure/allow-external settings cannot bypass this requirement.
A loopback listener refuses a `Host` header that is not `localhost` or a
loopback IP address unless it is listed in `http_allowed_hosts`. Every listener
refuses a cross-origin `Origin` and requires `Content-Type: application/json`
on write requests with a body; see [request checks](docs/api.md#request-checks).
`serve` stops gracefully on `SIGINT` and `SIGTERM`: runs that are executing
are cancelled and recorded as `CANCELLED` before the store closes.
With a configured token, `/v1/` routes require `Authorization: Bearer <token>`.
`/healthz` and the static pages at `/approval` and `/ui` are public. Open
`/approval` on desktop or mobile to view the pending queue, make an approval
decision, or answer a question. Open `/ui` to start sessions, send messages,
and inspect runs. Paste the configured token once on `/approval`; both pages
share it on the same origin. See the [Web UI reference](docs/api.md#get-ui)
and [approval page reference](docs/api.md#get-approval).
Optional webhook, Bark, and ntfy notifications report approval waits, user
questions, completion, and failure or cancellation. Set `notify.approval_base_url`
to include a direct link to the waiting run. See
[notification configuration](docs/config.md#run-notifications).

On Linux, `run_command` processes and their children run under kernel resource
limits (memory, CPU time, file size, open files, process count). The defaults
fit ordinary builds and tests; adjust or disable them with `command_limits`.
See [command resource limits](docs/config.md#command-resource-limits).
On Linux and macOS the same processes cannot read or write `.cloud-harness`
or the harness configuration directory; see
[control files on macOS](docs/config.md#control-files-on-macos) for the macOS
mechanism and its limits. On macOS the first command creates both directories,
empty, if they do not exist yet.

Harness control files are protected from the agent. File tools cannot read or
write anything under `.cloud-harness/` (hooks, policy, the default database,
worktree metadata); no policy rule can override this. The workspace
`.cloud-harness/hooks.json` and `.cloud-harness/policy.json` take effect only
after you trust their exact content with `cloud-harness trust approve`. Trust is
a content hash stored in the configuration directory, outside the workspace, so
any later change needs a new approval. Until then workspace hooks are not run
and workspace `allow` rules are not applied; `run`, `chat`, `serve`, and
`doctor` say so. Approvals for writes to `AGENTS.md` and `.agents/` carry a
warning that the write changes later runs. See [policy](docs/policy.md),
[hooks](docs/hooks.md), and [`trust`](docs/cli.md#trust).

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

Local stdio MCP servers listed under `servers` in the configuration file are
started by `serve`, `run`, `chat`, `runs resume`, and `runs recover`. Their tools
appear to the model as `mcp.<server>.<tool>` and go through the same policy,
approval, budget, hook, and audit path as built-in tools; by default every call
waits for approval. A server that fails to start is skipped with a warning, and
`doctor` shows each server's real state. See
[docs/config.md](docs/config.md#mcp-servers) and [docs/tools.md](docs/tools.md#mcp).

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

`eval suite` runs that list and prints completion, correctness, wall time, model and tool call totals, token totals the tasks actually reported, cache hit rate, planner arms, repeated tool calls, and recovery.

For a real model, run the live subset with a configured provider:

```bash
cloud-harness eval suite --live --provider openai --out live-report.json
cloud-harness eval suite --live --provider openai --compare live-baseline.json
```

`--live` rejects the mock provider and uses the `live_tasks` list in [docs/eval/tasks.json](docs/eval/tasks.json); see [docs/eval/live-suite.md](docs/eval/live-suite.md).

## Repository symbol map

`cloud-harness repomap` scans the current directory (or `--dir PATH`) and
prints a deterministic, compact map for later context assembly and manual
inspection. The default output limit is 64 KiB; use `--max-bytes N` to change
it. Each supported source file is one tab-separated line:

```text
main.go	lines=6	package=main	symbols=type:App,func:NewApp,method:(*App).Run
script.py	lines=3	symbols=def:hello
```

The first field is the path relative to the scanned directory. Go files list
their package and top-level `type`, `func`, and receiver method declarations;
methods include the receiver type. Python, JavaScript, TypeScript, Rust, Java,
and Ruby files use lightweight top-level `def`/`function`/`fn`/`class` style
matching. A supported file with no matching definition still records its path
and line count. Symbols are de-duplicated within each file and retain source
order; files are sorted by path.

`.git`, `.cloud-harness`, `node_modules`, `vendor`, `dist`, and `__pycache__`
directories are skipped. A file that cannot be read or whose Go syntax is
invalid is skipped; the command continues and reports a diagnostic on stderr.
When the byte limit is reached, the map keeps the longest complete prefix of
file lines and ends with `# TRUNCATED omitted_files=N`, where `N` is the number
of files omitted from the map. An empty workspace produces an empty stdout map.
The limit includes the truncation marker; if it is too small to fit the marker
and its file count, the command exits with an error and does not print a partial
map.

A live OpenAI-compatible call uses `OPENAI_API_KEY` only from the environment. See [docs/config.md](docs/config.md).

```bash
export OPENAI_API_KEY=sk-...
export OPENAI_BASE_URL=https://api.openai.com/v1
export OPENAI_MODEL=gpt-4.1-mini
./cloud-harness run --provider openai --message "Say hello in one sentence."
./cloud-harness eval run --task batch-three-reads --provider openai
```

## Release artifacts

Release builds produce Go binaries for Linux, macOS, and Windows plus a container image. The `release` workflow runs on `v*` tags; see [docs/release.md](docs/release.md) for local builds and `ghcr.io/brianogg/cloud-harness`.

```bash
go build -o cloud-harness ./cmd/cloud-harness
docker build -t cloud-harness .
```

## Trace and events

`cloud-harness trace RUN_ID` and `GET /v1/runs/{id}/trace` return Session → Run → Step → tool batch, plus child runs. `GET /v1/runs/{id}/events` replays persistent events after `Last-Event-ID`, then stays open and sends new ones as they are committed. Reconnecting does not execute tools again.

A run created over HTTP executes in the server: closing the request does not stop it, and only `POST /v1/runs/{id}/cancel` does. `POST /v1/sessions/{id}/runs` returns as soon as the run is `RUNNING`; add `?wait=1` to have it answer once the run has stopped, as it did before M13-2. The same holds for `/responses`, `/answer`, and `/budget`. While the model generates a response, the event stream sends its text as `stream.delta` events. A `stream.delta` has no id and is not stored, so it is not replayed: after a reconnect, read the remaining persistent events and the run's `final_answer`. `cloud-harness chat` prints the same text in the terminal as it arrives (`--stream=false` turns that off); `cloud-harness run --stream` writes it to stderr and keeps stdout to its one JSON line. See [docs/api.md](docs/api.md#get-v1runsidevents) and [docs/cli.md](docs/cli.md#run--chat).

The authenticated `GET /v1/sessions` and `GET /v1/sessions/{id}/runs` API
routes list sessions with run aggregates and each session's runs. See the
[HTTP API reference](docs/api.md#get-v1-sessions).

`cloud-harness retain --days 30` deletes history-index rows older than 30 days. Run rows stay.

## Layout

```
cmd/cloud-harness          CLI and the worker subprocess entry
internal/domain            state machine, batches, continuation (no I/O)
internal/agent             run manager, resume, child runs, scripts
internal/provider          mock and OpenAI-compatible HTTP adapter
internal/context           prefix, memory, planner, compaction
internal/history           Chinese bigrams, paths, identifiers
internal/tools             broker, poll, stdio MCP servers
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
web                        Web UI source and embedded Vite output
```

Rebuild the Web UI after changing its source, and include the updated
`web/dist/` in the change. The Go build embeds those assets into the executable.

```bash
(cd web && npm ci && npm run build)
go build -o cloud-harness ./cmd/cloud-harness
```

CI rebuilds the Web UI and fails when `web/dist/` differs from the result.
[web/README.md](web/README.md) covers frontend development, tests, and this rule.

## Tests

```bash
gofmt -l .                      # prints nothing when the tree is formatted
go build ./...
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
golangci-lint run ./...         # v2.15.0, configured by .golangci.yml
go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
(cd web && npm ci && npm run lint && npm test && npm run check:dist)
```

Non-Linux builds compile and refuse programmatic execution. They do not interpret Starlark in the harness process.

### CI

`.github/workflows/ci.yml` runs the commands above on every pull request and on pushes to `main` and `dev`, in five jobs:

| Job | Runs |
|---|---|
| `go / test` | `gofmt -l .`, `go build`, `go vet`, `go test`, `go test -race` |
| `go / lint and govulncheck` | `golangci-lint` v2.15.0 and `govulncheck` v1.10.0 |
| `go / macOS command sandbox` | On a macOS runner, the `run_command` tests in `internal/tools`, including the Seatbelt isolation of control files; fails rather than skips if the sandbox cannot be applied |
| `go / build darwin/arm64`, `go / build windows/amd64` | `CGO_ENABLED=0 go build ./...` for each target |
| `web / lint, test, build, dist` | `npm ci`, `npm run lint`, `npm test`, `npm run check:dist` on Node.js 22 |

The Go jobs read the Go version from `go.mod`; the web job reads the Node.js version from `web/.nvmrc`. `govulncheck` reads the live vulnerability database, so a newly published advisory can fail CI on a commit that passed before.

### Lint baseline

`.golangci.yml` enables `govet`, `ineffassign`, `staticcheck`, `unused`, and `errcheck`. `errcheck` reports unchecked `fmt.Fprint*`/`io.WriteString` to CLI writers, deferred `Close` calls, and `rows.Close` calls; those are now assigned to `_` explicitly so the linter gates new code. The config still leaves these out:

- `staticcheck` quick-fix suggestions (`QF*`) are off. They report 11 style suggestions.
- 13 existing findings are excluded by file and message under `linters.exclusions.rules`: four uses of the deprecated `runtime.GOROOT` in `internal/hooks`, three unused declarations, two ineffectual assignments, and four smaller `staticcheck` findings. The same finding in any other file still fails. Delete a rule when its finding is fixed.

Cross-compiled targets are built, not vetted or tested: `GOOS=windows go vet ./...` fails because `internal/tools/stdio_mcp_test.go` calls `syscall.Kill`.

## License

Apache-2.0. See [LICENSE](LICENSE).

## Docs

- [docs/cli.md](docs/cli.md) rewind modes, preview, confirmation, and safety snapshots
- [docs/config.md](docs/config.md) configuration
- [docs/policy.md](docs/policy.md) layered policy, builtin baseline, and workspace file trust
- [docs/hooks.md](docs/hooks.md) lifecycle hooks and workspace hook trust
- [docs/api.md](docs/api.md) approval queue and HTTP authentication
- [docs/tools.md](docs/tools.md) tool, poll, script, and stdio MCP servers
- [docs/errors.md](docs/errors.md) error codes
- [docs/state-machine.md](docs/state-machine.md) run and invocation transitions
- [docs/worker-starlark.md](docs/worker-starlark.md) worker isolation
- [docs/eval/tasks.json](docs/eval/tasks.json) task list
- [docs/eval/live-suite.md](docs/eval/live-suite.md) real-model eval subset
- [docs/release.md](docs/release.md) binaries and container image
- [docs/m5-report.md](docs/m5-report.md) M5 evidence, after that milestone is recorded

Milestone reports `docs/m0-report.md` through `docs/m4-report.md` are the historical records. They are not rewritten when a later milestone lands.
