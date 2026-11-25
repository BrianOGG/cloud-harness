# HANDOFF-CP1-A2

更新时间：2026-11-25 18:20（Asia/Shanghai）

## 当前边界

- 分支：`cp1/a-history`
- 基线：`b81b569c5780766257ee26e9406c615317f551db`
- A2 仅负责 `internal/storage`、`migrations/009_messages.sql` 和存储测试。
- A1 契约提交：`9eba93e7cc393c81bbd9fb34efb639b8f7a363e9`；A3 的 transport/docs 文件未修改。

## 已写入的 WIP

- 增加 `storage.Message`、`storage.MessageCursor`、`storage.MessagePage`，以及 `Store.ListMessages(ctx, sessionID, after, limit)`。
- 增加 `messages` 表和 `(session_id, created_at, id)` 索引。
- 生产写入路径已接入 turn 输入、模型响应、ask_user 问答、审批请求/决定和终态投影；消息读取使用单个读事务和 `limit+1`。
- 当前分页边界为 `(created_at, id)` 严格大于内部 cursor，按 `created_at ASC, id ASC` 排序。
- 已添加跨重启/分页/隔离的存储测试草稿。

## 尚未完成

- 测试文件仍使用旧字符串 cursor，当前 WIP 尚未重新通过编译。
- 尚需把所有非终态消息状态归一为 `recorded`，把终态状态归一为 `recorded/failed/cancelled`，并对齐 `run_status` 与 approval 内容映射。
- 尚需按模型 response item 的 `ordinal` 聚合文本，并仅对 complete、用户可见 response 投影。
- 尚需修正历史回填：同一 turn 多 run 不得生成重复输入，final-answer fallback 只能按来源关联而非内容相等去重。
- 尚需补齐失败/取消/追问/审批前后及长分页/重启回归，并执行 build、vet、gofmt、全量 test 和串行 race。

## 真实检查结果

- 首个实际检查：2026-11-25 15:42（Asia/Shanghai），HEAD/branch/status 核对通过，原始工作区干净。
- 当前实际 `go test ./internal/storage/sqlite` 退出码：`127`，因为宿主 `go` 不在 PATH；仓库 SDK 位于 `/home/ubuntu/cloud-harness-dev/sdk/go/bin`，后续命令必须使用该路径。
- 本文件对应的是未完成 WIP，不是 A1 冻结契约，也不是可供 A3 直接集成的完成交接。
