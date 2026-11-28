# Configuration

Settings load from an optional JSON file and then from the environment. The environment wins. The primary provider API key is never read from the main configuration file: the JSON decoder rejects unknown fields, so `api_key` in the file is an error.

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
  "anthropic_base_url": "https://api.anthropic.com",
  "anthropic_model": "claude-sonnet-4-5",
  "model_prices": {},
  "oracle_max_cost_usd": 0,
  "max_run_tokens": 0,
  "max_run_cost_usd": 0,
  "http_token": "",
  "http_allowed_hosts": [],
  "http_allow_insecure": false,
  "disable_snapshots": false,
  "command_env_allow": [],
  "command_limits": {},
  "servers": [
    {"name": "files", "command": "/usr/local/bin/files-mcp", "args": ["--root", "/srv/data"], "readonly": false}
  ],
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

## MCP servers

`servers` (alias `mcp_servers`; when both are set `servers` wins) lists local stdio and remote Streamable HTTP MCP servers.

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | Unique label made of letters, digits, `_`, and `-`. Tools are registered as `mcp.<name>.<tool>`. |
| `command` | stdio | Executable to start, resolved through `PATH` when it has no slash. It is not run through a shell. |
| `args` | no | Arguments passed to the command. |
| `readonly` | no | Default `false`: every call waits for approval unless `policy.json` decides otherwise. `true` declares that the server's tools have no side effects; such tools are denied unless a `policy.json` rule allows them. |
| `url` | HTTP | MCP endpoint. Use HTTPS, or HTTP on loopback. Credentials, query strings and fragments are rejected. |
| `transport` | no | Defaults to `streamable_http` with a URL or `stdio` with a command. |
| `token_env` | no | Name of a process environment variable holding the resource bearer token. |
| `oauth` | no | Pre-registered public PKCE client's `issuer`, `client_id`, `redirect_uri` and optional array of `scopes`. Select one authentication method per server. |

```json
{
  "servers": [
    {"name": "local", "command": "/usr/local/bin/local-mcp"},
    {"name": "learn", "url": "https://learn.microsoft.com/api/mcp", "readonly": true},
    {"name": "catalog", "url": "https://catalog.example/mcp", "token_env": "CATALOG_MCP_TOKEN", "readonly": true},
    {"name": "tasks", "url": "https://tasks.example/mcp", "oauth": {
      "issuer": "https://auth.example/tenant",
      "client_id": "cloud-harness",
      "redirect_uri": "http://127.0.0.1:19421/v1/mcp/oauth/callback"
    }}
  ]
}
```

The public Microsoft Learn entry works without credentials. With only that entry configured, `cloud-harness doctor --json` reports the discovered documentation tools. [Microsoft Learn MCP](https://learn.microsoft.com/en-us/training/support/mcp)

Register the exact callback URI with the issuer and open Settings on the same
origin. Choose **MCP 服务 → 授权**. The client checks RFC 9728 resource metadata,
discovers the pinned issuer using OAuth or OpenID metadata, and requires S256.
The resource metadata URL must share the resource's origin. The authorization
and token endpoints must share the configured issuer's origin. HTTP redirects
are disabled for authenticated and discovery requests.

Access and refresh tokens stay in the serving process. The client refreshes
expiring tokens and requests a fresh authorization when the grant expires.
Disconnect clears both credentials and any pending authorization. After a
process restart, authorize again in Settings. For separate CLI or unattended
processes use `token_env`. Settings can reconnect either transport and reload
its inventory. Changing server definitions requires restarting the process.

`serve`, `run`, `chat`, `runs resume`, and `runs recover` start the servers,
discover their tools, and stop them on exit. A server that fails to start is
reported on stderr and skipped; it does not stop the command. Other commands do
not start servers, except `doctor`, which starts each one briefly to probe it.
See [tools.md](tools.md#mcp) for the call path, limits, and child-run rules.

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
`<approval_base_url>/ui?run=<run-id>` as `approval_url`. When it is
omitted, the notification contains no approval link.

## Environment

| Variable | Default | Purpose |
| --- | --- | --- |
| `CLOUD_HARNESS_ADDR` | `127.0.0.1:19421` | HTTP listen address |
| `CLOUD_HARNESS_DB` | `.cloud-harness/harness.db` | SQLite file |
| `CLOUD_HARNESS_PROVIDER` | `mock` | `mock`, `openai`, `openai-responses`, or `anthropic` |
| `CLOUD_HARNESS_HTTP_TOKEN` | `""` | Bearer token required on `/v1/` endpoints (minimum 16 characters when set) |
| `CLOUD_HARNESS_HTTP_ALLOWED_HOSTS` | unset | Comma-separated extra `Host` header values `serve` accepts, replacing `http_allowed_hosts` from the file |
| `CLOUD_HARNESS_HTTP_ALLOW_INSECURE` | `false` | Deprecated compatibility setting; non-loopback `serve` still requires `http_token` |
| `CLOUD_HARNESS_ALLOW_EXTERNAL` | unset | Deprecated legacy compatibility setting; cannot bypass the HTTP token requirement |
| `CLOUD_HARNESS_OWNER` | `local` | lease owner id |
| `CLOUD_HARNESS_RETENTION_DAYS` | `0` | history-index age limit; `0` keeps every row |
| `CLOUD_HARNESS_MAX_RUN_TOKENS` | `0` | Input plus output tokens one root run may use, child runs included; `0` is unlimited. See [Run token and cost limits](#run-token-and-cost-limits) |
| `CLOUD_HARNESS_MAX_RUN_COST_USD` | `0` | Estimated USD one root run may cost, child runs included; `0` is unlimited |
| `CLOUD_HARNESS_CONFIG` | unset | optional JSON file |
| `CLOUD_HARNESS_DISABLE_SNAPSHOTS` | `false` | Disable automatic workspace snapshots (`1` or `true`); the default captures pre/post snapshots around batches with side effects |
| `CLOUD_HARNESS_COMMAND_LIMITS` | unset | `off`, or `name=value` pairs overriding single `command_limits` fields; see [Command resource limits](#command-resource-limits) |
| `CLOUD_HARNESS_MODEL_PROFILES` | unset | JSON object replacing `model_profiles` from the file |
| `CLOUD_HARNESS_MODEL_ROLES` | unset | JSON object replacing `model_roles` from the file |
| `OPENAI_API_KEY` | unset | required only when the provider is `openai` or `openai-responses` |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | Chat Completions or Responses origin, without `/chat/completions` or `/responses` |
| `OPENAI_MODEL` | `gpt-4.1-mini` | default model id for roles without a profile; also used by `openai-responses` |
| `OPENAI_DISABLE_IMAGES` | `false` | Reject image input locally for a text-only compatible endpoint/model; JSON field `openai_disable_images` |
| `ANTHROPIC_API_KEY` | unset | required only when the provider is `anthropic` |
| `ANTHROPIC_BASE_URL` | `https://api.anthropic.com` | Messages API origin, without `/v1/messages` |
| `ANTHROPIC_MODEL` | `claude-sonnet-4-5` | default Anthropic model id for roles without a profile |

## Command resource limits

On Linux every `run_command` process starts under kernel resource limits
(`setrlimit`), which its child processes inherit. `command_limits` adjusts
them. An omitted field keeps its default and `0` leaves that one resource
unlimited.

| Field | Default | Kernel limit | Effect |
| --- | --- | --- | --- |
| `memory_bytes` | `8589934592` (8 GiB) | `RLIMIT_DATA` | Private writable memory (heap, anonymous mappings, thread stacks) of each process |
| `address_space_bytes` | `0` (unlimited) | `RLIMIT_AS` | Virtual address space of each process |
| `cpu_seconds` | 120 per CPU core, at least `600` | `RLIMIT_CPU` | CPU time of each process; the kernel sends `SIGXCPU` at the limit and `SIGKILL` 5 seconds later. The default cannot be reached within the 120 second timeout, so it only stops processes that outlive their command |
| `file_size_bytes` | `4294967296` (4 GiB) | `RLIMIT_FSIZE` | Size of any single file a process writes |
| `open_files` | `16384` | `RLIMIT_NOFILE` | Open file descriptors of each process |
| `processes` | `16384` | `RLIMIT_NPROC` | Processes and threads of the user running the harness, counted across the whole system |
| `disabled` | `false` | | `true` starts commands with none of these limits |

```json
{
  "command_limits": {"memory_bytes": 17179869184, "cpu_seconds": 0}
}
```

`CLOUD_HARNESS_COMMAND_LIMITS` overrides the file: `off` disables the limits,
and a comma-separated list such as `memory_bytes=17179869184,cpu_seconds=0`
replaces the named fields, keeps the others, and turns the limits back on if
the file disabled them. Values are non-negative integers in bytes, seconds, or
counts.

What the limits do and do not cover:

- They are per process, not a budget for the command's process tree. A command
  that starts ten processes can use ten times `memory_bytes`. Only `processes`
  bounds the tree, and it counts every process of the user, including ones the
  command did not start. The kernel does not enforce it for root.
- The default memory limit is `RLIMIT_DATA` and not `RLIMIT_AS` because
  Chromium reserves far more address space than it uses and fails under an
  8 GiB address-space limit. Set `address_space_bytes` to add one.
- C and C++ binaries built with AddressSanitizer or ThreadSanitizer map
  terabytes of writable shadow memory at startup and fail under the default
  `memory_bytes` with `failed to allocate ... bytes`. Set `memory_bytes` to
  `0` to run them. Go's race detector (`go test -race`) is not affected.
- A limit never raises what the harness process itself runs under (`ulimit`,
  systemd, a container): the lower value wins.
- Core dumps are turned off (`RLIMIT_CORE` 0) whenever limits are enabled.
- The 120 second timeout, the environment allowlist, and the 1 MiB output cap
  apply as before. On Linux, when unprivileged user namespaces are available,
  the command runs in its own process group and in a private user and mount
  namespace where `.cloud-harness` and the harness configuration directory are
  masked by empty tmpfs mounts, even when `disabled` is true; a timeout or
  cancellation kills the whole process group.
- For `--worktree` runs on Linux, the original workspace `.cloud-harness`
  is masked while the run worktree is preserved. If the configuration directory
  contains the workspace, `run_command` fails closed because the mask would hide
  the workspace. On a restricted Linux kernel where the mask cannot be applied,
  safe commands keep the rlimits-only path while approved dangerous commands are
  not started.
- macOS does not apply these limits, but it does keep commands away from the
  control files: see [Control files on macOS](#control-files-on-macos).
  Windows runs commands without the limits or the control-file protection.

### Control files on macOS

On macOS every `run_command` process runs under a Seatbelt profile, applied
with the system's `/usr/bin/sandbox-exec`. The profile allows everything except
file operations on the workspace `.cloud-harness` directory and the harness
configuration directory (`trust.json`, `policy.json`, and the settings file).
There, reads, writes, `stat`, hard links, clones, renames, and deletes all fail
with `Operation not permitted`. Linux shows the command an empty directory
instead; on macOS the directory is visible in its parent's listing but cannot be
opened.

- The first `run_command` on macOS creates the workspace `.cloud-harness`
  and the configuration directory, empty and with mode `0700`, if they do not
  exist yet, and leaves them in place; in a `--worktree` run that includes a
  `.cloud-harness` in the run worktree. On a case-insensitive volume this is
  what stops a command, or a background process it leaves running, from
  creating `.Cloud-Harness` and having the harness read it later. An empty
  directory changes nothing on the next start: nothing in it is trusted, and
  hooks, policy and trust load as if it were absent. Git does not show empty
  directories. If the harness cannot create one (the parent is not writable),
  the command is not started, since a command running as the same user could
  make the parent writable and create the other spelling itself. A read-only
  volume is the exception.
- For `--worktree` runs the original workspace `.cloud-harness` is denied
  while the run worktree inside it stays usable. The two directories above the
  worktree answer `stat` so its path can be resolved, and nothing else.
- Seatbelt rules follow a file's current path, so the directories above a
  protected directory, up to the volume root, cannot be renamed or removed by
  the command. That includes the workspace directory itself.
- Before the command starts, the harness checks from inside the sandbox that
  the kernel refuses each protected path. If it does not, or the profile cannot
  be built (the configuration directory contains the workspace, a control path
  is a dangling symlink, a path contains a control character), the command is
  not started.
- Seatbelt profiles do not nest. A command that applies its own (`swift build`
  without `--disable-sandbox`, some browser-based test runners) fails inside
  `run_command`. When the harness itself already runs inside a sandbox, no
  profile can be applied: safe commands run unprotected and approved dangerous
  commands are not started, as on a restricted Linux kernel.
- As on Linux, a process the command leaves running stays under the same
  restrictions for as long as it lives.
- As on Linux, the protection covers the command's own process tree. A command
  that asks a service outside it to touch the files (launchd, an application
  started with `open`, Apple events, `ssh localhost`, a Docker daemon) is not
  contained.
- `sandbox-exec` is marked deprecated by Apple but ships with every current
  macOS release. If a future release removes it, approved commands stop being
  started rather than running unprotected.

How a limit shows up in the `run_command` result is described in
[tools.md](tools.md).

## Model profiles

Use `model_profiles` to name model choices and `model_roles` to assign them.
Each profile requires a `model` and can specify a different `fallback_model`.
Both models use the configured provider, endpoint, and credentials. For example:

```json
{
  "provider": "openai",
  "model_profiles": {
    "planning": {"model": "gpt-4.1-mini"},
    "work": {"model": "gpt-4.1", "fallback_model": "gpt-4.1-mini"},
    "brief": {"model": "gpt-4.1-nano"}
  },
  "model_roles": {"planner": "planning", "executor": "work", "fast": "brief"}
}
```

| Role | Used for |
| --- | --- |
| `planner` | The context planner, on turns that already qualify for planning |
| `executor` | The main model loop and delegated runs without a child agent `model` override |
| `fast` | Optional model based turn summaries when a summarizer is supplied, and future lightweight calls such as `/compact` |

Setting a `fast` profile does not replace the default local summary with a
model call. A child agent with a `model` override uses that model and does not
switch to the executor profile's backup. Any role you leave unmapped keeps its
existing default model and retry behavior. With no role mappings, the call and
event sequence is unchanged. The optional environment variables above each
replace their entire JSON object; a role must refer to a profile in the
resulting configuration.

For a mapped role, HTTP 429 and 5xx responses trigger up to three retries. The
delay starts at one second and doubles up to a cap of eight seconds (the
three waits are 1, 2, and 4 seconds). Cancellation interrupts the wait. Other
4xx responses and errors within a response stream follow the existing error
path without retrying or switching models.

When retries are exhausted, the call can try the backup immediately if one
is configured. The switch persists for that profile in the same run,
including after a wait and resume. `provider.fallback` records the switch
with only `role`, `from_model`, `to_model`, and `reason` (`rate_limited` or
`upstream_5xx`). It is available from `GET /v1/runs/{id}/events`. Each attempted
call, including retries and the backup, uses a slot in `model_calls`. If the
budget is already exhausted when the switch is recorded, the backup cannot
be called until the budget is extended.

## Model prices

`model_prices` is optional JSON configuration used by `cloud-harness stats`
and the `ask_oracle` cost limit for estimates. Keys are model names and each value
contains `input`, `output`, and `cached` prices per one million tokens. Prices
must be finite and non-negative. The cached price applies to the cached subset
of input tokens; the estimate charges the remaining input tokens at `input`,
including cache creation tokens. Reasoning tokens are already part of output
and are not priced a second time. No separate cache-write or reasoning price
is configured.
No default prices are supplied, so an unpriced model is reported with cost
`—` rather than an invented amount. The current database schema does not store
the model id on each response; stats therefore maps OpenAI rows to the current
`openai_model` and uses the persisted provider value for other rows. Keep the
matching key aligned with that attribution when enabling estimates.
With model profiles, stats estimates use the default model's price; they cannot
break down costs by the models actually called. Oracle estimates use the model
recorded in each `oracle.asked` event and its input/output token counts. An
unpriced oracle model contributes zero to the oracle total.

`stats` exposes this same event-sourced ledger as `oracle_consultations`,
`oracle_estimated_cost_usd`, and `oracle_by_model`. `doctor` includes the
all-runs oracle summary and counts `oracle.failed` events with `reason:
"budget"`, which identifies consultations blocked by the cost circuit
breaker. Text reports render estimated USD to four decimal places; JSON keeps
the amount numeric. `stats` also exposes `model_usage_by_model`, an all-run
breakdown of persisted model-response calls and numeric estimated costs; an
unpriced model contributes zero there as well. No cost rows are persisted: both
commands recalculate from the existing event history and usage rows, and an
empty history reports zero and empty model breakdowns.

`oracle_max_cost_usd` is an optional non-negative finite dollar limit per run.
Zero (the default) means unlimited and disables the amount limit.
`CLOUD_HARNESS_ORACLE_MAX_COST_USD` overrides the file value. The limit is
checked before each new consultation request against the accumulated cost from
that run's `oracle.asked` events, including after resume. Since the completed
consultation's token usage is unknown before it runs, one consultation can put
the run over the limit; the next consultation is stopped before its request.
All monetary amounts are estimates.

Each model entry contains numeric `input`, `output`, and `cached` fields. Use
the exact model name as the key, and fill the values from the provider's
current pricing; the example configuration leaves this map empty by default.

CLI flags `--addr`, `--db`, `--provider`, and `--http-token` override the environment for that process. `--http-allow-insecure` remains accepted for compatibility and cannot waive the HTTP token requirement.

`openai-responses` reuses the OpenAI base URL, model, API key, reasoning replay, image, and retry settings, and posts to `/responses`. `anthropic` uses the Anthropic base URL, model, and API key, and posts to `/v1/messages`. Both providers are stdlib HTTP clients and return `provider.HTTPStatusError` for HTTP status failures. Unmapped calls retry HTTP 429 and 5xx responses with exponential backoff and jitter before any response items are produced; mapped profile calls leave retries and fallback to `ProfileProvider`.

## Run token and cost limits

Every run counts the tokens its model calls report and their estimated cost,
next to its model, tool, script, and child-run counters. The run's `budget`
object carries `used_input_tokens`, `used_output_tokens`,
`used_cached_input_tokens`, `used_cache_creation_input_tokens`,
`used_reasoning_tokens`, and `used_cost_usd`. Input includes both cache reads
and cache creation; reasoning is included in output. The token total is always
input plus output, without adding these subsets again. The executor, the
planner, turn summaries, compaction summaries, and `ask_oracle` are all counted. A child run's usage is
counted on the child and on its parent.

`cache_reported` and `reasoning_reported` become true once any call reports
the corresponding optional fields, including an explicit zero. Missing or
false markers in old JSON mean the provider did not report those fields;
clients can still show a positive legacy cached-input counter. The run detail
shows zero only when reported, and hides unavailable cache/reasoning details.
These counters and markers use the existing budget/usage JSON columns, with
no database migration. OpenAI Chat and Responses retain the provider's total
output (including reasoning); Anthropic input sums input, cache creation, and
cache reads. The standalone Gemini usage parser adds thoughts to candidates
for output; no Gemini request transport is introduced.

`max_run_tokens` (`CLOUD_HARNESS_MAX_RUN_TOKENS`) limits input plus output
tokens for a root run, and `max_run_cost_usd`
(`CLOUD_HARNESS_MAX_RUN_COST_USD`) limits its estimated cost. Both default to
`0`, which is unlimited. Cost uses [`model_prices`](#model-prices): a model
without a price counts tokens and costs zero, so a cost limit needs a price for
each model the run calls.

Token counts are known only after a call returns, so a limit is checked before
each model call: the call that crosses a limit completes and is recorded, and
the next one is refused. The run then waits with `wait_reason` `BUDGET`, the
same wait as an exhausted model-call count, and keeps its checkpoint. Resume it
with a higher limit:

```bash
cloud-harness runs resume --extra-tokens 200000 RUN_ID
cloud-harness runs resume --extra-cost-usd 2 RUN_ID
```

or `POST /v1/runs/{id}/budget` with `extra_tokens` or `extra_cost_usd`. A
resume that leaves the run without room for one more model call is refused and
changes nothing. `extra_tokens` and `extra_cost_usd` have no effect on a run
that has no such limit. A child run starts with the tokens and cost its parent
has left.

## Conversation history and compaction

A new turn's context holds, after memory and before the new user message, the
user and assistant messages of the session's most recent turns, oldest first.
At most six turns are carried, and together they take at most a third of the
context limit: older turns are dropped first, and a message that is too long
on its own keeps its start and end with a pointer to the full text for
`retrieve_resource`. Child runs are not part of the history. The run records a
`context.history` event with the number of turns, messages, and tokens. After
`/compact`, the compacted summary stands in for the turns it covers and only
later turns are carried as messages.

When a run's context grows past the limit, the run asks a model to summarise
the turn's older work (the `fast` [model profile](#model-profiles) when one is
configured, otherwise the run's model) and replaces that work with the
summary. The system prefix, the current user message, and the newest tool call
with its result stay as they were, and a tool call is never separated from its
result. A later summary folds the earlier one in. The summary call is a model
call of the run: it counts toward its budget, is priced as the model that
answered it, and appears in `stats`; the run records `context.summarized`. A
run whose budget cannot pay for the summary waits for more budget with its
context unchanged. If the call fails (`context.summary_failed`), or the
context is still over the limit, older tool results are replaced by an excerpt
with a `ref` for `retrieve_resource` and other long messages keep their start
and end.

## HTTP authentication and non-loopback rule

- `http_token` (or `CLOUD_HARNESS_HTTP_TOKEN`) defaults to `""`. When set, it must be at least 16 characters long; shorter tokens are rejected at config load time.
- When `http_token` is set, API requests under `/v1/` require `Authorization: Bearer <token>` (verified in constant time). The browser redirect at `GET /v1/mcp/oauth/callback` authenticates the flow with OAuth state, a matching cookie and PKCE. `/healthz` remains open.
- Loopback binds (`127.0.0.1`, `localhost`, `::1`) work without `http_token`.
- A loopback bind refuses requests whose `Host` header is not `localhost` or a loopback IP address (HTTP 403). `http_allowed_hosts` (or `CLOUD_HARNESS_HTTP_ALLOWED_HOSTS`, or `serve --http-allowed-hosts`) lists extra accepted values, each a host name or IP address with an optional port and no scheme or path, for example `["harness.example", "proxy.example:8443"]`. An entry without a port matches every port. Invalid entries are rejected at config load time. On a non-loopback bind the `Host` header is unrestricted unless this list is set.
- Every bind refuses a request whose `Origin` header differs from its `Host`, and requires `Content-Type: application/json` on write requests with a body. See [request checks](api.md#request-checks).
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
The inspector also records a separate pre/post pair around each actual
side-effecting tool dispatch. An approval wait, denial or cached reply does
not start a dispatch pair. A fresh execution after resume or retry gets a
new pair, even when the batch or invocation ID is reused. Dispatch pairs
have a null `event_seq` and support code-only rewind.
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
export OPENAI_MODEL='agnes-3.0-flash'
cloud-harness eval run --task batch-three-reads --provider openai
```

The command creates the task fixture, runs the tool loop, and prints JSON with `status`, `run_id`, `tool_calls`, and `pass`. The key is not written to the fixture, the trace, or the JSON result.

The [Agnes 3.0 Flash documentation](https://www.agnes-ai.com/doc/agnes-30-flash)
requires an API key. It lists both nonzero standard prices and a zero Angel
User Trial Price; billing and availability depend on the account and key.
This example does not establish a keyless or account-specific free path.
An eval result also does not establish HTTP application acceptance. See
[Agnes application acceptance](agnes-acceptance.md) for the isolated
`serve` / Manager / approval / persisted-message checks and their execution
status.

## Session model selection and compatible providers

The conversation header reads `/v1/models`: the configured default model, each
`model_profiles` entry as `profile:<name>`, and enabled custom models as `custom:<providerID>/<model>` (legacy `custom:<id>` selections still resolve to the default model).
A selection affects subsequent runs; named profiles retain their configured
fallback models. A waiting run retains its resolved model,
provider ID, approval mode and reasoning effort in its checkpoint. Keys are resolved privately
at runtime and never placed in checkpoints.

Choices live in `session_preferences` in the configured SQLite `db` (default
`.cloud-harness/harness.db`). Before creating a session, the browser keeps only
the draft model ID, approval mode and thinking choice in `cloud-harness.web-draft-preferences`, then
writes them to the new session. Custom provider keys never go into browser storage.

Custom OpenAI compatible, Anthropic and Gemini services use `compatible-providers.json` beside
`policy.json`: the directory containing `CLOUD_HARNESS_CONFIG`, or Go's user
configuration directory plus `cloud-harness` (Linux: `$XDG_CONFIG_HOME/cloud-harness`,
normally `~/.config/cloud-harness`). The file contains saved keys with mode 0600,
atomic replacement and serialized writes within the serve process. Do not add
it to source control, logs or shared backups. Use one settings writer process
per shared configuration directory.

The authenticated `/v1/providers` API returns only `has_api_key`, never the key.
An empty key during edit retains the saved value. HTTPS is required except
for loopback HTTP endpoints. Tests use temporary directories, fixture keys and
`httptest` endpoints without acceptance provider credentials.

Model service settings can discover the protocol and model list using read-only
catalog requests; a manual protocol or model ID is also accepted. The enabled
list controls the header menu, independently of the last fetched catalog.
Gemini runs through its official OpenAI compatibility endpoint; Anthropic uses
its native Messages API. Refresh failures retain the previous catalog.

Session thinking effort defaults to `default`, preserving effort-model service behavior
and enabling toggle-model thinking.
`off`, `low`, `medium` and `high` apply to the next run and survive waits in
its checkpoint. Known unsupported models omit the override; a thinking
parameter rejected with HTTP 400 is retried once without it and remembered
for that provider/model until this process restarts.


Model settings use one searchable, filterable list with enable checkboxes and
an inline default choice. Other modalities/router/realtime models are collapsed;
enabled/default entries remain visible. Manual IDs can always be added.
Public models.dev metadata supplies friendly names, classification and toggle
or effort capabilities. Address host + longest API-path prefix wins, then
first-party providers before aggregators; capabilities come from one record.

Only a fixed credential-free GET to https://models.dev/api.json is made. The
public models-dev-cache.json in the existing config directory expires after
24 hours. Requests time out after five seconds and refuse redirects; stale
cache survives failures and no cache uses name heuristics. No API keys or
custom base URLs leave for models.dev. The public cache is not a repository
asset. See [API metadata and thinking mapping](api.md#public-modelsdev-metadata)
for matching rules and wire behavior.

Agnes toggle models expose off/on (default and legacy effort preferences mean
on), using the documented chat_template_kwargs.enable_thinking boolean for
OpenAI-compatible requests. Effort models disable unadvertised values, for
example medium on Step 3.5 Flash, and never promote max/xhigh to high. The
existing HTTP 400 retry/cache remains the final negotiation fallback.
