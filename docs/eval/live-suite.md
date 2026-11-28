# Real-model evaluation

`eval suite --live` runs the 10 tasks listed in `live_tasks` using the selected real provider. Approval skipping, recovery, polling and the planner/cache probes remain in the full suite. [Task catalog](tasks.json)

Supply `OPENAI_API_KEY` through the environment. For an OpenAI-compatible service, set its API URL and a tool-capable model ID. Use the same endpoint, model and runtime configuration for both runs when comparing harness revisions.

```sh
export OPENAI_BASE_URL=https://api.openai.com/v1
export OPENAI_MODEL=gpt-4.1-mini
go build -o /tmp/cloud-harness ./cmd/cloud-harness
d=$(mktemp -d)
/tmp/cloud-harness eval suite --live --provider openai --db "$d/a/runs.db" --workspace "$d/a/work" --out "$d/a.json"
/tmp/cloud-harness eval suite --live --provider openai --db "$d/b/runs.db" --workspace "$d/b/work" --out "$d/b.json"
/tmp/cloud-harness eval compare "$d/a.json" "$d/b.json" > "$d/comparison.json"
jq '{benchmark, measurements, task_metrics}' "$d/a.json"
jq '.metrics' "$d/comparison.json"
```

Each run writes its report even when individual task checkers fail and the suite exits nonzero. The report includes the configured provider/model, per-task success in `task_pass`, expected and observed tool names in `task_metrics`, token coverage and wall times. Keep both JSON reports with the revisions under test. Provider usage omissions survive serialization as null measurements. [Metric definitions](baseline.md)

Two examples of the live success criteria are a safe edit that leaves the forbidden file unchanged, and a retrieval answer that identifies both `evt_9f3a2c` and `server.go`. Either task may pass through different call sequences. The tool-choice rubric is scored independently.

## Available live data

The M8-3 measurements used `deepseek-v4.1-flash-free` via an OpenAI-compatible gateway. The historical full 13-task set passed 2/13 before the authorization fix, with 1–5 model calls per run. That report predates the per-task tool-choice rubric and the new rules and catalog fixtures. It contains no paired token or latency comparison for the current 10-task set. The checked-in `capabilities-v1` baseline uses scripted data. A new real-model pair requires provider credentials. [Historical calibration](live-calibration.md) · [Scripted measurements](baseline.md)
