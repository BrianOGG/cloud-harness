# Agnes 应用链验收（2026-11-26，上海时间）

> 当前状态：CH-PM 已在 Osaka 的 127.0.0.1:19441（目标模型 agnes-3.0-flash）
> 完成真实回复、自然 WAITING/APPROVAL、approve 写入及两次 transcript 一致性复核。
> 账单与零费用 entitlement 仍未核实；下文原 Provider 未执行的表述仅指早期交付阶段。

初始 Provider 交付范围：**文档与脚本；当时真实 Agnes 验收尚未执行**。该阶段交付了无密钥配置、provider 定点回归和真实应用的缺失密钥负向证据。后续 Chief 已完成一次隔离真实应用验收；详见本文末尾的 Chief follow-up。外部零账单和账号权益仍未核实。

## 官方资料与免费性边界

2026-11-26 在官方页面核查：

| 官方资料 | 核实内容 |
| --- | --- |
| [Agnes 3.0 Flash](https://www.agnes-ai.com/doc/agnes-30-flash) | 模型名为 agnes-3.0-flash，Base URL 为 https://apihub.agnes-ai.com/v1；支持 Chat Completions、stream、tools；请求示例带 Authorization: Bearer YOUR_API_KEY。 |
| 同页 Limits and pricing | 标准 cached/input/output 分别为 $0.005/$0.05/$0.15 每百万 token；Angel User Trial Price 三项均为 $0。 |
| 同页 Best practices / Limits and pricing | 明确 billing、availability、rate limits 取决于 Agnes 账号与 API key 的权益。 |
| [Quickstart](https://www.agnes-ai.com/en/docs/quickstart) | 前提是有效账号与有效 API key；说明 key 用于认证所有 API 请求。 |

$0 试用价不能证明无条件免费，更不能证明免密钥；本轮没有查看账号权益或账单。
本次交付不包含凭据读取或上游模型请求。应用验收的执行状态与账号免费性
分别记录；没有独立账单证据时，必须保留“未核实”的结论。

## 协议与配置结论

现有 openai provider 已能发送 Chat Completions 流式请求、解析文本和工具调用，
并经 cmd/serve 的 Manager 进入审批、消息持久化路径。没有证据支持添加 Agnes 专用生产代码。
本轮只修改 docs/config.md，并新增本文件及 scripts/acceptance-agnes.py。
生产 Go、配置规则、agent、storage、migrations、tools、web 和 web/dist 均未修改。

脚本准备的配置固定 provider=openai、上述 endpoint/model，model_profiles、
model_roles、model_prices 均为空，MCP/通知为空；不配置备用模型。
已有 provider 的同模型临时错误重试行为保留；脚本不重试模型操作。
默认响应头超时 30 秒、流空闲超时 120 秒；本次配置均设为 360000 毫秒，
以容纳派单报告的 4–5 分钟延迟，应用 HTTP 调用默认等待上限 900 秒。
这只是超时配置，尚未证实真实服务的响应格式及延迟。
未定价模型的本地 cost 为 0 也不构成任何外部免费证明。

## 独立运行与脚本

独占目录：/home/ubuntu/cloud-harness-dev/previews/cp1-agnes-868173b-20261126。
唯一应用地址：http://127.0.0.1:19437。脚本限制 hostname=osaka，
关闭 localhost 请求的代理和重定向，不触碰 19435/19436。

从本工作树执行：

~~~bash
python3 scripts/acceptance-agnes.py
# 退出 2，blocked_not_executed；无 HTTP、无凭据读取、无文件写入。
python3 scripts/acceptance-agnes.py --prepare
# 只创建 config.json 与空 workspace；退出 0，prepared_live_not_executed。
~~~

本轮 --prepare 已执行。重复 prepare 会拒绝覆盖，不要用它重置已有运行证据。
脚本既不启动服务，也不读取或装载任何凭据。Chief 审查并按自己的既有任务授权
接手真实执行，使用本轮编译的 cloud-harness 启动独立 serve，指定上述 config.json、
workspace 和独立 harness.db。凭据仅由该请求进程使用原文件
/home/ubuntu/.config/cloud-harness/acceptance-provider.env；不得复制、打印、
写日志、进入 argv，或通过环境继承引入模型角色、端点或 fallback 覆盖。
使用现有默认审批策略；不要新增 policy、trust 或 grant 迫使模型进入审批。
读取启动输出时只保留已脱敏的 provider、endpoint、model、db、workspace、超时等
指定字段，不输出整份配置。配置文件本身不能证明运行进程没有环境覆盖。

以下是原 Provider 阶段的 19437 脚本用法，当时未执行 --run；
后续 19441 的真实验收与 CH-PM approve 复核见文末，不应据此判断当前尚未执行：

~~~bash
python3 scripts/acceptance-agnes.py --run --decision deny --timeout 900
# 或只对同一隔离文本文件选择 --decision approve。
~~~

--run 会实际请求模型。它要求空 session 数据库，
拒绝覆盖 evidence.json，不会原样重试超时请求。请在能取得该次新鲜退出码的前台
长命令中运行，给予整个两次 run 加审批续作足够的总超时；不要并行探测。
保持指定 endpoint/model，不配置收费备用模型；如出现购买、充值或付费前提，
停止依赖该前提的步骤并保留脱敏错误，不执行购买或切换其他模型。
若调用失败，脚本尝试取消它创建的 session 内仍活动的 run，保留失败历史；
取消失败会单独记载，须由执行者核实服务状态，不能宣称已经清理。

脚本逐步记录本地 HTTP 状态、耗时、session/run/message ID，并核查：

1. 首个 run 经真实 serve/Manager 完成回复，消息历史已有对应 assistant 文本；
   只读 DB 中有该 run 的完整 openai 模型响应和目标模型元数据。
2. 第二个 run 的模型请求仅写 workspace/agnes-acceptance.txt 的固定文本，
   自然进入 WAITING(APPROVAL)；文件尚未写入，审批队列含该 run。
3. pending invocation 的工具与参数完全匹配；关联审批、调用、batch、完整模型响应、
   原始 provider_call_id 的持久来源，且没有提前派发或夹带其他工具。
4. /v1/sessions/{id}/messages 含对应持久审批请求；正常 approve/deny 后，
   同一 run 到 COMPLETED，审批状态和精确 approve/deny 文本均已持久化，
   队列移除，批准时文件匹配，拒绝时没有文件。
5. 既有消息身份/内容稳定，分页 cursor 不循环、不重复、不跨 session，顺序正确；
   没有 provider.fallback 事件、其他模型元数据或子 run。

结果 application_checks_passed 仅指上述应用断言通过。
模型元数据是应用记录的模型归属，不是上游账单或实际扣费证明；endpoint 是
准备配置中的值，仍需核查执行者的启动环境。external_free_status 保持
unverified_account_entitlement，必须与单独的官方价格、账号权益、账单证据一起解读。
脚本不会把无密钥负向检查、内存 fixture 或本地 cost=0 充作真实模型验收。

## 本轮实际执行证据

首动作：2026-11-26 11:20:18 CST（上海时间），hostname=osaka，
分支 cp1/agnes-provider-868173b，HEAD=868173bbe24a8d23b41f2fc086c27b5bf06bf280，起始干净。

- provider 定点回归退出 0，覆盖缺失 key、流式工具组合、不完整流不可派发、
  文本 delta、超时与流开始后不重试；耗时输出 0.324s。
- 共享锁 flock -n -E 75 下 go build ./cmd/cloud-harness 退出 0；无参数绕过 VCS 检查。
- Python AST、真实 schema 的只读查询验证、调用来源、已派发/错误模型拒绝、
  cursor 循环/重复/跨 session 拒绝通过。它们明确为离线 fixture 校验。
- 真实 serve/Manager 无密钥负向检查退出 0，serve 正常退出 0：
  session=ses_e8b9abd29ad22684ed95835a68e159ff，
  run=run_7bdfc63d26d80f8c211ca7441fa873c0，status=FAILED；
  错误为缺少 OPENAI_API_KEY，历史包含失败事实且没有 assistant 回复，
  model_responses 行数为 0。这不是 Agnes 推理成功。

独立运行目录保留 keyless-guard-evidence.json、负向用的 keyless-guard.db / workspace、
离线检查脚本、二进制和交接材料。负向检查的服务已经退出，端口 19437 未留进程；
当时供未来真实验收的 harness.db 尚未创建，workspace 为空。上述端口与目录状态
为 11:34 的历史观察；本次收口不再探测或操作 Chief 接管的实例与运行数据。
该次 Provider 初始收口没有提交、推送或创建 PR；由 Chief 组合审查并统一发布。


## Chief follow-up（2026-11-26，上海时间）

在 Osaka 的隔离副本 `dev@0de12ae88e34d2aa5881e0edfcf93907b887853d` 上完成后续运行。

- **应用验收**：隔离实例 `127.0.0.1:19441` 使用 `openai` provider、Agnes endpoint 和 `agnes-3.0-flash`；拒绝决策路径返回 `application_checks_passed`。真实首轮回复、第二轮自然进入 `WAITING(APPROVAL)`、拒绝后的终态与审批持久化、工作区未写入目标文件以及历史复读稳定均通过。证据：session `ses_ac3449fb0c78ab1edd69c4206ec30154`，记录在 Osaka 隔离目录 `cp1-dev-0de12ae-agnes-20261126-v4/evidence.json`。`external_free_status` 仍为 `unverified_account_entitlement`；这不证明零费用或账单权益。
- **当前预览**：现有服务已在 `127.0.0.1:19442` 恢复运行，provider=`openai`，模型=`agnes-3.0-flash`，仅绑定 loopback，使用独立新数据库和 workspace；`/healthz` 返回 200。该当前实例仅做了健康检查，没有重复发送模型请求。二进制 SHA-256：`6217159ba90471963eda529270b880f85e08de94cff52c74ef8ebb21664fb0aa`，Go VCS revision 为 0de12ae88e34d2aa5881e0edfcf93907b887853d。密钥从权限为 0600 的指定凭据文件装载；当前配置、日志和进程参数不包含密钥。

初始提交证据（无密钥负向测试及脚本离线验证）仍按上文记录；本 follow-up 的真实模型证据单独标注，不将本地成本或模型元数据解释为外部账单证明。

## CH-PM approve 与 transcript 复核（2026-11-26，上海时间）

CH-PM 于 14:44:57—14:45:08 对 Osaka 的 127.0.0.1:19441 进行后续审查，
目标模型为 agnes-3.0-flash；它与同实例较早的 v4 deny 验收为两次独立执行。
本次文档修订只读核对已保存证据，没有重跑模型调用或审批。

- 第一轮到达 COMPLETED，保存了非空真实模型回复；此处不复制回复内容。
- 第二轮自然由 RUNNING 进入 WAITING，wait_reason 和 wait_kind 均为 APPROVAL。
- 正常 approve 后，同一轮恢复并到达 COMPLETED，workspace 下
  ch-pm-acceptance.txt 已写入；此处不复制文件内容。
- 连续两次读取 transcript，各返回 8 条消息，两次完整结果完全一致。

证据：/tmp/ch-pm-agnes-acceptance-20261126.json。
同实例 v4 证据的三条持久模型响应元数据均记录 agnes-3.0-flash；
后续审查的上游 host/model 日志字符串扫描未命中，不能将该扫描写成额外归属证明。
账单、账号权益及零费用 entitlement 仍未核实；本地 cost=0 不是外部零账单证明。
本文不嵌入原始 transcript、密钥、文件内容或其他敏感值。
