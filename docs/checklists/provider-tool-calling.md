# Model API and tool-calling checklist

M0 does not call a live provider. Rows marked "mock contract" are tested in-process. Rows marked "not run" wait for a key and for M1's adapters. Do not treat an unchecked row as supported.

| Capability | OpenAI Responses | Anthropic Messages | OpenAI-compatible Chat Completions | M0 status |
| --- | --- | --- | --- | --- |
| One text completion | Required | Required | Required | Chat Completions client tested against `httptest`. Live call not run (no key). |
| Multiple tool calls in one response | Required | Required | Required | Domain batch contract: three calls, ordered, ids paired. No live provider. |
| Tool arguments split across stream chunks | Required | Required | Required | Not run. Incomplete responses are refused and not dispatched. |
| Text plus tool calls is non-terminal | Required | Required | Required | Mock contract. |
| `finish_turn` exclusive | Harness protocol | Harness protocol | Harness protocol | Mock contract. Mixed with another call: zero dispatches. |
| `ask_user` exclusive | Harness protocol | Harness protocol | Harness protocol | Mock contract. |
| Opaque reasoning / thinking items kept in order | Reasoning items | Thinking and redacted thinking, with signature | Not available on this endpoint | Mock contract for storage, replay, and trace exclusion. Chat Completions client returns `opaque_replay_unsupported` instead of stripping bytes. |
| Signature bytes unchanged across a follow-up | Required | Required | n/a | Mock contract. |
| Prefix caching metrics | `cached_input_tokens` when reported | cache fields when reported | Not requested | Usage struct exists. Not collected from a live API. |
| `previous_response_id` vs manual replay | Provider rule | n/a | n/a | Not implemented. M1. |
| Switching provider drops opaque state | Required | Required | Required | `PrepareContinuation` degrades and removes opaque items. |
| Official Go SDK | Planned | Planned | Not required for the probe | Not imported. Stdlib HTTP only, so SDK types cannot leak into domain. |

## Tool-calling rules the broker already enforces

- Unknown tool, bad JSON, or a missing required field never reaches the executor and does not charge budget.
- A grant for the same tool does not authorize a different resource path.
- Expired grants do not match.
- An MCP-shaped adapter can only contribute specs. Invoke still goes through the broker. Nothing dials an MCP server.

## Not checked against a live model

- Parallel tool calls (V1 is serial).
- Argument streaming and partial JSON.
- Provider-specific required fields on thinking signatures.
- Prompt-cache hit rate.
- Refusal / safety stop sequences.
