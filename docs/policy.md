# 分层权限策略（policy.json）

权限策略由 `internal/security` 独立读取，不需要在主配置中增加字段。每个 run
启动时读取一次两层文件，按以下顺序找到第一条匹配规则后停止：

1. 工作区：`<workspace>/.cloud-harness/policy.json`，按文件内顺序。
2. 全局：`<配置目录>/policy.json`，按文件内顺序。
3. 内置基线规则。
4. 默认 `ask`。

配置目录沿用 `CLOUD_HARNESS_CONFIG` 指向的主配置文件的所在目录。没有设置
`CLOUD_HARNESS_CONFIG` 时，使用 Go `os.UserConfigDir()` 下的 `cloud-harness`
目录；Linux 上通常为 `$XDG_CONFIG_HOME/cloud-harness`，未设置 XDG 时通常为
`~/.config/cloud-harness`。工作区路径与工具实际执行的工作区一致；使用 worktree
隔离时，为该 run 的 worktree 路径。

策略文件缺失表示该层没有规则。文件存在但 JSON、字段类型、必填字段、effect、
glob 或未知字段有误时，run 启动失败，错误指出文件路径和问题，规则错误还包含
从 1 开始的规则序号。空策略写成 `{"rules":[]}`。

## 文件格式与规则字段

```json
{
  "rules": [
    {"tool": "read_file", "path_glob": "fixtures/.env", "effect": "allow"},
    {"tool": "write_file", "path_glob": "generated/**", "effect": "allow"},
    {"tool": "run_command", "command_prefix": "go test", "effect": "allow"},
    {"tool": "write_file", "path_glob": "protected/**", "effect": "deny"},
    {"tool": "*", "effect": "ask"}
  ]
}
```

| 字段 | 必填 | 含义 |
| --- | --- | --- |
| `tool` | 是 | 工具全名或 `*`；不支持 `read_*` 这样的部分通配。 |
| `path_glob` | 否 | 目标路径模式；相对模式以执行工作区为根，绝对模式匹配绝对路径。 |
| `command_prefix` | 否 | 命令或脚本前缀，`tool` 须为 `*` 或命令工具：`run_command`、`execute_program`、`shell`、`bash`、`exec_command`。当前 `execute_program` 的命令内容是 `script` 参数。 |
| `effect` | 是 | `allow`、`ask`、`deny` 之一，区分大小写。 |

同一条规则中的选择条件必须同时满足。路径分隔符使用 `/`；`*`、`?`、`[abc]`
遵循标准库 `path.Match` 语义，不跨目录。独立路径段 `**` 匹配零个或多个目录，
因此 `**/.env` 同时匹配 `.env` 和 `a/b/.env`，`generated/**` 匹配任意深度。
路径会规范化，并检查可解析的符号链接目标；工作区外路径应使用绝对模式。
一个调用有多个文件目标时，`allow` 的路径条件必须覆盖全部目标；`deny`、`ask`
命中任意目标即匹配。没有路径的调用不会匹配带 `path_glob` 的规则。

命令和前缀去除首尾空白后比较；前缀之后必须是空白或命令结束。例如 `go test`
匹配 `go test ./...`，不匹配 `go testevil`。命令内容不会作为文件路径进行匹配；
命令工具的 `path_glob` 使用显式 `cwd`。规则可以同时指定 `cwd` 模式和命令前缀。

## 效果与内置基线

`allow` 自动放行当前工具调用，跳过人工审批。调用仍须通过 Broker 的参数校验、
预算检查、资源解析、工作区边界和敏感信息外发等既有约束。自动放行产生的权限只
绑定当前 run、session、invocation 和目标资源，不会变成后续调用的通用 Grant。
子任务仍受 `childGrants` 和工具交集限制，显式空工具集仍为空；策略不会增加子任务
原本没有的权限。Starlark 和轮询的既有预授权、确认要求也继续生效，其内部调用
同样检查策略拒绝。

`deny` 硬拦截，工具不执行、不消耗工具预算，也不会进入人工审批。工具结果包含
`auth_denied`、来源层和规则摘要，例如：

```text
policy deny (builtin): file read matches **/.env or **/.env.* (except .env.example)
```

这个结果会发送给模型，模型可以选择其他工具或路径。批次中的后续调用继续遵循
既有拒绝后的跳过规则。

`ask` 将控制权交回现有 Permission/Grant 门控：需要审批的调用继续进入
`WAITING(APPROVAL)`，已有有效 Grant 或原本允许的只读调用继续按既有行为处理。
因此，两层文件都不存在时，除下列基线拒绝外，行为与引入策略前一致。

内置基线按以下顺序生效：

1. 文件读取目标名为 `.env` 或 `.env.*` 时拒绝；准确命名为 `.env.example` 的文件除外。
   例如 `.env.local` 和 `.env.example.local` 会被拒绝。
2. 文件写入目标位于执行工作区之外时拒绝，包括 `..` 越界和符号链接指向工作区外的目标。

用户规则位于基线之前，可以显式覆盖基线，比如示例中的 `fixtures/.env`。
覆盖基线仍然遵循工具自身的硬约束；本地文件工具原有的工作区外写入限制不会被
`allow` 取消。文件基线针对文件工具的目标路径，不解析 shell 命令内部的文件访问。

示例最后的通配 `ask` 会停止后续层和基线的求值，只希望为少数操作增加规则时，应
省略这一条，使其他调用继续经过全局规则和基线。

## 装载快照与事件

run 启动后修改或删除策略文件，只影响以后启动的 run。策略快照写入已有 checkpoint；
审批、预算和用户等待恢复时继续使用原快照，包括由新进程恢复。引入策略前生成、
没有策略快照的旧 checkpoint 在首次恢复时装载策略。

每次实际策略判定记录持久事件 `policy.decided`，包含 `effect`、`source`
（`workspace` / `global` / `builtin` / `default`）、工具名、规则摘要和可选规则序号。
同一 invocation 的预检查、执行和审批恢复共用一个事件，不重复记录。
事件不包含文件正文、工具参数或命令全文；命令内容与规则中的 `command_prefix`
只以 SHA-256 哈希记录。嵌套轮询或脚本目标检查有自己的判定事件。
