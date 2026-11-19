# Tool interface

Every tool call goes through one broker. The broker checks the JSON arguments, the scoped grant, and the run budget, and only then calls the executor. A child run and a script use that same broker. Their charges are applied to the parent budget in the same transaction.

## Spec

A spec carries the name, description, JSON schema, whether it is readonly, idempotent, and pollable, a side-effect class (`none`, `reversible`, `irreversible`), `extract_resources`, an optional `status_probe`, and `Execute`. The model sees the name, description, and schema. It does not receive `Execute`.

`read_file` is pollable. `poll_tool` may target only a pollable readonly tool with side-effect class `none`. It stops when `until` matches or when `max_attempts` is hit.

## Workspace tools

Every local tool resolves paths through `resolveInside`, which evaluates symlinks on existing paths (and on the nearest existing parent directory for not-yet-existing targets) and denies any target that resolves outside the evaluated workspace root with `auth_denied`.

- `read_file` (`readonly`, `pollable`, side-effect `none`): reads a UTF-8 file inside the workspace. Accepts required `path` and optional 1-based inclusive `start_line` and `end_line`. Returns `{path, content, total_lines, start_line, end_line, truncated}` and includes an explicit `notice` whenever content is truncated by the 1MiB cap or by the requested line range.
- `write_file` (side-effect `reversible`, `status_probe`): writes a UTF-8 file (up to 1MiB) inside the workspace. Requires a scoped grant or approval.
- `edit_file` (side-effect `reversible`, `status_probe`): surgical string replacement in a UTF-8 file inside the workspace. Accepts required `path`, `old_string`, and `new_string`. Requires `old_string` to match exactly once in the file; zero or multiple matches return an error and leave the file untouched. Routes through the same permission, approval, and status-probe machinery as `write_file`.
- `list_files` (`readonly`, `pollable`, side-effect `none`): lists workspace-relative file paths, optionally filtered by `pattern` (glob). Skips `.git` and `.cloud-harness` directories, caps results at 200 files, and includes a truncation `notice` when capped.
- `grep` (`readonly`, `pollable`, side-effect `none`): pure-Go (`regexp` + `bufio`) search over workspace files, skipping `.git` and `.cloud-harness`. Accepts required `pattern`, optional `glob` file filter, optional `literal` boolean (treats `pattern` as literal text instead of regex), and optional `max_hits` (default 50, hard cap 200). Returns `file:line:content` matches with `truncated` and an explicit `notice` when capped.
- `git_status` (`readonly`, `pollable`, side-effect `none`): runs `git status --porcelain=v1 -b` in the workspace root using a fixed argument vector (never a shell). Takes no arguments and returns `{status, stdout, lines, clean, truncated}` (or a clean `tool_error` if git is unavailable or the workspace is not a git repository).
- `git_diff` (`readonly`, `pollable`, side-effect `none`): runs `git diff --no-color --no-ext-diff` in the workspace root with optional `staged` (`--staged`) and optional `path` (resolved inside the workspace via `resolveInside`). Returns `{diff, stdout, staged, path, truncated}` (or a clean `tool_error` outside a git repository).
- `git_log` (`readonly`, `pollable`, side-effect `none`): runs `git log -n <limit> --no-color` in the workspace root with optional `limit` (default 20, hard cap 100). Returns `{log, stdout, limit, truncated}` (or a clean `tool_error` outside a git repository).
- `run_command`: runs a single allowlisted command (`ls`, `pwd`, `wc`) in the workspace without a shell (default 120s timeout); anything outside the safe set requires approval. Optional `cwd` must resolve inside the workspace via `resolveInside`. Passes only whitelisted environment variables (`PATH`, `HOME`, `LANG`, `LC_ALL`, `TERM`, plus any configured in `command_env_allow`). Returns `{argv, stdout, stderr, exit_code, truncated}` with an explicit truncation marker and `notice` when output reaches the 1MiB cap.

`execute_program` runs only after the allowlist is granted. A side-effect tool must be listed in `confirm_side_effects` or the script does not start. During the script, a tool outside the allowlist returns `auth_denied`. The worker cannot open files, load modules, or open the network. See [worker-starlark.md](worker-starlark.md).

`delegate_task` starts a child run for `goal` (required), with optional `agent`, `relevant_context`, `success_criteria`, `output_contract`, `allowed_tools`, `max_model_calls`, and `max_tool_calls`. The named agent can narrow `allowed_tools`, cap the child's budget, override its model, and add system instructions. See the [child agent reference](cli.md#select-an-agent-with-delegate_task).

`success_criteria` tells the child what counts as done, and `output_contract` gives the expected output shape. The parent checks the child's summary and self-assessment against the criteria. It returns `{child_run_id, status, summary, success, unmet}` and adds `error` on failure. Unmet criteria set `success` to `false` in the result. The child has its own trace and checkpoint, and cancelling the parent cancels the child.

## MCP

`ToMCPTool` maps a spec to `{name, description, inputSchema}`. `MCPErrorResult` maps a denial to an MCP tool result with `isError` and the domain code. `StaticMCP.Discover` returns the specs it was given. It does not open a socket. There is no MCP transport in this build.

## Permission

A grant names the tool, the operation, the resource scope, the run, and the expiry. A grant for `/ws/project` does not cover `/ws/project-other` or `/etc`. A sensitive read followed by `run_command` or `http_fetch` is denied unless a grant has operation `egress`.
