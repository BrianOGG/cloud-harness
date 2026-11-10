# Run and invocation transition tables

The tables below are the ones `internal/domain` enforces. `TransitionRun` and `TransitionInvocation` reject every pair that is not listed. A resume keeps the same run id.

## Run

| Current | Event | Next |
| --- | --- | --- |
| RUNNING | ask_user | WAITING (USER) |
| RUNNING | approval | WAITING (APPROVAL) |
| RUNNING | wait_child | WAITING (CHILD) |
| RUNNING | budget_exhausted | WAITING (BUDGET) |
| RUNNING | needs_reconciliation | WAITING (RECONCILIATION) |
| WAITING (any reason) | resume | RUNNING |
| RUNNING | final_answer | COMPLETED |
| RUNNING | fatal | FAILED |
| WAITING (any reason) | fatal | FAILED |
| RUNNING | cancel | CANCELLED |
| WAITING (any reason) | cancel | CANCELLED |

WAITING without a reason is corrupt. RUNNING, COMPLETED, FAILED, and CANCELLED must not carry a reason. COMPLETED, FAILED, and CANCELLED have no outbound edge. Cancel of an already CANCELLED run is an idempotent API success and does not call the table again.

A final answer is only legal from RUNNING. A waiting run has to resume first. Resume keeps the same run id.

## Invocation

| Current | Event | Next |
| --- | --- | --- |
| PENDING | start | RUNNING |
| PENDING | request_approval | WAITING_APPROVAL |
| PENDING | deny | DENIED |
| PENDING | skip | SKIPPED |
| PENDING | cancel | CANCELLED |
| PENDING | protocol_reject | FAILED |
| WAITING_APPROVAL | approve | RUNNING |
| WAITING_APPROVAL | deny | DENIED |
| WAITING_APPROVAL | cancel | CANCELLED |
| RUNNING | succeed | SUCCEEDED |
| RUNNING | fail | FAILED |
| RUNNING | mark_unknown | UNKNOWN |
| RUNNING | cancel | CANCELLED |
| UNKNOWN | reconcile_success | SUCCEEDED |
| UNKNOWN | reconcile_fail | FAILED |
| UNKNOWN | cancel | CANCELLED |

SUCCEEDED, FAILED, DENIED, SKIPPED, and CANCELLED are terminal. UNKNOWN is not. There is no edge from UNKNOWN back to RUNNING, so an unknown side effect is not replayed.

`protocol_reject` is the extra edge required by the batch rule that a malformed response still produces one paired error per call id and must not enter RUNNING. `Dispatched` stays false. It is not an execution failure.

## Batch rules covered by the mock contract tests

- Three ordinary calls run in provider order. Each call id gets one result.
- `finish_turn` or `ask_user` mixed with any other call refuses the whole batch. Nothing is dispatched.
- `finish_turn` alone is confirmed in place and is not an external dispatch.
- Approval pauses the blocked call and leaves later calls PENDING. A denial marks that call DENIED and later pending calls SKIPPED.
- After a crash, RUNNING becomes UNKNOWN. SUCCEEDED calls are not dispatched again. Drive will not continue past UNKNOWN.

## Continuation rules

- Replay keeps item order and the opaque payload and signature bytes.
- Trace and the history index see text only. Opaque bytes live in `provider_continuations`.
- A different provider does not receive those bytes. The M0 chat-completions client returns `opaque_replay_unsupported` rather than dropping them.
