# Tool interface

Every tool call goes through one broker. The broker checks the JSON arguments, the scoped grant, and the run budget, and only then calls the executor. A child run and a script use that same broker. Their charges are applied to the parent budget in the same transaction.

## Spec

A spec carries the name, description, JSON schema, whether it is readonly, idempotent, and pollable, a side-effect class (`none`, `reversible`, `irreversible`), `extract_resources`, an optional `status_probe`, and `Execute`. The model sees the name, description, and schema. It does not receive `Execute`.

`read_file` is pollable. `poll_tool` may target only a pollable readonly tool with side-effect class `none`. It stops when `until` matches or when `max_attempts` is hit.

## Workspace tools

`load_skill` (`readonly`, idempotent) loads one discovered
`.agents/skills/<name>/SKILL.md` by its `name` argument. The result is
`{name, content, truncated}`, where `content` includes frontmatter and body.
The workspace file takes precedence over a same-named global skill. Unlisted
names beyond the catalog cap can be loaded explicitly. Missing names return
`tool_error`. `content` has a 64 KiB cap including the truncation marker.

The workspace file and command tools below resolve paths through `resolveInside`, which evaluates symlinks on existing paths (and on the nearest existing parent directory for not-yet-existing targets) and denies any target that resolves outside the evaluated workspace root with `auth_denied`.

- `read_file` (`readonly`, `pollable`, side-effect `none`): reads a UTF-8 file inside the workspace. Accepts required `path` and optional 1-based inclusive `start_line` and `end_line`. Returns `{path, content, total_lines, start_line, end_line, truncated}` and includes an explicit `notice` whenever content is truncated by the 1MiB cap or by the requested line range.
- `write_file` (side-effect `reversible`, `status_probe`): writes a UTF-8 file (up to 1MiB) inside the workspace. Requires a scoped grant or approval.
- `edit_file` (side-effect `reversible`, `status_probe`): surgical string replacement in a UTF-8 file inside the workspace. Accepts required `path`, `old_string`, and `new_string`. Requires `old_string` to match exactly once in the file; zero or multiple matches return an error and leave the file untouched. Routes through the same permission, approval, and status-probe machinery as `write_file`.
- `list_files` (`readonly`, `pollable`, side-effect `none`): lists workspace-relative file paths, optionally filtered by `pattern` (glob). Skips `.git` and `.cloud-harness` directories, caps results at 200 files, and includes a truncation `notice` when capped.
- `grep` (`readonly`, `pollable`, side-effect `none`): pure-Go (`regexp` + `bufio`) search over workspace files, skipping `.git` and `.cloud-harness`. Accepts required `pattern`, optional `glob` file filter, optional `literal` boolean (treats `pattern` as literal text instead of regex), and optional `max_hits` (default 50, hard cap 200). Returns `file:line:content` matches with `truncated` and an explicit `notice` when capped.
- `git_status` (`readonly`, `pollable`, side-effect `none`): runs `git status --porcelain=v1 -b` in the workspace root using a fixed argument vector (never a shell). Takes no arguments and returns `{status, stdout, lines, clean, truncated}` (or a clean `tool_error` if git is unavailable or the workspace is not a git repository).
- `git_diff` (`readonly`, `pollable`, side-effect `none`): runs `git diff --no-color --no-ext-diff` in the workspace root with optional `staged` (`--staged`) and optional `path` (resolved inside the workspace via `resolveInside`). Returns `{diff, stdout, staged, path, truncated}` (or a clean `tool_error` outside a git repository).
- `git_log` (`readonly`, `pollable`, side-effect `none`): runs `git log -n <limit> --no-color` in the workspace root with optional `limit` (default 20, hard cap 100). Returns `{log, stdout, limit, truncated}` (or a clean `tool_error` outside a git repository).
- `run_command`: runs a single allowlisted command (`ls`, `pwd`, `wc`) in the workspace without a shell (default 120s timeout); anything outside the safe set requires approval. Optional `cwd` must resolve inside the workspace via `resolveInside`. Passes only whitelisted environment variables (`PATH`, `HOME`, `LANG`, `LC_ALL`, `TERM`, plus any configured in `command_env_allow`). Returns `{argv, stdout, stderr, exit_code, truncated}` with an explicit truncation marker and `notice` when output reaches the 1MiB cap. On Linux the command and its child processes run under kernel resource limits for memory, CPU time, file size, open files, and process count (see [Command resource limits](config.md#command-resource-limits)). A command the kernel kills at its CPU time or file size limit fails with `resource_limit` and a message naming the limit and its `command_limits` field. The data of a command that failed for a reason attributed to a limit carries `resource_limit: {resource, limit, certain, message}`; `certain` is `false` when the limit is inferred from the exit status, memory use, or error output (for example `Cannot fork`, `Too many open files`, `out of memory`) of a command that exited on its own.

`execute_program` runs only after the allowlist is granted. A side-effect tool must be listed in `confirm_side_effects` or the script does not start. During the script, a tool outside the allowlist returns `auth_denied`. The worker cannot open files, load modules, or open the network. See [worker-starlark.md](worker-starlark.md).

`delegate_task` starts a child run for `goal` (required), with optional `agent`, `relevant_context`, `success_criteria`, `output_contract`, `allowed_tools`, `max_model_calls`, and `max_tool_calls`. The named agent can narrow `allowed_tools`, cap the child's budget, override its model, and add system instructions. See the [child agent reference](cli.md#select-an-agent-with-delegate_task).

`success_criteria` tells the child what counts as done, and `output_contract` gives the expected output shape. The parent checks the child's summary and self-assessment against the criteria. It returns `{child_run_id, status, summary, success, unmet}` and adds `error` on failure. Unmet criteria set `success` to `false` in the result. The child has its own trace and checkpoint, and cancelling the parent cancels the child.

## Expert consultation

The main model can call `ask_oracle` to get a second opinion on a difficult
question. Supply a nonempty `question` and optionally a selected `code` snippet
and additional `context`. Code and context are limited to 8 KiB and 4 KiB,
respectively. Oversized fields carry a truncation notice within the limit. The
tool masks common credentials in the supplied text before forwarding it, using
text patterns from `export --sanitize` such as API keys, bearer <redacted>, and
credential assignments.

The consultation uses the `planner` [model profile](config.md#model-profiles)
or the default model if `planner` is unset. It returns `{ "answer": "…" }` as
untrusted advice for the main model. The consultation receives no tool
definitions. `delegate_task` excludes `ask_oracle` from child tool sets.

A run can start up to three consultations, even after resuming or rewinding
the same run. Each provider attempt uses one of the run's `model_calls`, including
429 and 5xx retries and the planner's fallback attempt. A call limit returns
`budget_exhausted` before sending another request. Reported token usage
contributes to the same `stats` estimates as other model responses.

`oracle_max_cost_usd` optionally sets a per-run estimated cost limit. Before a
new consultation request is sent, the run's existing `oracle.asked` events are
recounted using each event's model and input/output token counts, with prices
from `model_prices`. When that accumulated cost is at or above the limit, the
request is denied with `budget_exhausted`, and `oracle.failed` records
`reason: "budget"`. Zero or an omitted limit disables this cost check. Since a
consultation's token usage is unknown until its response arrives, one
consultation can put the run over the limit; that cost is recorded and the next
consultation is blocked before its request is sent. Models without an entry in
`model_prices` add zero to the oracle total and are governed only by the
three-consultation limit.

`oracle.asked` records the question length in characters, the code bytes sent,
the last model called, reported token counts, and elapsed milliseconds. A
failed consultation records `oracle.failed` with `budget_exhausted`, `budget`,
`upstream_error`, or `timeout`. These event payloads contain no prompt text.

## MCP

Local stdio MCP servers listed under `servers` in the configuration file (see
[config.md](config.md#mcp-servers)) are started by every command that executes a
run: `serve`, `run`, `chat`, `runs resume`, and `runs recover`. Each server is a
subprocess that speaks newline-delimited JSON-RPC 2.0 on stdin/stdout. The
harness sends `initialize`, `notifications/initialized`, and `tools/list`, then
registers every discovered tool in the broker as `mcp.<server>.<tool>`. Only the
stdio transport exists; HTTP and SSE servers are not supported.

Lifecycle:

- Servers start concurrently when the command starts. Start plus discovery is
  limited to 30 seconds per server.
- A server that cannot start, does not answer, or returns an invalid tool list
  is skipped with a `warning: mcp server "<name>" is unavailable ...` line on
  stderr. The command and the other servers continue; the failed server's tools
  are not registered.
- A discovered tool is skipped with a warning when its name contains characters
  other than letters, digits, `_`, `-`, and `.`, or when the name is already
  registered.
- Servers are not restarted. If one exits later, its tools stay listed and each
  call fails with a tool error until the harness process is restarted.
- When the command exits, every server's process group is killed and reaped. On
  Linux the subprocess also receives SIGKILL if the harness itself dies.
- The subprocess inherits the harness working directory and only `PATH`, `HOME`,
  `LANG`, `LC_ALL`, `TERM`, and `TMPDIR` from the environment. Its stderr is
  discarded.

Calls use the same path as built-in tools: arguments are checked against the
server's `inputSchema`, the call is charged to the tool budget, `policy.json`
rules match the full name (`{"tool": "mcp.files.search", "effect": "allow"}`),
`PreToolUse` / `PostToolUse` hooks run, and the invocation, policy decision, and
approval are stored like any other. One call is limited to 15 seconds and 64 KiB
of result.

| Server setting | Tool class | Default without a policy rule |
| --- | --- | --- |
| `readonly: false` (default) | reversible, egress scope = server name | waits for approval on every call |
| `readonly: true` | no side effect, no egress scope | denied; needs a `policy.json` `allow` rule |

A tool from a non-readonly server counts as egress: after the run has read
sensitive data it is denied unless an explicit egress grant exists.
`readonly` is a declaration by whoever wrote the configuration; the harness
cannot verify it.

Child runs started by `delegate_task` never receive a grant for an MCP tool,
including when the tool is named in `allowed_tools`. A child cannot wait for
approval and `policy.json` `allow` rules do not widen a child's permissions, so
a child's MCP call ends the child with `WAITING (APPROVAL)` reported to the
parent. Agent definitions under `.cloud-harness/agents` cannot list MCP tools.

OpenAI-compatible endpoints only accept function names made of letters, digits,
`_`, and `-`, up to 64 characters. The provider adapter therefore sends
`mcp.files.search` as `mcp_files_search` (with a hash suffix if that would
collide or exceed the limit) and maps the model's calls back. Policy, grants,
events, and stored invocations always use the dotted name.

`doctor` reports the real state, see [cli.md](cli.md).

In-process mapping helpers remain: `ToMCPTool` maps a spec to `{name, description, inputSchema}`. `MCPErrorResult` maps a denial to an MCP tool result with `isError` and the domain code. `StaticMCP.Discover` returns the specs it was given without starting a process.

## Permission

A grant names the tool, the operation, the resource scope, the run, and the expiry. A grant for `/ws/project` does not cover `/ws/project-other` or `/etc`. A sensitive read followed by `run_command` or `http_fetch` is denied unless a grant has operation `egress`.
