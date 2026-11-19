# 确定性生命周期 Hooks（M11-10）

Hooks 在 agent 的固定执行位置运行，不依赖模型记住提示词。它们只能进一步拦截
操作；原有 policy、审批、预算、子任务工具限制和工具自身的工作区边界继续生效。

| 钩子点 | 触发位置 | 退出码 2 或超时的效果 |
| --- | --- | --- |
| `SessionStart` | 新 run 启动，装载配置并取得 lease 后 | run 失败，返回钩子错误，不调用模型 |
| `UserPromptSubmit` | 用户消息进入模型上下文和历史索引前；也包括 `ask_user` 的用户回复 | 初次提交失败并返回错误；回复被拒绝时保持原来的用户等待状态，可重新提交 |
| `PreToolUse` | policy 和现有权限门控放行之后，工具执行之前 | 当前调用成为未执行的 `DENIED`，钩子错误通过配对工具结果发送给模型 |
| `PostToolUse` | 每次 Broker 返回后，结果持久化和发送给模型前 | 工具结果带 `hook_blocked` 错误发送给模型；已发生的工具副作用不会撤销 |
| `Stop` | run 进入终态前，包括失败和取消；等待审批、用户、预算等暂停不触发 | 阻止成功完成，run 改为失败；已有失败或取消保持原来的终态与原因，并记录钩子阻断 |

所有动作遵循相同协议：`0` 放行；`2` 阻断；其他非零退出码放行并记录警告。
超时等同于阻断，事件包含超时原因。动作按数组顺序串行执行，遇到阻断就停止该
钩子点的后续动作。PreToolUse 阻断沿用批次的拒绝行为，后面的待执行调用被跳过。
policy 已拒绝、未获审批或审批被拒绝的调用不运行 PreToolUse。审批通过后才运行
该调用的 PreToolUse，且不会因为恢复而重跑 SessionStart。

脚本中的实际工具调用和 `poll_tool` 的每次尝试也经过前后钩子。嵌套调用先保留
原有预授权、子任务工具限制和 policy deny 检查，再运行钩子；不能通过包装绕过。
`finish_turn`、`ask_user` 仍是已有的控制协议，不经 Broker 执行；它们分别由 Stop
和用户回复时的 UserPromptSubmit 覆盖。

## 两层配置

文件位置与 B2-1 的配置目录口径一致：

1. 全局：`<配置目录>/hooks.json`。
2. 工作区：`<workspace>/.cloud-harness/hooks.json`。

配置目录为 `CLOUD_HARNESS_CONFIG` 指向的主配置文件所在目录；未设置时使用
`os.UserConfigDir()/cloud-harness`，Linux 通常为
`$XDG_CONFIG_HOME/cloud-harness` 或 `~/.config/cloud-harness`。使用 worktree 时，
工作区层来自工具实际执行的 worktree。

同名钩子点由工作区的整个数组覆盖全局数组；未声明的钩子点继承全局配置。空数组
可清空一个继承的钩子点。`go_check` 单独按工作区优先合并。不依赖主配置增加字段。

```json
{
  "go_check": true,
  "hooks": {
    "SessionStart": [
      {"type": "command", "shell": "sh", "command": "test -d .git", "timeout_ms": 1000}
    ],
    "UserPromptSubmit": [],
    "PreToolUse": [
      {
        "type": "starlark",
        "script": "def main():\n    input = broker.invoke(\"hook.input\", {})[\"data\"]\n    if input[\"tool\"] == \"write_file\" and input[\"arguments\"].get(\"path\") == \"protected.txt\":\n        return {\"exit_code\": 2, \"stderr\": \"protected.txt is maintained by the release process\"}\n    return {\"exit_code\": 0}\n"
      }
    ],
    "PostToolUse": [],
    "Stop": [
      {"type": "command", "command": "test -f required-output.txt || { echo 'Create required-output.txt before completion' >&2; exit 2; }"}
    ]
  }
}
```

文件缺失表示该层没有配置，用户钩子全空；内置 Go 自检仍默认开启。非法 JSON、
未知字段/钩子点、非法类型、动作缺少必填内容、非法 shell/cwd/timeout 都会在
装载时明确报错，包含文件路径、钩子点和从 1 开始的动作序号（适用时）。即使某个
全局钩子点被覆盖，该文件仍须合法。CLI 在打开 manager 时校验；直接嵌入 agent
的调用方在 run 启动时校验。配置文件上限为 1 MiB。

run 启动时读取并合并一次，配置随 checkpoint 保存。审批、用户、预算等恢复使用
原配置，包括由新 manager 恢复；修改配置只影响以后启动的 run。旧 checkpoint
首次恢复时装载 hooks。配置变更不改变 policy 的加载和求值。

## 动作与输入

| 字段 | 命令动作 | Starlark 动作 |
| --- | --- | --- |
| `type` | 必须为 `command` | 必须为 `starlark` |
| `command` | 必填、非空的命令文本 | 禁止 |
| `shell` | 可省略；仅接受 `sh`、`/bin/sh`、`bash`、`/bin/bash` | 禁止 |
| `script` | 禁止 | 必填、非空的内联脚本，最多 64 KiB |
| `cwd` | 可省略，默认工作区；必须解析到工作区内的现有目录 | 禁止，使用既有 worker 沙箱目录 |
| `timeout_ms` | 默认或 0 为 10 秒；可设 1–600000 毫秒 | 相同，包含 worker 启动时间 |

命令通过固定的 `/bin/sh -c` 或 `/bin/bash -c` 执行，不从调用方 PATH 查找
shell，不加载调用方的 `BASH_ENV` 等环境。环境仅提供固定系统目录和当前
Go 安装的 `bin` 组成的 `PATH`、`LANG=C`、`TMPDIR`（shell 自身可能增加 `PWD`）。
不继承 API key、HTTP token、代理或 `command_env_allow` 扩展。cwd 在装载和执行前
分别解析符号链接并检查边界。Linux 超时或取消会杀死整个子进程组，命令结束时也
清理其后台子进程。命令配置是受信任的本地代码；cwd 限制不是文件系统或网络沙箱。

输入通过命令 stdin 的 JSON 传入；Starlark 通过
`broker.invoke("hook.input", {})["data"]` 读取同一结构。按触发点提供：

```json
{
  "hook": "PreToolUse",
  "run_id": "run_...",
  "session_id": "ses_...",
  "invocation_id": "inv_...",
  "tool": "write_file",
  "arguments": {"path": "main.go", "content": "..."}
}
```

UserPromptSubmit 提供 `prompt`，PostToolUse 另外提供 `result`（`ok`、`data`、
可选 `error`），Stop 提供可选 `answer`。输入不会被复制到 hook 事件。

Starlark 复用 `internal/program.Execute` 的独立 worker、内核隔离探测、步数限制、
禁用文件/网络/模块加载的既有执行路径。唯一 broker 能力是读取 `hook.input`；
无法调用工具或申请权限，也不会递归触发钩子。`main()` 返回
`{"exit_code": 0, "stdout": "optional", "stderr": "optional"}`。现有 worker
丢弃 `print()` 输出，因此用返回值提供诊断。脚本语法、运行或隔离错误作为非零
警告处理；超时仍硬阻断。

## 内置 Go 自检

不需要 hooks.json，默认运行一个内置 PostToolUse 自检。显式关闭：

```json
{"go_check": false}
```

自检比较每次实际工具执行前后的 `.go` 文件内容哈希，覆盖 `write_file`、
`edit_file`、命令、自定义工具和 Starlark 中的修改，也检查先写入再报错的工具。
无需 Git，能识别未跟踪文件和已脏文件的后续修改，不依赖 mtime。扫描跳过
`.git`、`.cloud-harness` 和符号链接，不读取链接指向工作区外的文件。

检测到修改后，先对变动的现有文件执行 `gofmt -l`，再对变动文件所在的每个包执行
`go vet .`；不使用 `./...` 扫描其他包。也会对删除 Go 文件影响的包进行 vet。
包和文件按路径排序，整个自检默认限时 10 秒。使用当前 Go 安装中的 Go/gofmt，
禁用 Go 用户配置、自动工具链下载、模块下载和模块文件写入，使用现有 GOPATH
和构建缓存。不会自行运行 gofmt 写回文件。

格式、语法或 vet 错误放入 `hook.warning` 事件，并追加到原工具 JSON 结果的
`hook_diagnostics` 字段（非对象结果保留为 `tool_result`）。原工具失败时也把
自检诊断追加到错误文本。下一次模型请求在同一个 run 内看到错误，可以继续修复。
自检超时以 `hook_blocked` 工具错误回传。内置检查在用户配置的 PostToolUse 动作
之前执行，用户可在 `result` 中进一步检查追加的诊断。

## 事件与验收

每个实际动作记录持久 `hook.completed` 事件；其他非零退出码及 Go 自检问题记录
`hook.warning`。事件包含 `hook`、动作序号、类型、invocation 关联、退出码、
阻断/警告/超时标志、原因和输出。命令和脚本原文、prompt、参数及工具结果不会
自动写入 hook 事件。stdout/stderr 在移除已知环境凭据、常见凭据赋值、Bearer、
URL userinfo 和私钥后，合计截断到 4 KiB，并保持有效 UTF-8。钩子应只输出必要
诊断。审计事件不能写入时停止处理，避免无记录放行。

M11-10 的完整标准验收命令输出与退出码保存于
`.cloud-harness/m11-10/acceptance-final.log`。覆盖五个触发点、前置阻断的模型可见
错误、警告、超时与子进程清理、两种动作、配置校验与合并、审批/用户恢复、policy
deny 顺序、嵌套调用、默认 Go 自检、关闭自检和既有审批/预算回归。
