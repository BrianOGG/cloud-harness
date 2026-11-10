# M4 report

Date: 2026-11-10. Host: Linux x86-64. Go: `go1.27.3 linux/amd64`.

M4 adds polling, pre-authorized Starlark scripts, real child runs, and a ToolSpec to MCP tool mapping. Full MCP sessions and M5 are not in this milestone.

## Commands

Run from the repository root. `gofmt -l .` printed nothing. The Go commands exited 0. No data race was reported. Output is copied from that run.

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
ok  	github.com/cloud-harness/cloud-harness/cmd/cloud-harness	0.013s
ok  	github.com/cloud-harness/cloud-harness/internal/agent	0.223s
ok  	github.com/cloud-harness/cloud-harness/internal/config	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/context	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/domain	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/eval	0.076s
ok  	github.com/cloud-harness/cloud-harness/internal/history	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/program	1.023s
ok  	github.com/cloud-harness/cloud-harness/internal/provider	0.007s
ok  	github.com/cloud-harness/cloud-harness/internal/runtime	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/security	0.002s
?   	github.com/cloud-harness/cloud-harness/internal/storage	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/storage/sqlite	0.025s
ok  	github.com/cloud-harness/cloud-harness/internal/subagent	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/tools	0.003s
ok  	github.com/cloud-harness/cloud-harness/internal/trace	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/transport	0.018s
?   	github.com/cloud-harness/cloud-harness/migrations	[no test files]
ok  	github.com/cloud-harness/cloud-harness/tests/architecture	0.003s
test exit:0

$ go test -race ./... -count=1 -timeout 180s
ok  	github.com/cloud-harness/cloud-harness/cmd/cloud-harness	1.094s
ok  	github.com/cloud-harness/cloud-harness/internal/agent	4.338s
ok  	github.com/cloud-harness/cloud-harness/internal/config	1.010s
ok  	github.com/cloud-harness/cloud-harness/internal/context	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/domain	1.012s
ok  	github.com/cloud-harness/cloud-harness/internal/eval	2.023s
ok  	github.com/cloud-harness/cloud-harness/internal/history	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/program	9.102s
ok  	github.com/cloud-harness/cloud-harness/internal/provider	1.023s
ok  	github.com/cloud-harness/cloud-harness/internal/runtime	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/security	1.010s
?   	github.com/cloud-harness/cloud-harness/internal/storage	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/storage/sqlite	1.308s
ok  	github.com/cloud-harness/cloud-harness/internal/subagent	1.011s
ok  	github.com/cloud-harness/cloud-harness/internal/tools	1.012s
ok  	github.com/cloud-harness/cloud-harness/internal/trace	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/transport	1.212s
?   	github.com/cloud-harness/cloud-harness/migrations	[no test files]
ok  	github.com/cloud-harness/cloud-harness/tests/architecture	1.013s
race exit:0
```

CLI, no API key:

```text
$ cloud-harness eval run --task incident-wait-for-ready --provider mock
{"task":"incident-wait-for-ready","status":"COMPLETED","run_id":"run_8fa0f262daee4e4e8b188a8298c54e82","pass":true,
 "tool_calls":[
   {"provider_call_id":"w1","name":"poll_tool","status":"SUCCEEDED","result":"{\"stopped\":\"condition\",\"attempts\":1,\"last\":{\"content\":\"ready\",\"path\":\"ready.txt\"}}"},
   {"provider_call_id":"w2","name":"poll_tool","status":"SUCCEEDED","result":"{\"stopped\":\"cap\",\"attempts\":3,\"last\":{\"content\":\"down\",\"path\":\"down.txt\"}}"}
 ]}
poll exit:0
```

`ready.txt` already said `ready`, so the first poll stopped on the condition at attempt 1. `down.txt` never matched, so the second poll stopped at the declared cap of 3. `poll_tool` is not an ordinary repeat, and the internal reads are not a second batch.

Script and child checks are `go test` cases in `internal/agent` and `internal/program`:

- `TestScriptRefusesSideEffectsAndNewPermissions`: `write_file` without `confirm_side_effects` returns `script_policy` and does not start the interpreter. A running script that calls `write_file` outside its allowlist gets `auth_denied`, does not create the file, and does not leave the run `WAITING`.
- `TestScriptCannotTouchTheSystem`: `open('/etc/passwd')` is a script error. The parent process does not interpret it (`starlarkEntries` stays 0).
- `TestSafeReplayRequiresReadonlyIdempotentTools`, `TestReplayRunsASafeCrashOnceMore`, `TestReplayDoesNotRepeatAnUnsafeCrash`: a crashed script runs once more only when every declared tool is readonly, idempotent, and side-effect `none`.
- `TestChildHasItsOwnTraceBudgetAndContext`: the child has its own run id, events, and checkpoint. The checkpoint does not contain the parent secret. Parent model calls are the child's calls plus the parent's own calls. Parent tool calls are the child's tool calls plus the `delegate_task` call.
- `TestParentCancelReachesChild`: cancelling the parent unblocks the child tool and both runs end `CANCELLED`.
- `TestMCPAdapterMapsSpecAndDeniedCall`: a spec maps to an MCP tool descriptor. A denial becomes `isError` with code `auth_denied`. `Discover` does not dial a server.

## Host isolation

```text
$ go test ./internal/program/ -count=1 -run TestWorkerBrokerSyntaxAndLimits -v
cgroup_memory=false detail=memory.max: open .../memory.max: permission denied
--- PASS: TestWorkerBrokerSyntaxAndLimits (0.01s)
probe exit:0
```

`memory.max` is not writable in this pod. The worker still runs only after a separate process, `RLIMIT_AS` 8 GiB, `RLIMIT_CPU` 5s, `RLIMIT_FSIZE` 0, `RLIMIT_NOFILE` 128, and a parent check of `/proc/<pid>/limits`. If that probe fails, `program.Execute` returns `program_disabled` and does not interpret the script in the harness process. Non-Linux builds stay disabled.

## Design

`poll_tool` calls the broker once per attempt. The target must be registered as pollable, readonly, and side-effect `none`. The stop field is compared to the JSON result. The call ends on a match, on the attempt cap, or when the context is cancelled.

`execute_program` checks the allowlist before the worker starts. A side-effect tool (`write_file`, `run_command`, or any non-readonly tool) is refused unless it is named in `confirm_side_effects` and a grant already exists. The running script can call only that allowlist. Anything else is `auth_denied`. A need for approval is a `script_policy` error, not `WAITING(APPROVAL)`. The worker has no `open`, no `load`, and no network. Replay uses `program.Replay` and only when `SafeReplay` is true.

`delegate_task` creates a child run with `parent_run_id` set. The parent row moves to `WAITING(CHILD)` for the duration of the child and returns to `RUNNING` when the child finishes. The child has its own events and a checkpoint taken before its loop. Its messages are the goal and the relevant context, not the parent transcript. A child cannot delegate. Charges against a child run update the child and the parent in one transaction, so the parent's used counts include the child. Cancelling the parent cancels the context the child tool is using.

`StaticMCP` and `ToMCPTool` map a local spec to an MCP tool name, description, and input schema. `MCPErrorResult` maps a broker denial. Nothing in that path opens a socket.

Chat Completions responses now keep `prompt_tokens`, `completion_tokens`, and `prompt_tokens_details.cached_tokens` on a usage item. Streaming requests ask for usage with `stream_options.include_usage`.

## Real provider run

Pending the external run. These checks use the mock provider and local fixtures. No API key is written into the repo, fixtures, traces, or this report.

## Host isolation re-run

Pending a host where `memory.max` can be written, and pending non-Linux machines. On this Linux host the cgroup write is permission denied, so the enabled path is the verified rlimit set above. A failed probe still disables programmatic execution instead of interpreting the script in-process.

## Deviations

- Child work runs inside the parent's `delegate_task` call. The parent is `WAITING(CHILD)` while that call is in progress and resumes when the child returns. Children are not scheduled as a separate process.
- A live worker crash is not induced here. The replay decision is tested with a fake attempt that returns `worker_crashed`. An unsafe crash is not run a second time.
- `run_command` requires the same confirmation as `write_file` before a script may name it, even though its spec is marked readonly for the allowlisted commands.

## Known limitations

- MCP mapping is an in-process contract. There is no MCP transport.
- Programmatic execution stays off when the worker cannot prove its limits, including every non-Linux OS until the same checks exist there.
- The poll cap inside one call is 20 attempts.
- M5 is not started.
