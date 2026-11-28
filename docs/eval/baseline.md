# Comparing evaluation runs

The full suite contains 15 fixed tasks. The default CLI run uses the scripted mock. The live suite uses 10 of the same tasks and requires a configured model. [Task catalog](tasks.json) · [Live commands and historical data](live-suite.md)

## Run a scripted pair

Use a fresh workspace and database for each run. Keep the runtime configuration, workspace layout and build identical when measuring repeatability.

```sh
go build -o /tmp/cloud-harness ./cmd/cloud-harness
d=$(mktemp -d)
/tmp/cloud-harness eval suite --provider mock --workspace "$d/a/work" --db "$d/a/runs.db" --out "$d/a.json" > /dev/null
/tmp/cloud-harness eval suite --provider mock --workspace "$d/b/work" --db "$d/b/runs.db" --out "$d/b.json" --compare "$d/a.json" > "$d/b-compared.json"
/tmp/cloud-harness eval compare "$d/a.json" "$d/b.json" | jq '.metrics'
```

`--out` writes a standalone run report. With `--compare`, stdout also includes `comparison`. The offline command compares two saved reports without invoking a provider. Both commands flag tasks that changed from pass to fail. The offline command also exits nonzero for incomparable reports. The existing suite command retains its pass/fail regression exit behavior for historical baselines and added or removed tasks.

## Reading the measures

- **Success rate** is the fraction of all selected tasks that passed their checker. Each task counts once, including the multi-arm planner and cache tasks. A setup or provider failure still counts in the denominator.
- **Tool choice accuracy** gives each task one vote. Its rubric is recorded under `task_metrics[task].expected_tools`. The model must choose at least one name from every `required_any` group. Every chosen name must occur in a required group or in `optional`. For the Go fix task, reading the file and choosing either editor is correct, and running the test command is optional.
- **Chosen tools** are the distinct model-requested names across the task's recorded responses, including all arms. A denied or skipped call still represents a choice. Order, repetitions, arguments and the terminal `finish_turn` action do not affect this measure. Arguments, file changes and final answers belong to the outcome checker. With a complete trace, an empty choice set scores false on a task requiring tools. An incomplete trace has a null score. Suite accuracy is null if a task cannot be scored. The report retains the correct and scored counts.
- **Tokens** in `measurements` and `task_metrics` sum recorded model calls across every arm. `total_tokens` is input plus output, including cached input only once. Input and output each require a known count on every call. The normalized trace stores an integer for each counter, so an absent counter and a zero both decode to zero. These ambiguous counters yield null in the measurements. `token_complete_tasks` counts tasks with both totals. For compatibility, the older top-level token fields still sum the available counters, which can represent partial usage.
- **Latency** is measured wall time in milliseconds from fixture setup through scoring, including tool execution and local test commands. Suite latency sums these sequential task durations. Elapsed time varies with machine load and cache warmth.
- **Comparability** requires the same benchmark version, scripted/live mode and task IDs. Provider and configured model are recorded so a comparison of two live models can identify both. The command cannot verify external gateway settings. Deltas are current minus baseline. Missing operands yield null. `paired_total_tokens` compares only the named tasks with complete usage on both sides. With a legacy report or different task sets, the task transitions remain available and `metrics.reason` explains the missing metric deltas.

## Scripted measurements

Two runs of `capabilities-v1` in fresh workspaces and databases produced the following results. The first report is checked in. [Scripted baseline](baseline.json)

| Measure | Baseline | Current | Delta |
| --- | ---: | ---: | ---: |
| Successful tasks | 15/15 | 15/15 | 0 |
| Correct tool choices | 15/15 | 15/15 | 0 |
| End-to-end latency | 674 ms | 680 ms | +6 ms |
| Full-suite tokens | unknown | unknown | unknown |
| Tokens on 2 fully metered tasks | 930 | 930 | 0 |

The planner arms supplied 192 scripted tokens. The cache probe supplied 738 tokens from its prefix estimator across both arms. The other 13 task scripts omitted usage. The two added tasks exercise different local release values under frontend and backend project rules, and selection of the payments queue depth from an in-process catalog of 8 services × 8 metrics. The catalog uses the runtime's MCP naming and broker dispatch. Stdio transport is outside this fixture.
