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

## 查询决策审计

```sh
cloud-harness policy decisions
cloud-harness policy decisions --run run_123 --tool write_file
cloud-harness policy decisions --effect deny --since 2026-11-19T00:00:00Z
cloud-harness policy decisions --limit 100 --db /path/to/harness.db --json
```

默认按事件时间倒序返回最近 50 条 `policy.decided`。`--limit` 必须为正整数，
超过 500 时按 500 处理。先过滤，再排序并应用条数限制；时间相同时按 run ID
升序、同一 run 的事件序号倒序，保证输出稳定。

| 参数 | 含义 |
| --- | --- |
| `--run ID` | 精确匹配 run ID。 |
| `--tool NAME` | 精确匹配工具名。 |
| `--effect allow\|ask\|deny` | 只显示指定效果。 |
| `--since RFC3339` | 只显示该时刻及之后的事件，包含边界；支持时区偏移和小数秒。 |
| `--limit N` | 返回条数，默认 50，最多 500。 |
| `--db path` | 沿用已有配置选择数据库，可显式覆盖。 |
| `--workspace dir` | 解析默认相对数据库路径时使用的工作区，默认当前目录。 |
| `--json` | 输出 JSON 数组；没有匹配记录时为 `[]`。 |

文本输出每条包括时间、run、工具名、效果、来源层和规则摘要。来源层为
`workspace`、`global`、`builtin` 或 `default`；用户规则摘要包括匹配的工具名、
可选 `path_glob`，命令前缀仍只显示已有哈希。内置和默认规则显示其现有说明。
JSON 额外保留 invocation ID 和可选规则序号，便于关联同一次调用。
审计只投影已有决策字段，不展开工具参数、文件正文、工具结果或完整命令。

查询使用只读数据库连接，不执行迁移，不启动 provider 或 MCP，不重新装载当前
策略文件。即使策略文件随后变更，仍可查询历史判定。空库、数据库尚未创建、
不存在的 run 或无匹配记录均返回空列表，不创建数据库。参数错误退出码为 2，
数据库或决策事件损坏等读取错误退出码为 1。

## doctor 策略汇总

`cloud-harness doctor [--workspace dir] [--db path] [--json]` 的 `policy` 检查列出：

- workspace、global 的策略文件路径、是否加载及规则数；缺失文件显示 0，空规则
  文件显示已加载且为 0。路径沿用上文配置目录约定。
- builtin 基线为内置规则（`embedded`），共 2 条，没有对应的磁盘规则文件。
- 从全部保留的 `policy.decided` 事件统计的 `allow`、`ask`、`deny` 次数。
  不受查询的 50/500 条限制；空库或数据库尚未创建时各项为 0。

规则文件汇总描述当前配置，历史决策计数描述 run 实际使用的策略快照；二者可能
因配置变更而不同。doctor 不输出规则正文或命令前缀，不读取工具参数、结果、
checkpoint 或模型内容来推断决策；汇总的事件读取只访问决策事件正文和关联 ID。
整个检查保持只读。策略文件加载失败或事件不可读时，`policy` 检查报告失败。
