# Configuration

Settings load from an optional JSON file and then from the environment. The environment wins. The API key is never read from the file: the JSON decoder rejects unknown fields, so `api_key` in the file is an error.

Tool approval rules are configured separately in global and workspace
`policy.json` files. See [分层权限策略](policy.md) for locations, precedence,
baseline denials, examples, and run snapshot behavior.

## File

`CLOUD_HARNESS_CONFIG` points at the file.

```json
{
  "listen": "127.0.0.1:19421",
  "db": ".cloud-harness/harness.db",
  "provider": "mock",
  "openai_base_url": "https://api.openai.com/v1",
  "openai_model": "gpt-4.1-mini",
  "model_prices": {},
  "http_token": "",
  "http_allow_insecure": false,
  "disable_snapshots": false,
  "command_env_allow": [],
  "notify": {
    "approval_base_url": "https://harness.example",
    "channels": [
      {"type": "webhook", "url": "https://hooks.example/harness", "events": ["approval", "completion"], "timeout_ms": 5000},
      {"type": "bark", "url": "https://bark.example", "events": ["approval"]},
      {"type": "ntfy", "url": "https://ntfy.sh/my-topic"},
      {"type": "osc9", "events": ["approval", "ask_user", "completion", "failure"]},
      {"type": "desktop", "events": ["approval", "completion"]}
    ]
  }
}
```

## Run notifications

Notifications are disabled when `notify` is absent or its `channels` list is
empty. Each channel has a required `type`, optional `events` filter, and optional
`timeout_ms` (default 5000). Network channels also require an HTTP(S) `url`.
The supported types are:

| Type | `url` | Credential environment variable |
| --- | --- | --- |
| `webhook` | Destination that receives a JSON `POST` | `CLOUD_HARNESS_NOTIFY_CHANNEL_N_WEBHOOK_BEARER` (optional bearer token) |
| `bark` | Bark server base URL; the device key is added as the final path component | `CLOUD_HARNESS_NOTIFY_CHANNEL_N_BARK_KEY` (required device key) |
| `ntfy` | Full ntfy topic URL, such as `https://ntfy.sh/my-topic` | `CLOUD_HARNESS_NOTIFY_CHANNEL_N_NTFY_TOKEN` (optional bearer token) |
| `osc9` | Omit; writes an OSC 9 notification to process stderr | None |
| `desktop` | Omit; runs `notify-send` on Linux | None |

`N` is the one-based position of that channel in `notify.channels`. Secret
fields are rejected in JSON; credentials are loaded only from these
environment variables and are omitted from redacted configuration logs.
Network targets must use HTTP or HTTPS and cannot contain URL credentials, query
strings, or fragments; inject secrets through the listed environment
variables. Logs show only each target's host and path.

Local channels accept no URL or credentials. OSC 9 requires a terminal that
supports these notifications. Its text is the event summary, or
`<event_type> <status>` when no summary is available. Control characters
(including escape characters) are removed before the text is truncated to 200
Unicode characters and written as `ESC ] 9 ; <text> BEL` to stderr.

Desktop notifications use the event type as the title and the same summary or
fallback text as Bark for the body. Linux must have `notify-send` in `PATH` and
an accessible desktop notification service. A missing command or a failed
invocation is a delivery error. Configuring `desktop` on any other operating
system fails startup with an `unsupported` error.

The valid event names are `approval`, `ask_user`, `completion`, and `failure`.
The `failure` event also covers cancellation. Omit `events` or use an empty
list to receive every lifecycle event; an explicitly listed filter receives
only those events.
Channels send concurrently. A channel's timeout is independent, defaults to
five seconds, and failures are not retried. Delivery errors add a `notify.failed`
run event and a log entry without changing the run result.
Canceling a run discards its pending notifications using the existing lifecycle
cancellation behavior.

Set `approval_base_url` to the externally reachable approval page origin or
base path. Waiting notifications then carry
`<approval_base_url>/approval?run=<run-id>` as `approval_url`. When it is
omitted, the notification contains no approval link.

## Environment

| Variable | Default | Purpose |
| --- | --- | --- |
| `CLOUD_HARNESS_ADDR` | `127.0.0.1:19421` | HTTP listen address |
| `CLOUD_HARNESS_DB` | `.cloud-harness/harness.db` | SQLite file |
| `CLOUD_HARNESS_PROVIDER` | `mock` | `mock` or `openai` |
| `CLOUD_HARNESS_HTTP_TOKEN` | `""` | Bearer token required on `/v1/` endpoints (minimum 16 characters when set) |
| `CLOUD_HARNESS_HTTP_ALLOW_INSECURE` | `false` | Deprecated compatibility setting; non-loopback `serve` still requires `http_token` |
| `CLOUD_HARNESS_ALLOW_EXTERNAL` | unset | Deprecated legacy compatibility setting; cannot bypass the HTTP token requirement |
| `CLOUD_HARNESS_OWNER` | `local` | lease owner id |
| `CLOUD_HARNESS_RETENTION_DAYS` | `0` | history-index age limit; `0` keeps every row |
| `CLOUD_HARNESS_CONFIG` | unset | optional JSON file |
| `CLOUD_HARNESS_DISABLE_SNAPSHOTS` | `false` | Disable automatic workspace snapshots (`1` or `true`); the default captures pre/post snapshots around batches with side effects |
| `OPENAI_API_KEY` | unset | required only when the provider is `openai` |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | Chat Completions origin, without `/chat/completions` |
| `OPENAI_MODEL` | `gpt-4.1-mini` | model id sent on that one call |
| `OPENAI_DISABLE_IMAGES` | `false` | Reject image input locally for a text-only compatible endpoint/model; JSON field `openai_disable_images` |

## Model prices

`model_prices` is optional JSON configuration used only by
`cloud-harness stats` for cost estimates. Keys are model names and each value
contains `input`, `output`, and `cached` prices per one million tokens. Prices
must be finite and non-negative. The cached price applies to the cached subset
of input tokens; the estimate charges the remaining input tokens at `input`.
No default prices are supplied, so an unpriced model is reported with cost
`—` rather than an invented amount. The current database schema does not store
the model id on each response; stats therefore maps OpenAI rows to the current
`openai_model` and uses the persisted provider value for other rows. Keep the
matching key aligned with that attribution when enabling estimates.

Each model entry contains numeric `input`, `output`, and `cached` fields. Use
the exact model name as the key, and fill the values from the provider's
current pricing; the example configuration leaves this map empty by default.

CLI flags `--addr`, `--db`, `--provider`, and `--http-token` override the environment for that process. `--http-allow-insecure` remains accepted for compatibility and cannot waive the HTTP token requirement.

## HTTP authentication and non-loopback rule

- `http_token` (or `CLOUD_HARNESS_HTTP_TOKEN`) defaults to `""`. When set, it must be at least 16 characters long; shorter tokens are rejected at config load time.
- When `http_token` is set, every `/v1/` endpoint requires `Authorization: Bearer <token>` (verified in constant time); `/healthz` remains open without authentication.
- Loopback binds (`127.0.0.1`, `localhost`, `::1`) work without `http_token`.
- Every non-loopback listen address (for example `0.0.0.0:19421`, `[::]:19421`, or `:19421`) requires a token. Without one, `cloud-harness serve` exits before opening the database or binding the listener with `non-loopback listen requires an HTTP token` and instructions to configure `--http-token`, `CLOUD_HARNESS_HTTP_TOKEN`, or `http_token` (at least 16 characters). The token is never printed in an error.
- `http_allow_insecure`, `--http-allow-insecure`, `CLOUD_HARNESS_HTTP_ALLOW_INSECURE`, and `CLOUD_HARNESS_ALLOW_EXTERNAL` remain accepted as legacy settings but no longer allow a non-loopback listener without a token.
- A missing or incorrect bearer token returns HTTP 401 with `WWW-Authenticate: Bearer realm="cloud-harness"`. The [API reference](api.md) includes the error envelope and the read-only `/v1/approvals` queue.

`retention_days` in the JSON file is the same history-index knob as
`CLOUD_HARNESS_RETENTION_DAYS` (default `0`, which keeps every row).
`cloud-harness retain --days N` overrides it for that invocation and deletes
history-index documents older than N days, keeping runs, events, checkpoints,
and workspace snapshots unless run pruning is explicitly enabled.

Run pruning is enabled separately with `retain --prune-runs-days N`; it defaults
to `0` and has no configuration-file or environment counterpart. A positive N
deletes terminal runs (`COMPLETED`, `FAILED`, `CANCELLED`) with `updated_at`
strictly before the UTC cutoff N calendar days ago, together with their child
rows and associated automatic/rewind-safety snapshot records. It also removes
snapshot records with an identifiable run ID whose run no longer exists,
regardless of snapshot age. Snapshots with no identifiable run association and
snapshots belonging to surviving runs are kept.

Add `--dry-run` to preview both enabled policies without deleting persistent
rows or modifying shadow Git refs/objects. The JSON `pruned` field is the
selected history-document count in a preview and the removed count in a real
pass. With run pruning enabled, `runs_to_delete` (dry-run only) and
`snapshots_to_delete` count selected database rows; `deleted_runs` (also
`pruned_runs`) and `deleted_snapshots` remain zero in a dry-run.
`unassigned_retained` counts preserved snapshot rows with no identifiable run
association across the database. `tables`/`deleted_details` preview only runs
and snapshot rows; the real report includes actual child-table deletions.

`affected_workspaces` counts distinct workspace IDs among selected snapshot
records. `reclaimed_workspaces` counts successful object-reclamation batches
in a real pass, including batches with no reclaimable objects; it is always
zero in a dry-run. `workspace_reclamation` reports each workspace's selected
`snapshots` count and estimated distinct `objects`/`bytes` reachable only from
the removed snapshot refs. Shared objects protected by surviving snapshots are
excluded. Bytes sum uncompressed Git object payload sizes, not measured disk
savings. `reclamation_failures` counts entries with an `error`, including
failed dry-run estimates.

Actual object reclamation runs per workspace after the SQLite deletion
transaction commits. It removes deleted snapshots' refs, expires reflogs, and
runs `git gc --prune=now` only in the shadow repository under
`.cloud-harness/snapshots/WORKSPACE_ID/`; workspace files and the workspace's
own Git repository are unchanged. Records save the absolute workspace path;
for legacy records without it, `--workspace` (default current directory) is
used only if its workspace ID matches. Reclamation errors are reported per
workspace and, in a real pass, as stderr warnings; committed database
deletions remain committed and the command can still exit zero. Neither
history-only pruning nor `--dry-run` performs object reclamation. See
[CLI retain](cli.md#retain) for the full report contract and estimate limits.

Resume flags are parsed before the run id:

```bash
cloud-harness runs resume --decision deny RUN_ID
```

`cloud-harness runs resume RUN_ID --decision deny` stops at `RUN_ID` and returns `run id required`.

Logs use a redacted view. The key is printed as `set` or `unset`, never as the secret.

Workspace snapshots use a separate Git repository under `.cloud-harness/snapshots/`.
Snapshot labels contain the run ID and batch number; `event_seq` refers to the
latest persisted event in that run when the record is created, or is null when
the event sequence cannot be read. Approval resume reuses the batch's pre
snapshot. A post snapshot is captured after each execution segment that actually
dispatched a side-effecting tool, including a segment ending in an approval wait.
Snapshot failures only add a `snapshot_skipped` event and never fail the run.
Unavailable Git is reported at most once per run. Setting `disable_snapshots`
to `true` also suppresses snapshot initialization and diagnostics.

Use `cloud-harness rewind RUN_ID --to SNAPSHOT_ID` to preview and restore a
snapshot (default `--all`; also `--code-only` and `--chat-only`). Code restore
first creates a safety snapshot. Interactive confirmation defaults to N;
non-interactive callers must pass `--yes`. See [CLI rewind](cli.md#rewind) for
checkpoint scope, safety recovery, and argument order.

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
