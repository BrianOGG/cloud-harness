# Configuration

Settings load from an optional JSON file and then from the environment. The environment wins. The API key is never read from the file: the JSON decoder rejects unknown fields, so `api_key` in the file is an error.

## File

`CLOUD_HARNESS_CONFIG` points at the file.

```json
{
  "listen": "127.0.0.1:19421",
  "db": ".cloud-harness/harness.db",
  "provider": "mock",
  "openai_base_url": "https://api.openai.com/v1",
  "openai_model": "gpt-4.1-mini",
  "command_env_allow": []
}
```

## Environment

| Variable | Default | Purpose |
| --- | --- | --- |
| `CLOUD_HARNESS_ADDR` | `127.0.0.1:19421` | HTTP listen address |
| `CLOUD_HARNESS_DB` | `.cloud-harness/harness.db` | SQLite file |
| `CLOUD_HARNESS_PROVIDER` | `mock` | `mock` or `openai` |
| `CLOUD_HARNESS_ALLOW_EXTERNAL` | unset | must be `1` to bind a non-loopback address |
| `CLOUD_HARNESS_OWNER` | `local` | lease owner id |
| `CLOUD_HARNESS_RETENTION_DAYS` | `0` | history-index age limit; `0` keeps every row |
| `CLOUD_HARNESS_CONFIG` | unset | optional JSON file |
| `OPENAI_API_KEY` | unset | required only when the provider is `openai` |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | Chat Completions origin, without `/chat/completions` |
| `OPENAI_MODEL` | `gpt-4.1-mini` | model id sent on that one call |

CLI flags `--addr`, `--db`, and `--provider` override the environment for that process.

`retention_days` in the JSON file is the same knob. `cloud-harness retain --days N` deletes history-index rows older than N days. Run, event, and checkpoint rows are kept.

Resume flags are parsed before the run id:

```bash
cloud-harness runs resume --decision deny RUN_ID
```

`cloud-harness runs resume RUN_ID --decision deny` stops at `RUN_ID` and returns `run id required`.

Logs use a redacted view. The key is printed as `set` or `unset`, never as the secret.

## One real call

```bash
export OPENAI_API_KEY='sk-...'
export OPENAI_BASE_URL='https://api.openai.com/v1'
export OPENAI_MODEL='gpt-4.1-mini'
cloud-harness run --provider openai --message 'Say hello in one sentence.'
```

No key is stored in this repository. The request path is covered by an `httptest` server in `internal/provider`.

## Eval task

```bash
export OPENAI_API_KEY='sk-...'
export OPENAI_BASE_URL='https://apihub.agnes-ai.com/v1'
export OPENAI_MODEL='agnes-2.5-flash'
cloud-harness eval run --task batch-three-reads --provider openai
```

The command creates the task fixture, runs the tool loop, and prints JSON with `status`, `run_id`, `tool_calls`, and `pass`. The key is not written to the fixture, the trace, or the JSON result.
