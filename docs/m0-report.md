# M0 report

Date: 2026-11-10. Host: Linux x86-64 (`Linux cursor 6.12.113+`). Go: `go1.27.3 linux/amd64`.

All commands below were run in this workspace after `go mod tidy`. Exit codes are the ones the commands returned.

## Go version pin

`go.mod` begins:

```
module github.com/cloud-harness/cloud-harness

go 1.27.3
```

That line requires Go 1.27.3 or newer. A separate `toolchain go1.27.3` line was tried. `go mod tidy -diff` deletes it as redundant, and `go test` then fails with `updates to go.mod needed` until tidy is allowed to remove it. The tidy-clean pin is the `go` line.

Direct dependencies:

- `modernc.org/sqlite` v1.62.0
- `go.starlark.net` v0.0.0-20261021141710-4f4267fb140d

## SQLite driver

Driver: `modernc.org/sqlite` (import name `sqlite`). It is a pure-Go translation of SQLite, so the build does not use cgo and does not need a C compiler.

Why not `github.com/mattn/go-sqlite3`: that package is cgo. It is usually faster and can link a system libsqlite, but cross-compilation then needs a C toolchain per target, and FTS5 depends on how that library was built. M0 has to build and test without a C compiler and still create an FTS5 table.

A one-off program against v1.62.0 printed:

```
sqlite 3.54.1
fts5 ok
```

`PRAGMA compile_options` on that build includes `ENABLE_FTS5`. The store opens the file with WAL, `busy_timeout`, `synchronous=NORMAL`, and foreign keys. `SetMaxOpenConns(1)` keeps a single writer from deadlocking on a nested query. The store interface does not expose `*sql.Tx`.

Tradeoff: the pure-Go binary is larger and slower than cgo SQLite. FTS5 behavior matches the queries in `internal/storage/sqlite/sqlite_test.go`. Windows and macOS were cross-compiled, not executed, so file-locking differences on those systems are unmeasured.

## Commands

### `go build ./...`

Exit 0. No output.

### `go vet ./...`

Exit 0. No output.

### `go test ./... -count=1 -timeout 120s`

Exit 0.

```
ok  	github.com/cloud-harness/cloud-harness/cmd/cloud-harness	0.012s
ok  	github.com/cloud-harness/cloud-harness/internal/agent	0.017s
ok  	github.com/cloud-harness/cloud-harness/internal/config	0.003s
ok  	github.com/cloud-harness/cloud-harness/internal/context	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/domain	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/history	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/program	1.013s
ok  	github.com/cloud-harness/cloud-harness/internal/provider	0.006s
?   	github.com/cloud-harness/cloud-harness/internal/runtime	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/security	0.001s
?   	github.com/cloud-harness/cloud-harness/internal/storage	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/storage/sqlite	0.024s
ok  	github.com/cloud-harness/cloud-harness/internal/subagent	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/tools	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/trace	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/transport	0.017s
?   	github.com/cloud-harness/cloud-harness/migrations	[no test files]
ok  	github.com/cloud-harness/cloud-harness/tests/architecture	0.002s
```

`internal/program` includes a one-second CPU-limit proof, which is why that package takes about a second.

### `go test -race ./... -count=1 -timeout 180s`

Exit 0. Same packages. The program package took 8.092s under the race detector; every other tested package finished in about 1.0–1.3s. No race reports.

### Cross builds

These produced binaries and were not run.

```
GOOS=windows GOARCH=amd64 go build -o /tmp/cloud-harness.exe ./cmd/cloud-harness
GOOS=darwin GOARCH=arm64 go build -o /tmp/cloud-harness-darwin ./cmd/cloud-harness
GOOS=linux GOARCH=arm64 go build -o /tmp/cloud-harness-arm64 ./cmd/cloud-harness
```

`file` reported a Windows PE32+ console executable, a Mach-O arm64 executable, and an ELF aarch64 executable. Exit 0 for each build.

### CLI

`./cloud-harness version`

```
cloud-harness 0.0.0-m0 127.0.0.1:19421
```

`./cloud-harness isolation` (exit 0)

```json
{"goos":"linux","separate_process":true,"rlimit_as":true,"rlimit_cpu":true,"rlimit_fsize":true,"rlimit_nofile":true,"fsize_write_blocked":true,"cgroup_memory":false,"cgroup_detail":"memory.max: open /sys/fs/cgroup/system.slice/pod-khd4yry42jd6tgmmsax2hbibaq-44cb2c1e/cursor-agent/workload/ch-m0-12177/memory.max: permission denied","parent_verified":true,"secret_env_blocked":true,"programmatic_enabled":true}
```

`./cloud-harness program check` (exit 0)

```json
{"data":{"ping":true},"error":null,"hash":"893ac1e769dc9c75a15e0e760fe96c5daf9c01d549944afca80c0b8209af1aa4","ok":true,"probe":{"goos":"linux","separate_process":true,"rlimit_as":true,"rlimit_cpu":true,"rlimit_fsize":true,"rlimit_nofile":true,"fsize_write_blocked":true,"cgroup_memory":false,"cgroup_detail":"memory.max: open /sys/fs/cgroup/system.slice/pod-khd4yry42jd6tgmmsax2hbibaq-44cb2c1e/cursor-agent/workload/ch-m0-12189/memory.max: permission denied","parent_verified":true,"secret_env_blocked":true,"programmatic_enabled":true}}
```

The cgroup detail is the same permission error as `isolation`. The echo call went through the broker.

`./cloud-harness run --db /tmp/m0-demo.db --provider mock --message "检索历史"` (exit 0)

```json
{"error":null,"final_answer":"Mock answer: 检索历史","run_id":"run_00d636274e14a28d1201506a79877830","session_id":"ses_6e990915f089253c62299297b8a43871","status":"COMPLETED","wait_reason":""}
```

`./cloud-harness serve --addr 0.0.0.0:19421 --db /tmp/m0-demo.db` (exit 1)

```
external_bind_refused: refusing to listen on 0.0.0.0:19421; M0 has no authentication. Use 127.0.0.1 or set CLOUD_HARNESS_ALLOW_EXTERNAL=1 only on a trusted network
```

`CLOUD_HARNESS_PROVIDER=openai ./cloud-harness run --db /tmp/m0-openai.db --provider openai --message hi` with no key (exit 1)

```json
{"error":{"code":"provider_not_configured","message":"OPENAI_API_KEY is not set; the mock provider needs no key"},"final_answer":"","run_id":"run_44c70c8d5be08ec37368e936485223eb","session_id":"ses_4528fe9b791532c2d7f02e2361497248","status":"FAILED","wait_reason":""}
```

No live model was called.

## What the tests actually covered

- Run and invocation tables, including illegal edges, preserved run id, and UNKNOWN not returning to RUNNING.
- Tool batch contract: three calls in order with paired ids; mixed `finish_turn` / `ask_user` dispatch nothing; approval pause; denial skips the rest; crash recovery does not replay a success or an UNKNOWN call.
- Provider continuation contract: opaque payload and signature bytes survive replay, stay out of the trace projection and the index text, and are refused across providers. The mock records the follow-up request and the bytes match.
- SQLite migration reopens at version 1, journal mode is WAL, foreign keys reject an orphan turn.
- History: two-character Chinese words `检索`, `历史`, `框架` hit; `云计算` does not; `/var/log/app/server.go` and `server.go` are exact and do not hit a document that only says "server" and "go"; `parseToolCallID` and `ToolCall` hit; `evt_9f3a2c` is exact; `10086` hits and `100` does not. A forced rollback leaves neither the document nor the FTS row. Rebuild from raw text still answers `检索` and `server.go`.
- Invocation update and its event share a transaction: a duplicate event id rolls the status change back.
- Two lease owners: the second cannot acquire a live lease; an expired owner cannot heartbeat; the other owner can take over.
- Opaque bytes round-trip from `provider_continuations` and are absent from visible items and run events.
- Broker: schema errors and denials do not execute or charge; a second call past the tool cap does not execute. An MCP-shaped adapter cannot skip the broker.
- Worker: syntax error, disabled `load`, undefined `open`, step limit, broker allow plus `auth_denied`, parent `/proc` verification, secrets stripped from the child environment, `RLIMIT_CPU` killing a burn loop, cancel while a script is blocked in `broker.invoke`. The parent process's Starlark entry counter stays 0.
- HTTP: mock session and run, SSE ids on persistent events, `Last-Event-ID` does not replay older events, a preview delta has no id and is not stored, resume of a WAITING run returns 501 and keeps the same run, cancel is idempotent.

## Known limitations

- Tool batches are planned and stored. The run manager does not execute them. A model response that contains tool calls ends the run as FAILED with `not_implemented` and every invocation still PENDING and not dispatched. Driving the batch is covered by domain tests only.
- Resume and budget extension return `not_implemented` and do not create another run.
- Repeat detection (the "3 identical calls" reminder) is not implemented. The constant `DefaultRepeatWarning = 3` is fixed so M2 does not invent a second number.
- Checkpoints, approvals, turn summaries, and session memories have tables and no writers.
- There is no JSON Schema library. The broker checks that arguments are an object and that required names are present.
- Official OpenAI and Anthropic SDKs are not imported. The live path is a stdlib Chat Completions client. It cannot replay opaque items and returns `opaque_replay_unsupported` instead of dropping them.
- cgroup v2 `memory.max` is not writable in this environment, so there is no RSS cap. `RLIMIT_AS` is 8 GiB because a Go process died below about 2 GiB of virtual address space during the probe. That is a coarse cap, not a 64 MB script sandbox. `RLIMIT_NPROC` is not set because it is per-user.
- Programmatic execution is compiled off for non-Linux. Those binaries were not run. Windows Job Objects and macOS limits are unverified, so the feature stays disabled there rather than running the script in-process.
- `runs list` prints the store struct with Go field names. The HTTP API uses the lower-case JSON view.
- Sub-agent delegation returns `not_implemented`.
- The eval set in `docs/eval/tasks.json` has `"executed": false`.

## Deviations from the plan

- `internal/config` is an extra package. Loading env and JSON there keeps `domain` on the standard library.
- Chinese segmentation is overlapping bigrams plus an exact-term table, not gse. Bigrams match the two-character requirement without a dictionary. unicode61 splits on `.`, `_`, and `/`, so paths and ids are equality lookups. gse can be reconsidered in M3 if phrase quality needs a dictionary.
- Budget is charged only after permission allows. A denial does not consume the ledger. The executor still cannot run unless both the decision is allow and the charge succeeds.
- `PENDING` plus `protocol_reject` goes to FAILED without entering RUNNING, so a refused batch can store one paired error per call id. The architecture's invocation list did not name that edge. It is in `docs/state-machine.md` and the transition test.
- Default caps besides the confirmed model limit of 100: 300 tool calls, 50 script calls, 4 child runs. They are constants in `domain.DefaultBudget`, not a second policy.

## Real provider call (user machine)

The user ran this on their own machine. It was not run in the VM that produced the rest of this report, and no API key is recorded here.

```
OPENAI_BASE_URL=https://apihub.agnes-ai.com/v1 OPENAI_MODEL=agnes-2.5-flash cloud-harness run --provider openai --message 'Say hello in one sentence.'
```

The user reported exit 0, status COMPLETED, run id `run_82a3d5f168d8465a45c8980246ae3a4b`.
