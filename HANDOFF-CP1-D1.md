# CP1-D1：OpenCode 官方免鉴权直连核实

- 核验日期：2026-11-25（上海时间，Asia/Shanghai）
- 官方页面访问时段：约 16:35 CST
- 工作区：`/home/ubuntu/cloud-harness-dev/worktrees/cp1-d-provider`
- 分支：`cp1/d-provider`
- 核验基线：`b81b569c5780766257ee26e9406c615317f551db`（与指定 dev 基线相同）
- 初始状态：工作树干净；当前检查未发现适用的 `AGENTS.md`。
- 范围：D1 只核验官方文档；没有改 provider/config，没有请求任何模型，也没有使用/探测密钥。

## 结论

**D1 闸门不成立，应停止 OpenCode 免费聊天模型的 cloud-harness 免鉴权直连适配。** OpenCode 官方文档明确规定 free-tier chat models 需要 OpenCode；其他客户端使用 paid models 或 Go。Console 文档另要求用户登录、添加账单资料与 credits 并取得 API key，且按请求计费。虽然文档列有 API endpoint 和若干 `Free` 标价模型，但这些事实不能推出 free-tier 模型支持外部客户端免鉴权调用；官方文档反而明确限制了客户端范围。

因此，没有可供本任务采用的“公开、免鉴权、明确零费用、可供第三方 cloud-harness 调用”的官方模型 endpoint/model 组合。不得从通用 paid/Go API endpoint 推断免费模型可匿名访问，也不得通过 OpenCode CLI、借用 key、空/假 key、未文档化路径或收费 fallback 绕过此限制。**本次不提交 provider/config 代码。**

## 官方来源与关键事实

以下均为 OpenCode 官方文档；页面未显示明确发布日期/更新时间，访问核验日为 2026-11-25（上海时间）。

1. [Console models](https://opencode.ai/docs/console/models/) — How it works / Endpoints / Pricing / Free models
   - 原文： “Sign in to the console, add your billing details and credits, and copy your API key.”
   - 原文： “You are charged per request and can add credits to your account.”
   - 原文： “Free-tier chat models require OpenCode; use paid models or Go from other clients.”
   - 模型表确实列出多个 Input、Output 标为 `Free` 的模型，例如 Big Pickle、Tidepool Alpha Free、Iceman Free、Beam Free、Flint 40B Free、Hy4 flash Free、Jev 1.13 Free 等；Free models 段落将若干模型描述为限时免费。此价格信息适用于官方规定的 OpenCode 客户端路径，不构成外部 API 的匿名调用承诺。
   - Endpoints 表列出模型及原生 API。GPT 模型示例 endpoint 为 `https://opencode.ai/inference/openai/v1/responses`；该表与上面外部客户端限制同时阅读，不能把 endpoint 表视作 free-tier 外部访问授权。

2. [Console harnesses](https://opencode.ai/docs/console/harnesses#base-urls) — Get an API key / Base URLs
   - 原文： “Free-tier chat models are only available from within OpenCode. Other harnesses receive HTTP 403 for those models, even with a Console API key. Use paid models or a Go subscription.”
   - Harnesses authenticate with a Console API key; the document instructs users to create one in Console and says usage counts toward the member budget.
   - 原文： “Every API serves every paid and Go text model.” Base URLs 列出 pay-as-you-go `https://opencode.ai/inference` 和 Go `https://opencode.ai/inference/go`，以及 OpenAI Responses、OpenAI Chat Completions、Anthropic Messages、Gemini 等协议路径。这些 API 路径的官方说明范围是付费或 Go 文本模型；没有把免费 chat 模型列为第三方可用 endpoint。

3. [Models: Free models](https://opencode.ai/docs/models#free-models)
   - 原文： “Free means an opencode or opencode-go model with no input price.”
   - 原文： “Signed out, the opencode provider is absent until you connect an OpenCode Console account or set OPENCODE_API_KEY, which restores its paid models.”
   - 该页说明 OpenCode 产品内免费模型的价格/目录语义，并未提供第三方可用的免鉴权 API 规范。作为交叉证据，它仍不推翻 Console models 对其他客户端的显式限制。

## 对 cloud-harness 的判断

- 现有 F02 审查记录已指出 `internal/provider/openai.go`、`internal/provider/openai_responses.go` 和 `internal/provider/anthropic.go` 的 Console/API 调用要求 API key；未发现 OpenCode 专用匿名路径。
- 在基线 `b81b569`，OpenAI Responses 通用协议客户端不能成为 OpenCode 免费模型的合规替代接入方式，因为官方对免费 chat 模型规定客户端仅限 OpenCode。
- 目前官方资料不足以核实免鉴权 endpoint、适用免费模型 ID、外部工具调用能力以及应用侧零费用保证；本轮不发请求，亦不声称工具调用已验证。
- 后续若官方发布明确允许第三方调用的免费 API 契约，可重新做证据闸门；在此之前保持现状，不更改默认鉴权保护、不引入 fallback、不影响 A/B/C。

## 变更与未完成项

- 本分支仅新增本交接文件；不改 `internal/provider/`、`internal/config/`、`go.mod`、`go.sum` 或其他文件。
- 未执行 Go 测试：本次无代码变更，且 D1 仅做官方文档核验。
- 未做真实模型回复/审批工具/重启恢复实验：没有官方可用的外部免钥免费路径，因此该真后端验收前置条件不成立；mock/fixture 不能替代。
- 没有 endpoint 请求、计费试探或凭证探测。
- 后续 D2 可基于以上结论停止本路径；如要更改范围或寻找其他供应商，需另行授权与官方证据核实，不得把本结论解释为 OpenCode 官方提供了匿名访问。
