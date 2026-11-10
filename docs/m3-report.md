# M3 report

Date: 2026-11-10. Host: Linux x86-64. Go: `go1.27.3 linux/amd64`.

M3 stores raw history, turn summaries, and versioned session memory, retrieves them on demand, compacts only the visible suffix, and records prompt-cache numbers against a baseline. Sub-agents, full worker execution, and MCP runtime integration are not in this milestone.

## Commands

Run from the repository root. `gofmt -l` printed nothing. The four Go commands exited 0. Output is copied from that run.

```text
$ go version
go version go1.27.3 linux/amd64

$ gofmt -l $(find . -name '*.go' -not -path './agent-tools/*' -not -path './uploads/*')

$ go build ./...
build exit:0

$ go vet ./...
vet exit:0

$ go test ./... -count=1 -timeout 180s
ok  	github.com/cloud-harness/cloud-harness/cmd/cloud-harness	0.013s
ok  	github.com/cloud-harness/cloud-harness/internal/agent	0.206s
ok  	github.com/cloud-harness/cloud-harness/internal/config	0.003s
ok  	github.com/cloud-harness/cloud-harness/internal/context	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/domain	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/eval	0.078s
ok  	github.com/cloud-harness/cloud-harness/internal/history	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/program	1.016s
ok  	github.com/cloud-harness/cloud-harness/internal/provider	0.007s
ok  	github.com/cloud-harness/cloud-harness/internal/runtime	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/security	0.003s
?   	github.com/cloud-harness/cloud-harness/internal/storage	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/storage/sqlite	0.023s
ok  	github.com/cloud-harness/cloud-harness/internal/subagent	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/tools	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/trace	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/transport	0.017s
?   	github.com/cloud-harness/cloud-harness/migrations	[no test files]
ok  	github.com/cloud-harness/cloud-harness/tests/architecture	0.002s
test exit:0

$ go test -race ./... -count=1 -timeout 180s
ok  	github.com/cloud-harness/cloud-harness/cmd/cloud-harness	1.096s
ok  	github.com/cloud-harness/cloud-harness/internal/agent	2.861s
ok  	github.com/cloud-harness/cloud-harness/internal/config	1.010s
ok  	github.com/cloud-harness/cloud-harness/internal/context	1.010s
ok  	github.com/cloud-harness/cloud-harness/internal/domain	1.012s
ok  	github.com/cloud-harness/cloud-harness/internal/eval	1.872s
ok  	github.com/cloud-harness/cloud-harness/internal/history	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/program	8.093s
ok  	github.com/cloud-harness/cloud-harness/internal/provider	1.021s
ok  	github.com/cloud-harness/cloud-harness/internal/runtime	1.010s
ok  	github.com/cloud-harness/cloud-harness/internal/security	1.010s
?   	github.com/cloud-harness/cloud-harness/internal/storage	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/storage/sqlite	1.301s
ok  	github.com/cloud-harness/cloud-harness/internal/subagent	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/tools	1.011s
ok  	github.com/cloud-harness/cloud-harness/internal/trace	1.011s
ok  	github.com/cloud-harness/cloud-harness/internal/transport	1.170s
?   	github.com/cloud-harness/cloud-harness/migrations	[no test files]
ok  	github.com/cloud-harness/cloud-harness/tests/architecture	1.014s
race exit:0
```

No data race was reported.

CLI, scripted fixture provider, no API key:

```text
$ cloud-harness eval run --task retrieve-chinese-term --provider mock
{"task":"retrieve-chinese-term","status":"COMPLETED","run_id":"run_6458e0904ce873fecc4f278779ffe2c6","pass":true,"tool_calls":[{"ordinal":0,"provider_call_id":"h1","name":"retrieve_history","arguments":{"query":"检索"},"status":"SUCCEEDED","ok":true,"result":"[{\"SourceID\":\"note_search\",...,\"MatchKind\":\"fts\"}]"}]}
exit:0

$ cloud-harness eval run --task retrieve-path-and-id --provider mock
pass true. server.go returned evt_9f3a2c as an exact match and did not return the row that only contains the words server and go. evt_9f3a2c returned that same source row.
exit:0

$ cloud-harness eval run --task planner-compare --provider mock
{"metrics":{"arms":[
  {"name":"planner_on","quality":1,"cost_tokens":65,"latency_ms":60,"wall_ms":1,"model_calls":2,"tool_calls":0,"answer":"harbor-42"},
  {"name":"planner_off_fixed","quality":0,"cost_tokens":24,"latency_ms":15,"wall_ms":1,"model_calls":1,"tool_calls":0,"answer":"unknown"},
  {"name":"planner_off_retrieve","quality":1,"cost_tokens":103,"latency_ms":75,"wall_ms":2,"model_calls":2,"tool_calls":1,"answer":"harbor-42"}
]}}
exit:0

$ cloud-harness eval run --task cache-baseline --provider mock
stable hit_rate 0.3430232558139535 (cached_input_tokens 59 / input_tokens 172)
baseline hit_rate 0 (cached_input_tokens 0 / input_tokens 172)
hit_rate_delta 0.3430232558139535
exit:0
```

The full JSON for the path task is in the command log from this session. The second id lookup also matches the previous tool result, because that result quotes `evt_9f3a2c`. The words-only row is absent.

## Planner and cache numbers

Same fact, three arms, scripted token counts and `latency_ms`. `wall_ms` is the measured mock runtime on this machine.

| Arm | Quality | Cost (input+output) | Scripted latency | Wall | Model calls | Tool calls |
| --- | --- | --- | --- | --- | --- | --- |
| Planner on | 1 | 65 | 60 ms | 1 ms | 2 | 0 |
| Planner off, fixed window | 0 | 24 | 15 ms | 1 ms | 1 | 0 |
| Planner off, retrieve | 1 | 103 | 75 ms | 2 ms | 2 | 1 |

The planner injected `deployment token is harbor-42` before the main call. The fixed window did not contain that sentence and answered `unknown`. Retrieve found it with one `retrieve_history` call and cost more than the planner.

| Cache arm | Input tokens | Cached input tokens | Hit rate |
| --- | --- | --- | --- |
| Stable prefix | 172 | 59 | 0.343 |
| Baseline (hits not reported) | 172 | 0 | 0 |

The stable arm's second call reused the unchanged system prefix. The baseline arm saw the same prefix and still reported zero cached tokens, which is the comparison point.

## Design

Raw history is append-only. Each user message, visible model text, tool result, and turn summary is indexed with the M0 tokenizer: Chinese bigrams, and exact equality for paths, filenames, and ids. Opaque payloads and signatures are not indexed and are not copied into the visible context.

`finish_turn` writes the summary, source refs, and memory delta, then marks the run completed. A plain-text answer is stored first. A summary job then runs. If that job fails, the answer stays and the job remains retryable. The next message in the session retries it. The default summarizer is local. A `Summarizer` provider can replace it. Waiting for approval or a user does not write a completed turn summary.

Session memory is a versioned document: goal, facts, decisions, progress, pending, and references. Deltas are ADD, UPDATE, DELETE, and NOOP. A later turn loads the latest version into the dynamic suffix. The static prefix stays the system text and the tool digest.

The planner runs only when forced, when the user text refers to an earlier turn or a history id, when the session already has three prior turns, or when the suffix exceeds the context budget. A short first turn skips it, so it does not consume the main model's response. Its output is a list of history ids and a precision (`summary`, `excerpt`, or `verbatim`). The main loop can still call `retrieve_history` and `retrieve_resource`.

Compaction runs only on the visible message list, and only after the estimate passes the limit (default 12000). It does not change the prefix, the current user message, or the newest tool-call pair. Older tool results keep their `tool_call_id` and are replaced with a short untrusted excerpt plus a ref. If a compaction ever changed the prefix hash, the opaque continuation would be dropped instead of replayed. Raw history is not rewritten.

Each model call records input, output, cached input, latency, and the prefix hash on the stored response and the `model.completed` event. `PrefixCache` is the meter used by `cache-baseline`: it counts a hit when the prefix hash matches the previous call. The baseline leaves that reporting off.

## Real provider run

Pending the external run. These checks use the scripted fixture provider and a prefix-hash meter. No API key is written into the repo, fixtures, traces, or this report. Authorization headers stay redacted by the provider adapter.

## Deviations

- The post-hoc summary path is real and retryable. The default implementation does not call a model. Tests cover a failing summarizer and a later successful one. The answer from the failed attempt is unchanged.
- Planner quality, cost, and latency come from scripted usage fields so the comparison does not depend on wall-clock noise. Measured `wall_ms` on this VM is 1–2 ms for every arm.
- Cache hit rate is computed from the prefix hash in this process. It is not a live provider's prompt-cache counter. The stable rate above is 59 cached tokens out of 172 input tokens (0.343). The baseline is 0.
- The context limit and the token estimate (`len/4`) are placeholders. A later eval can calibrate them.
- Indexing tool results means a later exact query can also match an earlier result that quoted the same id. The original row is still returned.

## Known limitations

- Compaction clips text. It does not ask a model to rewrite history, and it does not delete raw rows.
- `retrieve_resource` reads one id at `summary`, `excerpt`, or `verbatim`. It does not search.
- Child agents, worker script recovery, and MCP runtime calls are M4.
- The planner comparison is one fact, not the full task set in `docs/eval/tasks.json`.
