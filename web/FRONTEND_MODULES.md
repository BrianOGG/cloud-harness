# Cloud Harness 前端 Web 模块清单

> 依据：cloud-harness 现有代码与 HTTP API（2026-11-12 核对）。给 coding agent 做 demo 时的规格说明。
> 一句话背景：harness 是一个 agent 运行时——用户发一句话，后端开一个 **run**，run 内部是「模型响应 → 工具批次 → 模型响应…」的循环，带权限审批、预算、等待/恢复、子任务。前端的本质是把一个 run 的生命周期可视化并提供干预入口。

## 前端使用的后端接口

| 接口 | 用途 |
|---|---|
| `GET /healthz` | 健康检查，返回 `{status, provider}` |
| `GET /v1/sessions` | 列出会话、run 数和最近状态/时间 |
| `POST /v1/sessions` | 新建会话，返回 `{id}` |
| `GET /v1/sessions/{id}/runs` | 列出该会话全部 runs |
| `POST /v1/sessions/{id}/runs` | 发消息开 run，body `{message, model_id?, approval_mode?}`，返回 run 视图。默认立即返回 `RUNNING` 的 run；`?wait=1` 等 run 停下才返回（M13-2 之前的行为）。恢复 run 的三个接口相同 |
| `GET /v1/runs/{id}` | 查 run 视图：`{id, session_id, parent_id, status, wait_reason, budget{...}, final_answer, error, version}`；子 run 才有 `parent_id` |
| `GET /v1/runs/{id}/events` | SSE：先重放该 run 的持久事件（带 `Last-Event-ID` 请求头则从其后开始），随后保持连接推送新事件，run 终态后断开。模型生成中的文本以无 `id`、不落库的 `stream.delta` 推送；`stream.reset` 表示丢弃当前响应已显示的文本 |
| `GET /v1/runs/{id}/trace` | 执行轨迹：`{session_id, run:{status, wait_reason, steps[], children[]}}`；step = 一次模型响应 `{id, action, batches[]}`；batch = 一批工具调用 `{id, status, action, calls[]}`；call = `{id, name, status, ordinal, provider_call_id}` |
| `POST /v1/runs/{id}/responses` | 恢复等待中的 run，body 可带 `{decision}` / `{answer}` / `{extra_model_calls}` |
| `GET /v1/approvals` | 读取待审批工具调用和 ask_user 问题 |
| `POST /v1/runs/{id}/budget` | 补预算，body `{extra_model_calls}` |
| `POST /v1/runs/{id}/cancel` | 取消 run |

## 全局硬约束（先读，否则会假设错接口）

1. **服务端列表可用**：会话与 run 列表分别来自 `GET /v1/sessions`、`GET /v1/sessions/{id}/runs`；服务端数据是列表权威来源。API 不返回 turn 文本，本客户端创建的消息在浏览器本地保存，旧会话缺失的原文会显示占位提示。
2. **API 无 CORS 头**：前端不能跨域直连，开发时用 Vite 代理或同源部署。M11-2 起 UI 在 `/ui/` 同源托管。
3. **token 流式输出走 SSE**：发消息和恢复 run 的请求立即返回 `RUNNING` 的 run（前端显式带 `?wait=0`，与默认值一致）；随后保持 `GET /v1/runs/{id}/events` 连接。`stream.delta` 不落库、没有游标：重连时服务端会把当前响应已生成的文本从头重发，所以前端重连前要清空已显示的流式文本。持久事件用 `Last-Event-ID` 续传；最终回答以 run 视图的 `final_answer` 为准。
4. **模型的原始思维链不可见（设计如此）**：推理内容是 opaque continuation，只回放给同一 provider，不落盘、不经 API 透出。前端能展示的"思考过程"是**编排层行踪**：每一步模型响应的意图、规划器决策、工具批次、用量（见模块 6）。
5. **一轮对话 = 一个 run**：会话里每发一条消息就开一个新 run；消息列表与 run 一一对应。
6. **演示数据注意**：默认 mock 模型只会一步直达 ANSWER 并复读用户输入。工具调用、审批、等待等状态不会自然出现，demo 要用脚本化 provider / eval 场景造数据，或直接对着状态枚举做 mock 渲染。

## 枚举速查

- run 状态：`RUNNING` / `WAITING` / `COMPLETED` / `FAILED` / `CANCELLED`
- 等待原因（wait_reason）：`USER`（等回答）/ `APPROVAL`（等审批）/ `CHILD`（等子任务）/ `BUDGET`（等补预算）/ `RECONCILIATION`（等对账）
- 模型响应意图（action）：`ANSWER` 回答 / `RETRIEVE` 检索历史 / `EXECUTE` 执行工具 / `PLAN` 规划 / `ASK_USER` 向用户提问
- 预算四组计数：`model_calls` / `tool_calls` / `script_calls` / `child_runs`，各有 used/max
- 主要事件类型：`run.created` `run.resumed` `run.completed` `run.cancelled` `model.completed`（带 action、input/output tokens、latency_ms、provider）`planner.selected` `planner.completed` `planner.failed` `tool.batch`（带 batch 状态与 calls 概要）`tool.probed` `tool.succeeded` `child.started` `child.finished` `turn.summary`

---

## A. 基础层

### 1. 连接状态模块
- 职责：显示后端是否可达、当前 provider 名；断线时明确提示。
- 数据：`GET /healthz`（页面加载时查一次即可）。

### 2. 会话管理模块
- 职责：新建会话、会话列表、切换会话；每个会话下挂它的 run 列表（标题取用户首句、状态点）。
- 数据：`GET /v1/sessions`、`POST /v1/sessions`、`GET /v1/sessions/{id}/runs`；run 状态按需轮询。

### 3. 布局壳
- 职责：左侧栏（新对话、观测、对话列表）/ 中间对话 / 右侧可开关的运行详情（预算、执行步骤、事件）。窄屏下侧栏和详情变成浮层。纯前端，无数据。

## B. 对话层

### 4. 对话框模块（消息列表 + 输入框）
- 职责：消息气泡列表（用户消息右、assistant 左，最终回答 Markdown 渲染）；底部输入框（回车发送、发送中禁用）；空状态引导。
- 数据：发送 `POST /v1/sessions/{id}/runs {message}`；assistant 侧内容取 run 的 `final_answer`；run 未完成时气泡显示状态占位（运行中/等待某事）。
- 注意：发送后要清输入框；同一会话可连续发多条（= 多个 run 顺序排列）。

### 5. 运行状态与预算条
- 职责：顶部呈现当前 run 的状态徽标 + wait_reason + 四组预算计数（used/max）；RUNNING/WAITING 时随事件流刷新；提供「取消运行」按钮。
- 数据：收到持久事件后读 `GET /v1/runs/{id}`；取消 `POST /v1/runs/{id}/cancel`。

## C. 过程可视化层（核心）

### 6. 思考过程模块（步骤时间线）
- 职责：把一个 run 的内部推进画成时间线：每一步 = 一次模型响应，显示其**意图**（五种 action 语义，用人话翻译：如"决定先查历史""决定执行工具""向用户提问"）、该步的 token 用量与延迟；规划器介入时插入"规划：选了哪些资料、精度如何"的节点。
- 数据：`GET /v1/runs/{id}/trace` 的 steps（action）+ 事件 `model.completed`（tokens/latency）+ `planner.*` 事件。
- 边界：原始思维链不可见（约束 4），文案上就叫"执行步骤/决策过程"，不要承诺展示模型内心独白。每步的**可见文本**在后端存储里有（VisibleItem），但 trace 接口暂未透出——要展示每步文本需后端给 trace 加字段。

### 7. 工具调用模块
- 职责：按批次展示工具调用：批次状态（顺序执行）、每次调用的名称、序号、状态（待审批/已派发/运行中/成功/失败/结果未知）；点开看参数与返回结果。
- 数据：trace 的 batches/calls 给名称与状态骨架；事件 `tool.batch` 给批次概要。
- 边界：调用的**参数与结果**后端存储里都有（Invocation.Arguments / Result{ok,data,error}），trace 的 call 节点提供只读 `arguments` 与 `result`。默认转录收起这些数据，展开过程后按调用 ID 查看；不暴露 provider opaque continuation。

### 8. 事件流模块（调试面板）
- 职责：按序号滚动展示 run 的全部持久事件（类型 + 时间 + payload 摘要），支持按类型筛选、自动跟随最新。
- 数据：`GET /v1/runs/{id}/events` + `Last-Event-ID` 增量拉取（约束 3）。

### 9. 子任务模块
- 职责：展示父 run 派出的子 run（children 树）：各自状态、等待原因、完成后的摘要；父 run 在 `WAITING(CHILD)` 时有明确提示。
- 数据：trace 的 `children[]` + 事件 `child.started` / `child.finished`；子 run 本身可再用其 id 查 trace（递归一层即可，深度上限 1）。

## D. 人机协同层（等待态处理，全部走 responses/budget 接口）

### 10. 审批模块
- 触发：run `WAITING` + `APPROVAL`。
- 职责：展示待审批的工具调用（哪个工具、参数摘要、为什么危险）+「同意 / 拒绝」按钮；操作后 run 自动继续，前端恢复轮询。
- 数据：`POST /v1/runs/{id}/responses {decision: "approve"|"deny"}`。待审批调用的详情同样受模块 7 的字段透出限制。

### 11. 回答提问模块
- 触发：`WAITING` + `USER`（模型用了 ASK_USER）。
- 职责：展示问题 + 回答输入框 + 提交。
- 数据：`POST /v1/runs/{id}/responses {answer}`。

### 12. 补预算模块
- 触发：`WAITING` + `BUDGET`。
- 职责：展示已用/上限（模型调用次数），提供「+N 次」补充按钮；说明 run 是暂停不是失败、进度不丢。
- 数据：`POST /v1/runs/{id}/budget {extra_model_calls}`（或 responses 带 extra_model_calls）。

### 13. 对账模块
- 触发：`WAITING` + `RECONCILIATION`（某次工具调用结果未知，后端不会自动重放）。
- 职责：展示结果未知的调用 + 「标记成功 / 标记失败」按钮。
- 数据：`POST /v1/runs/{id}/responses {decision: "reconcile_success"|"reconcile_fail"}`。

### 14. 取消与恢复感知
- 职责：取消按钮（模块 5）；页面刷新后能恢复现场——靠自记的 session/run id 回查 run 视图与事件，等待中的 run 要重新弹出对应的协同操作条（模块 10–13）。

## E. 收尾

### 15. 错误与终态呈现
- 职责：`FAILED` 时展示 run 的 `error{code,message}`；`CANCELLED` 有明确终态样式；接口报错（4xx 的 `{error:{code,message}}`）统一 toast/条幅呈现。

## M11-8：Web 真 serve 全流程（旧界面的验收记录）

- 会话列表、运行记录、会话内待审批卡均以服务端 API 为准。运行详情展示状态、等待原因、四组预算、最终回答/错误、执行步骤、工具批次、子任务和事件流。
- 当前会话里每个 RUNNING/WAITING run 保持一条 SSE 连接（`api.streamEvents`），事件按 seq 合并，断线后带 `Last-Event-ID` 重连，重连前先清空该 run 已显示的流式文本；`stream.delta` 实时显示在对话气泡里，`stream.reset` 清空当前响应的文本（`liveText`）。另有约 1 秒一次的 run 视图轮询。可见轨迹只包含编排层状态，不展示模型思维链。
- 会话内 WaitCard 可以直接 Approve/Deny；ask_user 在会话内回答。服务端负责恢复运行（请求立即返回），前端通过事件流跟进状态。
- token 使用 `localStorage['cloud-harness.http-token']`。不设置 token 时 UI 仍可打开，受保护 API 错误会提示 token 设置位置。
- `npm test` 检查 Last-Event-ID 续接和四组预算计数。`web/scripts/serve-acceptance.py` 通过脚本化 OpenAI 兼容 provider 对真实 serve 运行审批批次、子任务和 ask_user 流程，并用浏览器截取浅/深主题截图。

## M11-15：观测与响应式布局（旧界面的验收记录）

- 「观测」从同源的 `GET /v1/stats` 和 `GET /v1/doctor` 读取数据。检查项按失败、警告、通过排列；失败运行和最近错误可直接打开运行详情。没有用量、缺少价格、接口失败各有明确提示。
- 深色主题覆盖四个主屏。宽度 ≤768px 时隐藏运行侧栏，卡片改为单列。没有 action 的轨迹步骤用「步骤 N」作标题。
- 从仓库根目录运行 `bash web/scripts/acceptance.sh`。完整输出和退出码写入 `.cloud-harness/m11-15/acceptance-final.log`，四屏浅/深主题截图写入 `web/screenshots/m11-15/`。浏览器在 390px 和 768px 两种视口检查布局；设置 `M11_MOBILE_SCREENSHOTS` 可把额外的 390px 深色截图输出到指定目录，供目视复查。

---

## Demo 切分建议

- **Demo 1（对话骨架）**：模块 1、2、3、4、5、15——能建会话、发消息、看状态和预算、取消。mock 模型即可跑通。
- **Demo 2（过程可视化）**：模块 6、7、8、9——步骤时间线 + 工具调用 + 事件流 + 子任务。需要脚本化场景造数据（多步、带工具调用、带子任务各一条）。
- **Demo 3（人机协同）**：模块 10、11、12、13、14——四种等待态的操作条与恢复。需要能稳定触发 APPROVAL / USER / BUDGET / RECONCILIATION 的场景各一条。

## 对话式界面（当前实现）

`src/` 是对话式界面，上面各模块在其中的位置：

| 模块 | 位置 |
| --- | --- |
| 1 连接状态、2 会话管理 | 侧栏：底部连接状态，按日期分组的对话列表，标题取首条消息 |
| 4 对话框 | 对话区：用户消息在右，回答按 Markdown 渲染；底部输入框，回车发送，运行中变成停止按钮 |
| 5 状态与预算、14 取消 | 每轮单行状态摘要、悬停或聚焦显示的详情/取消图标，以及输入框的停止按钮；预算在运行详情里 |
| 6 步骤、7 工具调用、9 子任务 | 每轮一个默认收起的过程摘要；保存的消息与运行事件统一按轮投影，展开后查看过程和审计 |
| 8 事件流 | 运行详情的「事件」页签 |
| 10 审批、12 补预算、13 对账 | 对话里的操作卡片；没有独立审批页 |
| 11 回答提问 | 问题显示为 agent 的消息，在输入框里回答 |
| 15 错误与终态 | 过程摘要显示终态；错误原文在摘要下一行，接口错误显示在顶部 |

- `POST /v1/sessions/{id}/runs` 立即返回 `RUNNING` 的 run（M13-2 起的默认行为，前端仍显式带 `?wait=0`）。请求在途时前端轮询会话的 run 列表，找到新 run 后立即跟随它的事件流，所以对等到 run 停下才返回的旧服务端同样适用。
- `stream.delta`（没有 `id`，payload `{"text": "..."}`）属于过程里的实时草稿，默认收起；根 run 完成后，正文只以该根运行终态的 `final_answer` 为准。约束 3 在 serve 发出 delta 之前仍然成立。
- 带 `parent_id` 的 run 是子 run，细节归入所属根轮过程，不将子 run 的 response 或 final_answer 当成 Harness 根运行最终答复；子 run 的当前待办仍必须可见。
- 没有模型 key 时用 `scripts/demo-provider.py` 演示工具调用、审批、提问、子任务和失败。
- 验收：从仓库根目录运行 `bash web/scripts/acceptance.sh`，它用脚本化 provider 对真实 serve 走完审批、拒绝、提问、子任务、失败和新对话，截图写入 `web/screenshots/chat/`。
- 待批操作使用 run 视图的 `pending_invocation`；已处理调用的参数 / 结果在展开本轮过程后通过只读 trace 查看，规则来源来自持久 `policy.decided` 事件。

## 会话偏好与内嵌审批（2026-11-27）

- 会话 header 从 `GET /v1/models` 加载已配置模型，用紧凑当前值菜单选择，无可见「模型」类别标签。
  切换会话读取 `GET /v1/sessions/{id}/preferences`，切换模型/审批模式通过同一路径
  PUT 保存。加载或保存偏好时禁用相关选择与发送，避免用旧值发起运行。
- 新对话偏好暂存于 `cloud-harness.web-draft-preferences`；创建会话后先 PUT，再在
  `createRun` 中显式携带 `model_id`、`approval_mode`。
- 自定义 API 统一在侧栏「设置 → 模型服务」管理（`Settings.tsx`）：列表、搜索、添加、编辑、删除确认、连接检测。
  连接是全局 / 用户配置；保存与删除不自动改写会话模型或审批模式，返回会话后自行在 header 选择模型。
  `GET/PUT /v1/providers` 只返回公开元数据，列表仅显示密钥已配置/掩码状态。
  密码输入框留空保留原 key，填写则替换；保存成功清空并关闭表单。完整 key 不进入 localStorage。
  `DELETE /v1/providers/{id}` 删除连接，确认文案说明当前模型可能不可用；删除后原选中 ID 保留并提示重选。
  `POST /v1/providers/{id}/test` 只检测已保存服务的 /models，显示进行中/成功/失败，不发推理请求；支持手动模型 ID，暂不自动导入模型列表。
- Composer 只负责多行草稿、发送/停止和标注“下一轮”的审批模式 chip（`ApprovalMode.tsx`）。chip 展开后显示四个模式的范围说明；偏好只作用于下一轮，不改变正在运行或等待的回合，也不会自动重放或放行当前调用。
  Composer 不接收 provider 元数据、Base URL 或 API key，不含凭据表单，也不重复模型下拉。
  无模型或原模型已删除时，在输入框外提供「去配置模型服务」入口；往返设置页保留原地草稿。
- 没有侧栏审批按钮和独立审批视图。`/ui?run=<id>` 定位会话及 WaitCard；
  `/ui?session=<id>` 打开指定会话。批准、拒绝、用户回答留在会话内，观测页保留。
- 统一转录由 `transcript.ts` 投影、`SessionHistory.tsx` / `Chat.tsx` 共用，`MessageList.tsx` 保留人类对话与原始审计条目。以 `turn_id` / `run_id` 分组，区分根运行与子运行，不再让 SessionHistory、Turn 和旧 past-runs 分别重复画主正文。完成后默认显示用户输入、根运行权威 `final_answer` 以及一行过程摘要；没有最终答复时如实显示状态。
  模型中间文本、工具参数 / 结果、已处理审批请求 / 决定与子运行细节归入默认收起的过程；用户主动展开后，事件刷新不强制收起。审计消息 ID、真实历史顺序与持久化记录不改。
  当前 WaitCard 由 `run.pending_invocation` 与待办状态产生，不猜历史最后一条审批请求；当前提问 / 回答、预算暂停与未知结果确认保留在折叠区外。审批提交中禁止重复操作，失败仍保留待办；成功后同一次调用退回过程摘要。
- 现有模式 ID 保持 `ask` / `safe` / `workspace-write`，可见名称依次为“按策略确认”/“文件读取与 Git 查看自动”/“工作区文件编辑自动 · 命令另行确认”。
  `ask` 沿用既有策略与底层许可，不代表每个读工具都询问；`safe` 只自动授权 `read_file` / `list_dir` / `list_files` / `grep` / `git_status` / `git_diff` / `git_log`；`workspace-write` 再加通过现有边界校验的 `write_file` / `edit_file`。
  前三种模式均不因模式本身授予命令；既有明确 allow、默认安全命令 `ls` / `pwd` / `wc` 与 deny 继续生效。待批解释统一为“此调用需要你确认。”，不推断自动放行或后端原因。
  本轮审批模式与工作区只在运行详情中展示；活跃运行的已知模式与下一轮选择不同时，状态行显示可悬停或聚焦的提示标记。缺少模式时不推断差异，也不用当前会话偏好补猜。受限诊断自动执行本版未交付，没有对应已生效控件。
- `npm test` 包含实际 App/Composer/Turn 的 JSDOM 行为回归：偏好持久化与隔离、
  新会话发送参数、通知深链、折叠与可见正文、header 选模、composer 边界、设置 CRUD、密钥输入与空值编辑、连接检测、删除后重选与无模型草稿保留。
- `npm run check:dist` 对照 Git 暂存区校验重新构建的 dist；可在提交前暂存构建产物后运行，CI 中暂存区与 HEAD 一致。

## 前端冗余精简 B（2026-11-27）

- 用户消息不再显示“你的消息”标题、常驻时间或 run ID；上海时间可悬停或通过 Tab 聚焦时间提示查看。run ID、审批模式与工作区保留在运行详情。
- 每轮只保留一行可展开状态与工具调用数，审批数仅统计当前待批准项。详情与取消为行右侧图标，悬停、键盘聚焦时显示；触屏保持可见。完成后的权威最终答复和默认折叠过程不变。
- WaitCard 显示批准标题、完整命令（单行可横向滚动）或工具名与主要路径；目录仅在与工作区不同时出现。原始参数放在默认收起的“详情”中，行为文件警告与批准/拒绝按钮仍直接可见。
- 失败只在状态行标记一次，错误原文紧随其后；取消终态也不再重复。顶栏不重复运行状态和详情入口，侧栏未命名会话不暴露 ID，设置页合并重复说明。
- #37 设置/模型服务、header 选模与 composer 审批芯片保留；#38 根运行最终答复与折叠过程投影保留；#39 完全自主风险确认及先勾选后 onChange 的行为不变。
