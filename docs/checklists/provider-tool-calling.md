# Model API and tool-calling checklist

Live provider calls are not part of CI. Rows marked "httptest" are covered by stdlib httptest servers; rows marked "mock contract" are tested in-process. Do not treat an unchecked row as supported against a live API.

| Capability | OpenAI Responses | Anthropic Messages | OpenAI-compatible Chat Completions | httptest status |
| --- | --- | --- | --- | --- |
| One text completion | Required | Required | Required | Request shape, parsing, and usage covered for all three stdlib clients. Live call not run (no key). |
| Multiple tool calls in one response | Required | Required | Required | Domain batch contract plus provider parsing tests. No live provider. |
| Tool arguments split across stream chunks | Required | Required | Required | Stream tests for all three clients. Responses and Anthropic streams are hand-written fixtures in `internal/provider/testdata/stream`; they stream only when the caller takes text deltas and use non-streaming JSON otherwise. Live stream not run (no key). |
| Text plus tool calls is non-terminal | Required | Required | Required | Provider parsing preserves ordered text and tool-call items. |
| `finish_turn` exclusive | Harness protocol | Harness protocol | Harness protocol | Mock contract. Mixed with another call: zero dispatches. |
| `ask_user` exclusive | Harness protocol | Harness protocol | Harness protocol | Mock contract. |
| Opaque reasoning / thinking items kept in order | Reasoning items | Thinking and redacted thinking, with signature | Not available on this endpoint | Fixture order tests plus httptest continuation replay for Responses and Anthropic. Chat Completions returns `opaque_replay_unsupported` instead of stripping bytes. |
| Signature bytes unchanged across a follow-up | Required | Required | n/a | Anthropic thinking payload and signature are replayed from continuation bytes in httptest. |
| Prefix caching metrics | `cached_input_tokens` when reported | cache fields when reported | Not requested | Usage structs are populated from httptest responses for all three clients. |
| `previous_response_id` vs manual replay | Provider rule | n/a | n/a | Manual replay is used for Responses input items; `previous_response_id` is not implemented. |
| Switching provider drops opaque state | Required | Required | Required | `PrepareContinuation` degrades and removes opaque items. |
| Official Go SDK | Planned | Planned | Not required for the probe | Not imported. Stdlib HTTP only, so SDK types cannot leak into domain. |

## Tool-calling rules the broker already enforces

- Unknown tool, bad JSON, or a missing required field never reaches the executor and does not charge budget.
- A grant for the same tool does not authorize a different resource path.
- Expired grants do not match.
- The `MCPAdapter` interface only discovers and returns tool specs. The current `StdioMCP` runtime starts configured local stdio servers, registers discovered tools on the broker as `mcp.<server>.<tool>`, and supplies each registered spec with a `tools/call` executor; calls are broker-dispatched. The Streamable HTTP adapter uses the same broker path for JSON and SSE responses.

## Not checked against a live model

- Parallel tool calls (V1 is serial).
- Argument streaming and partial JSON for Responses and Anthropic.
- Provider-specific required fields on thinking signatures.
- Prompt-cache hit rate.
- Refusal / safety stop sequences.
