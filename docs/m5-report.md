# M5 report

Date: 2026-11-10. Host: Linux x86-64. Go: `go1.27.3 linux/amd64`.

M5 is the deliverable pass: the CLI and trace view, history retention, property checks, a scripted effect suite, and the docs that match this tree. It does not add a product surface beyond that.

## Commands

Run from the repository root. `gofmt -l .` printed nothing. The Go commands exited 0. No data race was reported.

```text
$ go version
go version go1.27.3 linux/amd64

$ gofmt -l .
(no files)

$ go build ./...
build exit:0

$ go vet ./...
vet exit:0

$ go test ./... -count=1 -timeout 180s
ok  	github.com/cloud-harness/cloud-harness/cmd/cloud-harness	0.021s
ok  	github.com/cloud-harness/cloud-harness/internal/agent	0.258s
ok  	github.com/cloud-harness/cloud-harness/internal/config	0.003s
ok  	github.com/cloud-harness/cloud-harness/internal/context	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/domain	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/eval	0.091s
ok  	github.com/cloud-harness/cloud-harness/internal/history	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/program	1.022s
ok  	github.com/cloud-harness/cloud-harness/internal/provider	0.007s
ok  	github.com/cloud-harness/cloud-harness/internal/runtime	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/security	0.002s
?   	github.com/cloud-harness/cloud-harness/internal/storage	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/storage/sqlite	0.039s
ok  	github.com/cloud-harness/cloud-harness/internal/subagent	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/tools	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/trace	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/transport	0.020s
?   	github.com/cloud-harness/cloud-harness/migrations	[no test files]
ok  	github.com/cloud-harness/cloud-harness/tests/architecture	0.003s
test exit:0

$ go test -race ./... -count=1 -timeout 180s
ok  	github.com/cloud-harness/cloud-harness/cmd/cloud-harness	1.170s
ok  	github.com/cloud-harness/cloud-harness/internal/agent	4.622s
ok  	github.com/cloud-harness/cloud-harness/internal/config	1.010s
ok  	github.com/cloud-harness/cloud-harness/internal/context	1.011s
ok  	github.com/cloud-harness/cloud-harness/internal/domain	1.017s
ok  	github.com/cloud-harness/cloud-harness/internal/eval	2.030s
ok  	github.com/cloud-harness/cloud-harness/internal/history	1.010s
ok  	github.com/cloud-harness/cloud-harness/internal/program	9.104s
ok  	github.com/cloud-harness/cloud-harness/internal/provider	1.022s
ok  	github.com/cloud-harness/cloud-harness/internal/runtime	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/security	1.009s
?   	github.com/cloud-harness/cloud-harness/internal/storage	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/storage/sqlite	1.437s
ok  	github.com/cloud-harness/cloud-harness/internal/subagent	1.010s
ok  	github.com/cloud-harness/cloud-harness/internal/tools	1.012s
ok  	github.com/cloud-harness/cloud-harness/internal/trace	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/transport	1.182s
?   	github.com/cloud-harness/cloud-harness/migrations	[no test files]
ok  	github.com/cloud-harness/cloud-harness/tests/architecture	1.013s
race exit:0
```

## Cross-platform build

Tests above ran on linux/amd64. The other two targets were compile-only.

```text
$ CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/cloud-harness-linux-amd64 ./cmd/cloud-harness
linux/amd64 exit:0

$ CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /tmp/cloud-harness-darwin-arm64 ./cmd/cloud-harness
darwin/arm64 exit:0

$ CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /tmp/cloud-harness-windows-amd64.exe ./cmd/cloud-harness
windows/amd64 exit:0
```

## M0–M4 regression

`go test ./...` on this host is the regression run. The cases in section 13 are covered by these tests, all of which passed in that run:

| Area | Tests |
| --- | --- |
| Multi-tool batch | `TestThreeReadsExecuteInOrderOnce`, `TestBatchThreeReadsStreamingOpenAI` |
| Approval and skip | `TestApprovalDenyPairsTheWholeBatch`, eval `batch-approval-skip` |
| Unknown result, no replay | `TestCrashRecoveryDoesNotReplaySideEffects`, eval `recover-mid-batch` |
| Reasoning continuation | `TestOpaqueReplayPreservesOrderAndSignatureBytes`, `TestToolBatchIsStoredAndOpaqueStaysOutOfTrace` |
| Prompt injection | `TestRepeatReminderAndSensitiveEgress` |
| Script isolation | `TestWorkerBrokerSyntaxAndLimits`, `TestScriptCannotTouchTheSystem`, `TestScriptRefusesSideEffectsAndNewPermissions` |
| Long history | `TestHistoryChinesePathCodeAndTransaction`, eval `retrieve-chinese-term`, `retrieve-path-and-id` |
| Owner lease | `TestLiveLeaseCannotBeStolen`, `TestPropertyLeaseIsUniqueAndResumeKeepsTheRun` |

Schema upgrade is `TestMigrationWALAndForeignKeys` (version 3, reopen keeps the session). SSE persistent events versus an unstored `stream.delta` is `TestSessionRunSSEAndTrace` in `internal/transport`.

## Property tests

```text
$ go test ./internal/domain/ ./internal/security/ ./internal/storage/sqlite/ -count=1 -run 'Property|Prune' -v
--- PASS: TestPropertyOneTerminalResultAndNoReplay (0.00s)
--- PASS: TestPropertyBudgetCannotBeEvaded (0.00s)
--- PASS: TestPropertyGrantScopeDoesNotExpand (0.00s)
--- PASS: TestPropertyLeaseIsUniqueAndResumeKeepsTheRun (0.01s)
--- PASS: TestPruneHistoryKeepsNewerRows (0.01s)
property exit:0
```

They check, over repeated random inputs where that applies:

- an invocation has one terminal result and a finished call is not executed again
- calls and results stay paired
- a child or a script charge cannot pass the parent cap
- a grant for `/ws/project` does not cover a sibling or `/etc` path
- a second owner cannot take a live lease; after expiry the same run id remains
- history pruning removes the old row and keeps the newer one

## Effect evaluation

Scripted suite, no API key. Numbers are the JSON the command printed.

```text
$ cloud-harness eval suite --db "$tmpdir/suite.db" --workspace "$tmpdir/ws"
{"tasks":7,"passed":7,"completion_rate":1,"correctness":1,"wall_ms":55,"repeated_tool_calls":0,"recovery_pass":true,"input_tokens":332,"output_tokens":34,"cached_input_tokens":59,"cache_hit_rate":0.3430232558139535,"baseline_hit_rate":0,"planner":{"arms":[{"answer":"harbor-42","cached_input_tokens":0,"cost_tokens":65,"hit_rate":0,"input_tokens":50,"latency_ms":60,"model_calls":2,"name":"planner_on","quality":1,"tool_calls":0,"wall_ms":1},{"answer":"unknown","cached_input_tokens":0,"cost_tokens":24,"hit_rate":0,"input_tokens":20,"latency_ms":15,"model_calls":1,"name":"planner_off_fixed","quality":0,"tool_calls":0,"wall_ms":1},{"answer":"harbor-42","cached_input_tokens":0,"cost_tokens":103,"hit_rate":0,"input_tokens":90,"latency_ms":75,"model_calls":2,"name":"planner_off_retrieve","quality":1,"tool_calls":1,"wall_ms":2}]},"task_pass":{"batch-approval-skip":true,"cache-baseline":true,"incident-wait-for-ready":true,"planner-compare":true,"recover-mid-batch":true,"retrieve-chinese-term":true,"retrieve-path-and-id":true}}
suite exit:0
```

Correctness here is the scripted scorer, not a human grade. Token totals are the sum of the planner arms and the stable cache arm. Tasks that do not report provider usage contribute 0 tokens. Wall time is the suite clock, 55 ms.

| Comparison | Result |
| --- | --- |
| Completion | 7/7 |
| Recovery (`recover-mid-batch`) | pass |
| Repeated tool calls | 0 |
| Cache hit rate vs baseline | 0.343 vs 0 |
| Planner on cost / scripted latency | 65 tokens / 60 ms, quality 1 |
| Planner off, fixed window | 24 tokens / 15 ms, quality 0 |
| Planner off, retrieve | 103 tokens / 75 ms, quality 1 |

## Isolation

```text
$ go test ./internal/program/ -count=1 -run 'TestWorkerBrokerSyntaxAndLimits|TestDecideRejectsMissingIsolation|TestScriptCannotTouch' -v
--- PASS: TestScriptCannotTouchTheSystem (0.00s)
--- PASS: TestDecideRejectsMissingIsolation (0.00s)
--- PASS: TestWorkerBrokerSyntaxAndLimits (0.01s)
    cgroup_memory=false detail=memory.max: open .../memory.max: permission denied
```

`memory.max` is not writable here. The worker still requires the rlimit probe (`RLIMIT_AS` 8 GiB and the other limits from M0). `Decide` refuses `darwin` and `windows` even when the probe flags are set. Those two OS builds were not executed; they were only cross-compiled. A failed probe returns `program_disabled` and does not interpret the script in this process.

## Local re-run

The M5 tree was extracted from `cloud-harness-m5.tar.gz` (SHA-256 `212857f78f168e57ddbd6a64b07b495b46647742ec18073dfe869fd1bf1f8acf`, commit `5cbc4eb0f7a1789a101b432cf3c26362fd0bf72b`) and re-run on a separate Linux x86-64 host with `go1.27.3 linux/amd64` at 2026-11-10 22:31 Asia/Shanghai. `gofmt -l .` printed nothing. `go build ./...`, `go vet ./...`, `go test ./...`, and `go test -race ./... -count=1 -timeout 180s` all exited 0. No data race was reported. `internal/program` took 9.190s under the race detector.

`CGO_ENABLED=0` cross-compiles of `./cmd/cloud-harness` for linux/amd64, darwin/arm64, and windows/amd64 all exited 0. The local CLI banner is `Cloud Harness M5` (`cloud-harness 0.5.0-m5`).

```text
$ cloud-harness eval suite --db /tmp/ch-m5-suite/suite.db --workspace /tmp/ch-m5-suite/ws
{"tasks":7,"passed":7,"completion_rate":1,"correctness":1,"wall_ms":77,"repeated_tool_calls":0,"recovery_pass":true,"input_tokens":332,"output_tokens":34,"cached_input_tokens":59,"cache_hit_rate":0.3430232558139535,"baseline_hit_rate":0,"planner":{"arms":[{"answer":"harbor-42","cost_tokens":65,"latency_ms":60,"name":"planner_on","quality":1,"tool_calls":0,"wall_ms":1},{"answer":"unknown","cost_tokens":24,"latency_ms":15,"name":"planner_off_fixed","quality":0,"tool_calls":0,"wall_ms":1},{"answer":"harbor-42","cost_tokens":103,"latency_ms":75,"name":"planner_off_retrieve","quality":1,"tool_calls":1,"wall_ms":2}]},"task_pass":{"batch-approval-skip":true,"cache-baseline":true,"incident-wait-for-ready":true,"planner-compare":true,"recover-mid-batch":true,"retrieve-chinese-term":true,"retrieve-path-and-id":true}}
suite exit:0
```

The metric set matches the cloud suite. Wall time on this host was 77 ms. Targeted property and isolation tests also exited 0:

```text
$ go test ./internal/domain/ ./internal/security/ ./internal/storage/sqlite/ -count=1 -run 'Property|Prune' -v
--- PASS: TestPropertyOneTerminalResultAndNoReplay (0.00s)
--- PASS: TestPropertyBudgetCannotBeEvaded (0.00s)
--- PASS: TestPropertyGrantScopeDoesNotExpand (0.00s)
--- PASS: TestPropertyLeaseIsUniqueAndResumeKeepsTheRun (0.02s)
--- PASS: TestPruneHistoryKeepsNewerRows (0.01s)

$ go test ./internal/program/ -count=1 -run 'TestWorkerBrokerSyntaxAndLimits|TestDecideRejectsMissingIsolation|TestScriptCannotTouch' -v
--- PASS: TestScriptCannotTouchTheSystem (0.00s)
--- PASS: TestDecideRejectsMissingIsolation (0.00s)
--- PASS: TestWorkerBrokerSyntaxAndLimits (0.02s)
    cgroup_memory=false detail=mkdir /sys/fs/cgroup/agent/ch-m0-356664: permission denied
```

`cloud-harness isolation` and `cloud-harness program check` both exited 0. Isolation JSON on this host:

```json
{"goos":"linux","separate_process":true,"rlimit_as":true,"rlimit_cpu":true,"rlimit_fsize":true,"rlimit_nofile":true,"fsize_write_blocked":true,"cgroup_memory":false,"cgroup_detail":"mkdir /sys/fs/cgroup/agent/ch-m0-356495: permission denied","parent_verified":true,"secret_env_blocked":true,"programmatic_enabled":true}
```

`program check` returned `{"ok":true,"data":{"ping":true}}` with the same probe flags. The cgroup failure here is a directory create, not an open of `memory.max`. The worker is still enabled because the rlimit set can be verified. A failed probe still returns `program_disabled` and does not interpret the script in-process.

## Real provider run

Live calls used `OPENAI_BASE_URL=https://apihub.agnes-ai.com/v1` and `OPENAI_MODEL=agnes-2.5-flash` on this host at 2026-11-10 22:35–22:36 Asia/Shanghai. The key stayed in the environment. It is not in the repo, fixtures, traces, or this report.

`batch-three-reads` went through the official CLI (`eval run --task batch-three-reads --provider openai`). The retrieve cases still force a scripted provider when `Options.Provider` is nil, which is the CLI path, so those two were run through a temporary in-module harness that seeded the same history rows and called the live adapter. That harness was deleted afterwards and is not in this tree.

```text
$ cloud-harness eval run --task batch-three-reads --provider openai \
    --db /tmp/ch-m5-live-batch/eval.db --workspace /tmp/ch-m5-live-batch
```

Exit 0, `pass: true`, status `COMPLETED`, run id `run_0344813dfa94b53954717a50d6f366c9`, wall about 2.4 s (22:35:40–22:35:42 Asia/Shanghai). Three `read_file` calls ran in order: `alpha.txt` / `call_b6d9cf9aadb94f85b1da235c`, `beta.txt` / `call_ab3eecbdc530403590622c1e`, `gamma.txt` / `call_0d19a562132d42af9aec2ed1`, contents `alpha-one`, `beta-two`, `gamma-three`. The model then answered those three values. Usage, now parsed by the Chat Completions adapter, was two `model.completed` events with the same `prefix_hash` `8bc6c6e41b98c0783f66e322d8d026120744f7fac544207f3c73a39180ac1c76`:

- EXECUTE: 1428 input, 108 output, 0 cached, 1354 ms
- ANSWER: 1690 input, 60 output, 0 cached, 1019 ms

Totals: 3118 input tokens, 168 output tokens, 0 cached, 2 model calls, 3 tool calls. `cached_input_tokens` was present and is 0; this endpoint did not report a prefix-cache hit even though the prefix hash was stable.

Retrieve scoring uses seeded `SourceID` values. Extra hits from the current user turn (`trn_*`) and later tool-result rows that quote the same text (`inv_*`) are expected and are not treated as a cross-note miss.

- `检索`, `run_6fcf37ad5094efc558d1b01358c66d4d`, 22:36 Asia/Shanghai, `COMPLETED`, wall 11164 ms, 3119 input / 249 output / 0 cached. Before the run, an index probe for `检索` returned only `note_search` (fts) and a probe for `云计算` returned only `note_cloud` (fts). The live `retrieve_history` call for `检索` (`call_bf9da1c3a2034a83acff959c`) seeded-hit `note_search` and also the current turn; it did not return `note_cloud`.
- `云计算`, `run_69c4944901caf09bfa29bec58f66b503`, `COMPLETED`, wall 4855 ms, 3561 input / 509 output / 0 cached. The first `retrieve_history` (`call_7cbebcaf1bf042a3baf1dbd3`) seeded-hit `note_cloud` and the current turn, not `note_search`. A second call for the same query also hit a previous tool-result row that quoted `note_cloud`.
- `retrieve-path-and-id`, `run_72cd084a6f0be118b599d8de177da26f`, `COMPLETED`, wall 10975 ms, 3631 input / 484 output / 0 cached. Index probes for `server.go` and `evt_9f3a2c` were both exact hits on `evt_9f3a2c`, not `note_words`. The live calls (`call_bed694626cc741f39d3d7035` for `server.go`, `call_c47de91f19d946efb2e6cd33` for `evt_9f3a2c`) seeded-hit only `evt_9f3a2c` (exact). Extra hits were the current turn (the prompt contained both terms) and a later tool-result quote of the first response.

## Design

`GET /v1/runs/{id}/trace` and `cloud-harness trace RUN_ID` return one view: session, run, steps, tool batches, and child runs. Persistent SSE events keep their numeric id. A preview `stream.delta` is written only when `preview=1` and is not inserted into `events`.

`cloud-harness retain --days N` deletes history-index rows older than N days. Run, event, and checkpoint rows stay. `CLOUD_HARNESS_RETENTION_DAYS=0` keeps the index.

Schema scripts `001` through `003` still apply in order on open. Reopening an existing file does not replay them.

The CLI banner is `Cloud Harness M5`. Resume flags are documented before the run id because the standard flag parser stops at the first non-flag.

## Known limitations

- Chinese pre-tokenization uses overlapping bigrams, not gse.
- `runs resume` flags must come before the run id (`cloud-harness runs resume --decision deny RUN_ID`). Putting the id first makes the flag parser return `run id required`.
- cgroup RSS / `memory.max` cannot be written on the Linux hosts used so far.
- Non-Linux builds disable programmatic execution. Darwin and Windows were compile-only here.
- MCP is an adapter contract only. There is no MCP runtime transport.
- Retrieval can also hit the current turn's prompt and later tool-result quotes of the same text.
- `eval suite` correctness is the scripted checker. It is not a live-model score.
- History retention does not delete run rows.
- The 100-call budget and the no-progress window of 4 are still the uncalibrated placeholders from earlier milestones.
