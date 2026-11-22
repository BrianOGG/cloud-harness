# Eval baseline

`cloud-harness eval suite` runs the scripted tasks and prints the metrics those runs actually produce. `cloud-harness eval suite --live --provider openai` runs the real-model subset in [live-suite.md](live-suite.md); harness-mechanics probes stay scripted.

Metrics to collect on every case, once a live model exists:

- completed (yes/no) and a human or checker score against the success criteria
- wall time
- model calls, tool calls, and repeated tool calls
- input tokens, output tokens, cached input tokens when the provider reports them
- whether the run resumed after a crash without repeating a succeeded side effect

Defaults to calibrate later, not to change in this file: 100 main-model calls per run, and a reminder after 3 identical ordinary tool calls. The reminder itself is M2.

The machine-readable list is [tasks.json](tasks.json).
