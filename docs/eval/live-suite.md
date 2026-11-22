# Live-model eval suite

`cloud-harness eval suite --live` runs the real-model subset of the eval tasks against the configured provider. It refuses the mock provider so a live run cannot be mistaken for scripted coverage.

```bash
export OPENAI_API_KEY=sk-...
export OPENAI_BASE_URL=https://api.openai.com/v1
export OPENAI_MODEL=gpt-4.1-mini
cloud-harness eval suite --live --provider openai --out docs/eval/live.json
cloud-harness eval suite --live --provider openai --compare docs/eval/live-baseline.json
```

The live set is the `live_tasks` list in [tasks.json](tasks.json):

- `dev-fix-failing-test`
- `dev-explain-call-site`
- `file-summarize-log`
- `file-safe-edit`
- `incident-service-down`
- `batch-three-reads`
- `retrieve-chinese-term`
- `retrieve-path-and-id`

Harness-mechanics probes stay in the scripted suite: `batch-approval-skip`, `recover-mid-batch`, `incident-wait-for-ready`, `planner-compare`, and `cache-baseline` have scripted call-shape or multi-arm expectations that do not measure real-model behavior.

Live scoring keeps the same pass/fail report shape but relaxes scripted call-order assumptions where a real model can satisfy the goal differently. For example, `file-safe-edit` passes when the allowed file changes, the forbidden file is unchanged, and any forbidden write attempt is denied; a compliant model that refuses without attempting the write also passes. `retrieve-path-and-id` checks that retrieval returns `evt_9f3a2c` and the answer names `server.go`, not a fixed number of retrieval calls.

Reports include `model_calls`, `tool_calls`, token totals, cache hit rate when the provider reports cached tokens, per-task pass/fail, and the usual suite comparison fields when `--compare` is used.
