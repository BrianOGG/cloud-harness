# M2 report

Date: 2026-11-10. Host: Linux x86-64. Go: `go1.27.3 linux/amd64`.

M2 makes a waiting run resumable in place, records tool transitions with their events, and refuses to replay a side effect whose outcome is unknown. Context compression, cache metrics, and child agents are not in this milestone. `WAITING(CHILD)` is only the state boundary.

## Commands

Run from the repository root. Output below is copied from the final verification on this tree.

```text
$ go version
go version go1.27.3 linux/amd64

$ gofmt -l $(find . -name '*.go' -not -path './agent-tools/*' -not -path './uploads/*')
```

`gofmt -l` printed nothing.

```text
$ go build ./...
build exit:0

$ go vet ./...
vet exit:0

$ go test ./... -count=1 -timeout 180s
ok  	github.com/cloud-harness/cloud-harness/cmd/cloud-harness	0.013s
ok  	github.com/cloud-harness/cloud-harness/internal/agent	0.151s
ok  	github.com/cloud-harness/cloud-harness/internal/config	0.003s
ok  	github.com/cloud-harness/cloud-harness/internal/context	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/domain	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/eval	0.030s
ok  	github.com/cloud-harness/cloud-harness/internal/history	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/program	1.013s
ok  	github.com/cloud-harness/cloud-harness/internal/provider	0.008s
ok  	github.com/cloud-harness/cloud-harness/internal/runtime	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/security	0.002s
?   	github.com/cloud-harness/cloud-harness/internal/storage	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/storage/sqlite	0.024s
ok  	github.com/cloud-harness/cloud-harness/internal/subagent	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/tools	0.002s
ok  	github.com/cloud-harness/cloud-harness/internal/trace	0.001s
ok  	github.com/cloud-harness/cloud-harness/internal/transport	0.017s
?   	github.com/cloud-harness/cloud-harness/migrations	[no test files]
ok  	github.com/cloud-harness/cloud-harness/tests/architecture	0.002s
test exit:0

$ go test -race ./... -count=1 -timeout 180s
ok  	github.com/cloud-harness/cloud-harness/cmd/cloud-harness	1.084s
ok  	github.com/cloud-harness/cloud-harness/internal/agent	2.255s
ok  	github.com/cloud-harness/cloud-harness/internal/config	1.010s
ok  	github.com/cloud-harness/cloud-harness/internal/context	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/domain	1.012s
ok  	github.com/cloud-harness/cloud-harness/internal/eval	1.293s
ok  	github.com/cloud-harness/cloud-harness/internal/history	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/program	8.092s
ok  	github.com/cloud-harness/cloud-harness/internal/provider	1.022s
ok  	github.com/cloud-harness/cloud-harness/internal/runtime	1.012s
ok  	github.com/cloud-harness/cloud-harness/internal/security	1.010s
?   	github.com/cloud-harness/cloud-harness/internal/storage	[no test files]
ok  	github.com/cloud-harness/cloud-harness/internal/storage/sqlite	1.331s
ok  	github.com/cloud-harness/cloud-harness/internal/subagent	1.010s
ok  	github.com/cloud-harness/cloud-harness/internal/tools	1.010s
ok  	github.com/cloud-harness/cloud-harness/internal/trace	1.009s
ok  	github.com/cloud-harness/cloud-harness/internal/transport	1.157s
?   	github.com/cloud-harness/cloud-harness/migrations	[no test files]
ok  	github.com/cloud-harness/cloud-harness/tests/architecture	1.013s
race exit:0
```

No data race was reported.

CLI, scripted fixture provider, no API key:

```text
$ cloud-harness eval run --task batch-approval-skip --provider mock --db "$tmpdir/approval.db" --workspace "$tmpdir/approval"
{"task":"batch-approval-skip","status":"COMPLETED","run_id":"run_781f8d96fdaff3dc66712d6c395202fc","pass":true,"tool_calls":[{"ordinal":0,"provider_call_id":"c1","name":"read_file","arguments":{"path":"alpha.txt"},"status":"SUCCEEDED","ok":true},{"ordinal":1,"provider_call_id":"c2","name":"write_file","arguments":{"path":"out.txt","content":"nope"},"status":"DENIED","ok":false},{"ordinal":2,"provider_call_id":"c3","name":"read_file","arguments":{"path":"beta.txt"},"status":"SKIPPED","ok":false}]}
approval exit:0

$ cloud-harness eval run --task recover-mid-batch --provider mock --db "$tmpdir/recover.db" --workspace "$tmpdir/recover"
{"task":"recover-mid-batch","status":"WAITING","run_id":"run_9d998dd05e477c624d9e6f3f838fa0ae","wait_reason":"RECONCILIATION","pass":true,"tool_calls":[{"ordinal":0,"provider_call_id":"c1","name":"read_file","arguments":{"path":"alpha.txt"},"status":"SUCCEEDED","ok":true},{"ordinal":1,"provider_call_id":"c2","name":"write_file","arguments":{"path":"out.txt","content":"x"},"status":"UNKNOWN","ok":false},{"ordinal":2,"provider_call_id":"c3","name":"read_file","arguments":{"path":"gamma.txt"},"status":"PENDING","ok":false}]}
recover exit:0
```

## Behavior covered by the tests

- `TestApprovalDenyPairsTheWholeBatch`: a batch of read, write, read pauses on the write. `Resume` with `deny` keeps the same run id, leaves the write file absent, and stores `SUCCEEDED`, `DENIED`, `SKIPPED`. The follow-up model request contains three tool messages, each marked `"untrusted":true`.
- `TestCrashRecoveryDoesNotReplaySideEffects`: the first `read_file` is `SUCCEEDED`. The in-flight `write_file` is committed as `RUNNING` and then the process path stops without calling the tool. `Recover` turns that row into `UNKNOWN`, does not write the file, does not run the third call, and sets the same run to `WAITING(RECONCILIATION)`.
- `TestLiveLeaseCannotBeStolen`: a second owner cannot resume while a lease is live. After the lease time passes, that owner can take the same run.
- `TestRepeatReminderAndSensitiveEgress`: three identical reads inject `RepeatReminder` into the next model input and do not copy tool text into the system message. Reading `secrets.txt` then `run_command` does not run the command.
- `TestCancelPropagatesToTool`: cancel closes the tool context. The run ends `CANCELLED`.
- `TestChildWaitKeepsTheSameRun`: `WaitForChild` sets `WAITING(CHILD)` and resume with `child_done` continues that run. No second run is created. No child agent is started.
- `TestIdempotentWriteDoesNotRunTwice`: the same idempotency key returns the stored result and does not write again.
- `TestWriteProbeConfirmsWithoutWriting` and `TestProbeConfirmsWriteWithoutReplaying`: `write_file` status probe reads the file and does not create or change it. A matching file is recorded as `SUCCEEDED` with `"probed":true`. A missing or different file stays `UNKNOWN`. `run_command` has no probe.
- `TestApprovalSkipAndCrashRecovery` runs `cloud-harness eval run`’s library path for `batch-approval-skip` and `recover-mid-batch` with the scripted fixture provider. Both pass.

Local commands, no API key:

```bash
cloud-harness eval run --task batch-approval-skip
cloud-harness eval run --task recover-mid-batch
cloud-harness runs resume RUN_ID --decision deny
cloud-harness runs resume RUN_ID --decision approve
cloud-harness runs resume RUN_ID --answer "the user's reply"
cloud-harness runs resume RUN_ID --extra-model-calls 10
cloud-harness runs resume RUN_ID --decision reconcile_success
cloud-harness runs resume RUN_ID --decision child_done
```

`POST /v1/runs/{id}/responses` accepts `decision`, `answer`, and `extra_model_calls` and resumes that id. A budget wait without extra calls stays the same waiting run.

## Design

Each tool dispatch writes `RUNNING` and an invocation event in one transaction before the executor is entered. A confirmed result writes the terminal status in a second transaction with its event. Run status changes go through `CommitRunTransition`, which updates the run row and appends the run event in one transaction. Checkpoints store the message list, continuation items, repeat observations, and the sensitive-read flag. A checkpoint is not a second source of truth. The invocation rows are.

`Resume` acquires the lease, moves `WAITING` to `RUNNING` on the same id, applies the decision, and continues. Deny marks the waiting call `DENIED` and later pending calls `SKIPPED`, then returns every call id to the model. Approve continues from the blocked call. Reconcile moves `UNKNOWN` to a terminal status without calling the tool. An expired lease can be acquired. A live lease cannot.

`UNKNOWN` is not a success and is not started again when the tool can write or run a command. Readonly calls that already `SUCCEEDED` are left as they are.

The repeat reminder uses the confirmed threshold of 3. The no-progress block uses a window of 4 identical result hashes, or an A→B→A→B window with stable hashes. Both numbers live next to each other in `domain` so a later eval can change the window without inventing a second constant. The block returns `no_progress` and does not call the executor.

Tool results sent back to the model are wrapped as `{"untrusted":true,...}`. They stay on the tool role. After a sensitive read (`.env`, `secrets.txt`, `credentials.json`, `id_rsa`, or a name containing `secret` or `credential`), `run_command` and `http_fetch` are denied unless a grant has op `egress`.

Cancellation stored on the manager cancels the run context, so a blocked tool returns. The run row becomes `CANCELLED`.

## Real provider run

Pending the external run. These checks do not need a model key. The scripted fixture provider drives `batch-approval-skip` and `recover-mid-batch`. No API key is written into the repo, fixtures, traces, or this report. Authorization headers are still scrubbed by the provider adapter from M1.

## Deviations

- Child agents are not started. `WaitForChild` only parks the parent in `WAITING(CHILD)`, and `child_done` resumes that same parent.
- Plain-text summaries stay local. This milestone does not add a second model call for them.
- The no-progress window is 4, a placeholder. It is not calibrated against the eval set.
- Idempotency is a result cache keyed by run, tool, and key. A repeat of the same key returns the cached result and skips the executor. The `write_file` status probe only confirms an existing byte-for-byte match. It never writes. A miss stays `UNKNOWN` and is not replayed. `run_command` has no probe.
- `batch-approval-skip` and `recover-mid-batch` always use the scripted fixture provider so the runtime check is deterministic. `--provider openai` does not send those scenarios to a live model.

## Known limitations

- Approving a `WAITING(APPROVAL)` call continues it. There is still no human UI. The decision is the resume flag or the HTTP body.
- A crashed side-effect call stays `UNKNOWN` until `reconcile_success` or `reconcile_fail`. Those decisions do not run the tool.
- Repeat detection counts ordinary tools only. `finish_turn`, `ask_user`, and `update_plan` are excluded.
- Lease heartbeat extends the same lease version. It does not recover a run whose process died without a waiting reason until `Recover` is called, which the `recover-mid-batch` task does.
- Context compression, retrieval quality, and prompt-cache hit rate are M3.
