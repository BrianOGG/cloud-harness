# Starlark worker

`execute_program` runs a script only after the allowlist is granted and the worker probe passes. A script runs in another process, or it does not run at all.

## What the script may do

Starlark is not Python. This dialect turns off `while`, recursion, `load` of modules, and top-level reassignment. There is no `open`, no network, and no shell. The only bridge is `broker.invoke`.

```python
def main():
    result = broker.invoke("harness.echo", {"ping": True})
    if not result["ok"]:
        return result
    return result["data"]
```

`broker.invoke` returns `{ok, data, error}`. `error` is `{code, message}` or `None`. The parent broker checks the grant and the budget before any executor runs. A tool that is not allowed comes back as `auth_denied`. The worker does not ask the user mid-script.

Stable codes: `script_syntax`, `script_error`, `script_policy`, `auth_denied`, `resource_limit`, `program_disabled`, `worker_crashed`.

Scripts are capped at 64 KiB and 100000 interpreter steps (hard max 1000000). Those caps are extra. They do not count as memory or CPU isolation.

## When execution is allowed

On Linux the parent starts `cloud-harness worker`, reads `/proc/<pid>/limits` and `/proc/<pid>/environ`, and only then sends the script. The minimum is:

- a different process, killed if the parent dies
- `RLIMIT_AS` of 8 GiB, `RLIMIT_CPU` of 5 seconds, `RLIMIT_FSIZE` of 0, `RLIMIT_NOFILE` of 128
- the file-size limit actually rejects a one-byte write
- the child environment does not contain API keys or other `SECRET` / `TOKEN` / `PASSWORD` / `*_KEY` variables

`RLIMIT_NPROC` is not applied. It is a per-user limit and would throttle every process of this user.

cgroup v2 `memory.max` was probed by creating a child cgroup of the current one. On this machine the directory can be created and the write to `memory.max` is denied. RSS is therefore not capped. The 8 GiB address cap is a virtual-memory ceiling. Go needs about 2 GiB of that just to start, which was measured before choosing 8 GiB. A tighter cap kills the worker before `hello`.

If any of the minimum checks fail, programmatic execution is disabled. The script is not interpreted in the harness process. The same refusal is compiled in for non-Linux targets: Windows Job Objects and macOS limits have not been executed here, so those builds refuse.

`cloud-harness isolation` prints the probe. `cloud-harness program check` runs the echo script above through the broker when the probe passes.
