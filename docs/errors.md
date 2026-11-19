# Error codes

Every failure the harness returns is a `domain.Error`: a stable `code`, a short message, and an optional `ref` (run, invocation, or response id). HTTP maps these to 4xx or 409. The CLI prints the same value.

| Code | When |
| --- | --- |
| `protocol_error` | Mixed control calls, a broken tool-call pair, or an incomplete stream |
| `corrupt_state` | A stored row breaks a state invariant |
| `illegal_transition` | The state machine has no edge for that event |
| `budget_exhausted` | A model, tool, script, or child charge would pass the parent cap |
| `auth_denied` | No grant covers the tool and resource |
| `skipped` | A later call in a batch after a denial |
| `provider_not_configured` | OpenAI mode without `OPENAI_API_KEY` |
| `provider_http` | The provider HTTP call failed. Authorization headers are redacted |
| `provider_mismatch` | Opaque continuation belongs to another provider |
| `program_disabled` | Isolation was not established, so the script was not started |
| `script_syntax` | Starlark rejected the script |
| `script_error` | The script ran and returned an error |
| `script_policy` | The allowlist or a side-effect confirmation was missing |
| `resource_limit` | The worker hit the step cap or the CPU limit |
| `worker_crashed` | The worker exited without a result. Unsafe scripts are not replayed |
| `tool_error` | The executor failed after it was entered |
| `invalid_argument` | Arguments or a flag value are not usable |
| `not_found` | The run, session, or history id is absent |
| `conflict` | A version or invariant check failed |
| `lease_conflict` | Another owner holds a live lease |
| `not_waiting` | Resume was asked of a run that is not waiting |
| `not_implemented` | A reserved path that this build refuses |
| `external_bind_refused` | A non-loopback listen without an HTTP token; configure `--http-token`, `CLOUD_HARNESS_HTTP_TOKEN`, or `http_token` (at least 16 characters) |
| `cancelled` | The run context was cancelled |

`stream.delta` events are not errors and are not stored.
