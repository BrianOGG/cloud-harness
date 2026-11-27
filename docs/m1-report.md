# M1 report

Date: 2026-11-10. Host: Linux x86-64. Go: `go1.27.3 linux/amd64` (`go 1.27.3` in `go.mod`).

M1 turns one model response into a serial tool batch. The run manager is still the only scheduler. Every real tool call goes through the broker, which checks arguments, permission, and budget before the executor runs. Resume, lease recovery, and repeat detection are not in this milestone.

## Commands

Working directory: the repository root. All four commands exited 0. `gofmt -l` on the Go tree printed nothing.

### `go build ./...`

No output.

### `go vet ./...`

No output.

### `go test ./... -count=1 -timeout 120s`

```
ok  	github.com/cloud-harness/cloud-harness/cmd/cloud-harness	0.011s
ok  	github.com/cloud-harness/cloud-harness/internal/agent	0.042s
ok  	github.com/cloud-harness/cloud-harness/internal/config	0.003s
ok  	github.com/cloud-harness/cloud-harness/internal/context	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/domain	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/eval	0.018s
ok  	github.com/cloud-harness/cloud-harness/internal/history	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/program	1.010s
ok  	github.com/cloud-harness/cloud-harness/internal/provider	0.008s
?   	github.com/cloud-harness/cloud-harness/internal/runtime	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/security	0.002s
?   	github.com/cloud-harness/cloud-harness/internal/storage	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/storage/sqlite	0.029s
ok  	github.com/cloud-harness/cloud-harness/internal/subagent	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/tools	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/trace	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/transport	0.017s
?   	github.com/cloud-harness/cloud-harness/migrations	[no test files]
ok  	github.com/cloud-harness/cloud-harness/tests/architecture	0.003s
```

### `go test -race ./... -count=1 -timeout 180s`

```
ok  	github.com/cloud-harness/cloud-harness/cmd/cloud-harness	1.075s
ok  	github.com/cloud-harness/cloud-harness/internal/agent	1.467s
ok  	github.com/cloud-harness/cloud-harness/internal/config	1.011s
ok  	github.com/cloud-harness/cloud-harness/internal/context	1.010s
ok  	github.com/cloud-harness/cloud-harness/internal/domain	1.012s
ok  	github.com/cloud-harness/cloud-harness/internal/eval	1.104s
ok  	github.com/cloud-harness/cloud-harness/internal/history	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/program	8.082s
ok  	github.com/cloud-harness/cloud-harness/internal/provider	1.021s
?   	github.com/cloud-harness/cloud-harness/internal/runtime	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/security	1.009s
?   	github.com/cloud-harness/cloud-harness/internal/storage	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/storage/sqlite	1.277s
ok  	github.com/cloud-harness/cloud-harness/internal/subagent	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/tools	1.013s
ok  	github.com/cloud-harness/cloud-harness/internal/trace	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/transport	1.141s
?   	github.com/cloud-harness/cloud-harness/migrations	[no test files]
ok  	github.com/cloud-harness/cloud-harness/tests/architecture	1.013s
```

No race reports. The program package still includes the one-second CPU-limit proof, which is slower under the race detector.

## What those tests cover

- Three `read_file` calls in one mock response run in order `c1`, `c2`, `c3`. Each result is `SUCCEEDED` and paired to that call id. The stored batch contains `c1` once (`TestThreeReadsExecuteInOrderOnce`).
- The same scenario against an `httptest` OpenAI server that streams three tool-call deltas and then a text answer: `TestBatchThreeReadsStreamingOpenAI` expects `pass: true`, status `COMPLETED`, and call ids `call_a`, `call_b`, `call_c`. A separate test parses a non-streaming body with three `tool_calls`.
- `finish_turn` mixed with `write_file` does not create the file. The run is corrected once and then completes on text (`TestMixedControlHasNoSideEffects`).
- Anthropic thinking / redacted thinking and OpenAI Responses reasoning items keep their original JSON bytes and order. Replay matches those bytes. The trace projection does not contain the secret payload (`TestAnthropicThinkingPassthroughOrder`, `TestOpenAIReasoningPassthroughOrder`).
- A cut stream has `Complete: false` and `PlanBatch` refuses it, so no tool is dispatched.
- `finish_turn` stores `turn_summary` "decided to ship" and completes with one model charge. A plain-text answer stores a generated `Turn summary:` row.
- `rm -rf .` leaves the run `WAITING` / `APPROVAL`. The sentinel file is unchanged and the invocation is `WAITING_APPROVAL` with `Dispatched` false.
- A path outside the workspace is denied. `write_file` without a grant is an approval decision and does not create the file. A granted `run_command` that is dangerous returns an error before `exec` starts.
- Provider errors that echo a bearer token are stored without the key. The eval JSON does not contain the fixture key or `Bearer`.
- Opaque continuation from a mock tool response is present on the next provider request, signature unchanged, and is absent from visible response rows and events.

## Design

The loop is: charge one model call, require the current lease, complete the provider call, persist the ordered items, split opaque bytes into `provider_continuations`, plan the batch, then either commit an answer, pause, or drive the batch serially.

Streaming chat chunks are folded into one response. Tool-call argument deltas append by index. Dispatch happens only after `finish_reason` is present. Opaque `reasoning_content` is its own item and is not copied into the visible text.

For the same provider, opaque payloads are sent back as base64 in `cloud_harness_opaque`, in item order. They are not rewritten as summaries. Anthropic and OpenAI Responses fixtures are parsed by keeping each block's original JSON as the payload. That is the passthrough contract. This process does not call those live APIs.

`read_file` is granted for the workspace root. `write_file` has no default grant, so it asks for approval. `run_command` is not a shell. The allowlist is `ls`, `pwd`, and `wc`, with arguments confined to the workspace. Anything else, including metacharacters, is an approval pause, and the executor refuses to start it even if a grant would otherwise match.

`finish_turn` commits `final_answer`, `turn_summary`, `memory_delta`, and `source_refs` in the same completion, with no extra model call. A text answer writes a local summary and a memory row so the fallback path is persisted too. There is no second model call for that summary.

Trace events record `model.completed` without opaque bytes, then `tool.batch` with call id, name, status, and ok. The HTTP trace still returns visible items, batches, and events.

## Real provider run

Pending the external run. No API key is available in this environment, and none is written into the repo, fixtures, traces, or this report.

On a machine that has a key:

```bash
export OPENAI_API_KEY='…'
export OPENAI_BASE_URL='https://apihub.agnes-ai.com/v1'
export OPENAI_MODEL='agnes-2.5-flash'
cloud-harness eval run --task batch-three-reads --provider openai
```

The command creates `alpha.txt`, `beta.txt`, and `gamma.txt`, runs the loop, and prints JSON with `status`, `run_id`, `tool_calls` in order, and `pass`. Paste that JSON here after the external run. Do not paste the key.

## Deviations

- Official OpenAI and Anthropic SDKs are still not imported. Chat Completions, including streaming tool calls, uses `net/http`. Domain types stay free of HTTP and SDK structs.
- Opaque replay on the chat-completions wire is a base64 side field. A strict endpoint that rejects unknown JSON fields will not see opaque items, because the field is omitted when there are none. The eval task does not send it. Byte-exact passthrough of thinking and reasoning blocks is tested on fixture parsers plus domain `Replay`.
- Plain-text summaries are generated locally from the answer text. A second lightweight model call is not made. `finish_turn` still stores the model's own summary.
- Dangerous commands pause the run. Approving them and continuing the batch is M2, so this milestone never starts the process.
- Only `batch-three-reads` has a fixture and a scorer. The other tasks in `docs/eval/tasks.json` remain unexecuted.

## Known limitations

- `cloud-harness runs resume` still returns `not_implemented` and does not create a new run. A `WAITING` run stays that run.
- Repeat detection, checkpoint restore, and unknown-result reconciliation are not implemented. A batch that reaches `UNKNOWN` is marked `WAITING(RECONCILIATION)` and is not replayed.
- In M1, `write_file` without a pre-issued scoped grant waited for approval and could not finish. Current versions use the session approval mode.
- `run_command` does not invoke a shell: pipes, redirects, and shell operators are unsupported. Only `ls` / `pwd` / `wc` with workspace-contained path arguments are safe. Current versions apply the session approval mode to dangerous commands; the M1 milestone paused them for approval.
- The real Agnes tool-calling eval has not been run here.
