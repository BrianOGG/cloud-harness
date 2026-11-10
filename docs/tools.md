# Tool interface

Every tool call goes through one broker. The broker checks the JSON arguments, the scoped grant, and the run budget, and only then calls the executor. A child run and a script use that same broker. Their charges are applied to the parent budget in the same transaction.

## Spec

A spec carries the name, description, JSON schema, whether it is readonly, idempotent, and pollable, a side-effect class (`none`, `reversible`, `irreversible`), `extract_resources`, an optional `status_probe`, and `Execute`. The model sees the name, description, and schema. It does not receive `Execute`.

`read_file` is pollable. `poll_tool` may target only a pollable readonly tool with side-effect class `none`. It stops when `until` matches or when `max_attempts` is hit.

`execute_program` runs only after the allowlist is granted. A side-effect tool must be listed in `confirm_side_effects` or the script does not start. During the script, a tool outside the allowlist returns `auth_denied`. The worker cannot open files, load modules, or open the network. See [worker-starlark.md](worker-starlark.md).

`delegate_task` starts a child run. The child gets only `allowed_tools`. It cannot call `delegate_task`. Its trace and checkpoint are its own. Cancelling the parent cancels the child.

## MCP

`ToMCPTool` maps a spec to `{name, description, inputSchema}`. `MCPErrorResult` maps a denial to an MCP tool result with `isError` and the domain code. `StaticMCP.Discover` returns the specs it was given. It does not open a socket. There is no MCP transport in this build.

## Permission

A grant names the tool, the operation, the resource scope, the run, and the expiry. A grant for `/ws/project` does not cover `/ws/project-other` or `/etc`. A sensitive read followed by `run_command` or `http_fetch` is denied unless a grant has operation `egress`.
