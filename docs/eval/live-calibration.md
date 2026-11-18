# Live Model Eval Baseline & Parameter Calibration (M8-3)

This document records the live-model baseline measurement across all 13 tasks in [`docs/eval/tasks.json`](tasks.json), the root-cause breakdown of failures, the harness live-mode authorization fix, and the empirical calibration review for runtime budget and context parameters.

---

## 1. Live Model Measurement Summary

- **Model**: `deepseek-v4.1-flash-free` (via OpenAI-compatible test gateway)
- **Scope**: All 13 evaluation tasks executed individually (`cloud-harness eval run --provider openai --task <task>`)
- **Initial Outcome (before M8-3 harness grant fix)**: **2 passed / 11 failed** (across 16 total runs including multi-arm tasks `planner-compare` and `cache-baseline`).

### Failure Classification

1. **Harness Live-Mode Authorization Gap (1 task — fixed in M8-3)**
   - **Affected task**: `dev-fix-failing-test`
   - **Root cause**: The live model read `calc.go` and `calc_test.go`, fixed `Add` in `calc.go` (`return a - b` -> `return a + b`), and then invoked `run_command` (`go test`) to verify the fix before answering. In `internal/eval/run.go`, pre-grants (`mgr.Grants`) only covered `write_file` and `edit_file` on `dev-fix-failing-test`, and approval `Resume` loops only existed for `batch-approval-skip` and `file-safe-edit`. Because scripted fixtures avoid unapproved gated commands by construction, the live model's spontaneous `run_command` verification left the run parked in `WAITING` (`WaitReason=APPROVAL`), causing the scorer to fail with `"run status is WAITING"`.
   - **Resolution**: In `internal/eval/run.go`, when running in live mode (`shouldUseScripted(opt) == false`) on non-restricted tasks (excluding the three authorization/recovery semantic scenarios `batch-approval-skip`, `file-safe-edit`, and `recover-mid-batch`), `eval.Run` pre-populates active workspace-scoped (`ResourceScope: abs`) grants for standard local tools (including `run_command`, `write_file`, `edit_file`) and automatically approves pending `WAITING_APPROVAL` command gates via `mgr.Resume(..., Decision: "approve")`. The three semantic exception scenarios retain their exact restricted grants and `deny`/crash-recovery semantics.

2. **Gateway Response Header Timeout (5 failed runs + `cache-baseline` baseline arm — external gateway queueing, no code change)**
   - **Symptom**: 5 runs terminated in `FAILED` with `provider_http`: `"response header timeout exceeded"` when upstream gateway queueing exceeded the HTTP response header timeout; the `baseline` arm of `cache-baseline` similarly received an empty/timed-out response.
   - **Disposition**: Confirmed as external test-gateway queueing latency rather than a harness protocol or state-machine defect. Recorded here for baseline traceability; no code change required.

3. **Script-Shaped Scorer Expectations (4 tasks — known scenario design limitation, out of scope for M8-3)**
   - **Affected tasks**:
     - `batch-approval-skip`: Scorer expects a single 3-call batch (`read_file` -> `write_file` -> `read_file`) with scripted call IDs (`c1`, `c2`, `c3`) so that denying the second call leaves the third `SKIPPED`. A live model typically issues tool calls step-by-step across turns or uses different call IDs.
     - `file-safe-edit`: Prompt instructs the model to update `allowed/config.conf` and refuse modifying `forbidden/config.conf`. A compliant live model refuses in natural language without attempting the forbidden `write_file`/`edit_file` tool call, whereas `scoreFileSafeEdit` requires an actual tool call against `forbidden/` that fails with `auth_denied`.
     - `retrieve-path-and-id`: Scorer expects exactly two separate `retrieve_history` calls in a fixed order (`server.go` then `evt_9f3a2c`), whereas a live model may combine the query or issue a different number of searches.
     - `planner-compare`: Scorer compares token cost and tool counts across three arms (`planner_on`, `planner_off_fixed`, `planner_off_retrieve`) assuming deterministic single-call retrieval and planner JSON output; live model token/tool variance across arms violates the strict scripted inequalities.
   - **Disposition**: Scorer logic is intentionally kept unchanged in M8-3 to preserve deterministic regression semantics.

---

## 2. Observed Resource & Call Distribution

Across the 16 live runs executed during the baseline pass:

| Metric | Observed Live Value | Configured Default Limit |
| :--- | :--- | :--- |
| `used_model_calls` per run | **1 – 5** (max `5`) | `DefaultMaxModelCalls = 100` (`internal/domain/budget.go`) |
| Runs failing due to `no_progress` | **0** | `DefaultNoProgressLimit = 4` (`internal/domain/budget.go`) |
| Runs failing due to context limit / compaction | **0** | `defaultContextTokens = 12000` (`internal/agent/memory.go`) |
| Single-task input tokens (`cache-baseline` stable arm) | **~5,968 tokens** | `defaultContextTokens = 12000` (`internal/agent/memory.go`) |

---

## 3. Parameter Calibration Conclusions

After reviewing the live-model empirical data against the three placeholder constants from earlier milestones, **all three constants are retained at their current values without code changes**:

1. **`DefaultMaxModelCalls = 100` (`internal/domain/budget.go`) — Keep unchanged**
   - **Rationale**:
     - *Safety margin vs. empirical usage*: Observed single-task model calls ranged from `1` to `5` (peak `5` calls). A hard ceiling of `100` provides a 20x safety margin for multi-step debugging, test-fix-retest loops, and delegated runs without prematurely terminating legitimate long-horizon turns, while still bounding runaway loops.
     - *No budget failures observed*: Zero live runs failed or paused due to model-call budget exhaustion.
     - *Follow-up plan*: Re-evaluate only if future multi-turn or subagent benchmarks tracked via `eval suite --compare` show sustained high-percentile usage approaching the cap or require tighter cost bounds.

2. **`DefaultNoProgressLimit = 4` (`internal/domain/budget.go`) — Keep unchanged**
   - **Rationale**:
     - *Safety margin vs. empirical usage*: Along with `DefaultRepeatWarning = 3` (which injects a repeat reminder after 3 identical ordinary calls), blocking identical/alternating no-progress tool calls at window length `4` stops infinite tool loops within 4 wasted dispatches while allowing normal read-edit-test cycles where file state changes between calls.
     - *No false positives observed*: Zero live runs triggered false-positive `no_progress` aborts during normal exploration and verification.
     - *Follow-up plan*: Continue monitoring `repeated_tool_calls` and `no_progress` occurrences across future model comparisons using `eval suite --compare`.

3. **`defaultContextTokens = 12000` (`internal/agent/memory.go`) — Keep unchanged**
   - **Rationale**:
     - *Safety margin vs. empirical usage*: The measured single-task input footprint in `cache-baseline` (stable prefix arm, including system prompt, tool schemas, and turn context) is approximately `5,968` tokens—roughly 50% of the `12,000`-token threshold. Keeping `defaultContextTokens = 12000` leaves ~6,000 tokens of headroom for multi-step tool outputs before planner activation or context compaction kicks in, while still compacting well before small/medium model context windows saturate.
     - *No context-limit failures observed*: Zero live runs failed due to context overflow or premature compaction loss.
     - *Follow-up plan*: Revisit with `--out` / `--compare` regression diffs once longer multi-turn sessions are benchmarked across additional live providers.

---

## 4. Baseline Persistence & Regression Comparison Workflow

- **Deterministic scripted baseline**: Stored at [`docs/eval/baseline.json`](baseline.json), generated via:
  ```bash
  cloud-harness eval suite --out docs/eval/baseline.json
  ```
- **Regression check against baseline**:
  ```bash
  cloud-harness eval suite --compare docs/eval/baseline.json
  ```
  Exits `0` when no task regresses from `pass` (`true`) to `fail` (`false`) (even if tasks are fixed or unchanged), and exits non-zero (`1`) with a per-task diff summary whenever any baseline-passing task fails.
