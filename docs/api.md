# HTTP API

`cloud-harness serve` listens on `127.0.0.1:19421` by default. Requests and
responses use JSON, except for the event stream at
`GET /v1/runs/{id}/events` (SSE). Successful responses contain the requested
object or array directly; errors use an `error` object with `code`, `message`,
and an optional `ref`.

## Authentication and listen addresses

Configure an HTTP bearer token with `serve --http-token TOKEN`, the
`CLOUD_HARNESS_HTTP_TOKEN` environment variable, or `http_token` in the JSON
configuration file. A configured token must contain at least 16 characters.
Every `/v1/` route, including `/v1/approvals`, `/v1/runs/{id}`, and its
`/events` stream, uses the same authentication middleware and constant-time
token comparison. `/healthz` is available without authentication.

When a token is configured, supply `Authorization: Bearer <token>`. A missing
or incorrect token returns HTTP 401 with
`WWW-Authenticate: Bearer realm="cloud-harness"` and this JSON body:

```json
{
  "error": {
    "code": "auth_denied",
    "message": "missing or invalid bearer token"
  }
}
```

Loopback addresses (`127.0.0.1`, `localhost`, `::1`, and other IP addresses
classified as loopback by the existing listen validation) can serve without a
token for local development. Every non-loopback address, including `0.0.0.0`,
`[::]`, and an empty host such as `:19421`, requires a token. Without one,
`serve` exits before opening the database or binding the listener:

```text
non-loopback listen requires an HTTP token; configure --http-token, CLOUD_HARNESS_HTTP_TOKEN, or http_token (at least 16 characters)
```

The legacy `--http-allow-insecure`, `http_allow_insecure`,
`CLOUD_HARNESS_HTTP_ALLOW_INSECURE`, and `CLOUD_HARNESS_ALLOW_EXTERNAL` settings
remain accepted for configuration compatibility and cannot waive this token
requirement. See [serve CLI](cli.md#serve) and
[HTTP configuration](config.md#http-authentication-and-non-loopback-rule).

## GET /v1/approvals

Returns the human approval queue across all sessions: runs whose status is
`WAITING` and whose stored wait reason is `APPROVAL` or `USER`. In this queue,
these reasons are serialized as `approval` and `ask_user`. Budget, child, and
reconciliation waits are outside this queue. All matching runs are returned,
without the interactive CLI run list's 200-record limit.

Entries are ordered by `waiting_since` ascending, so the longest current wait
comes first. Equal timestamps are ordered by `run_id` ascending. A run that
resumes and waits again uses the latest persistent `run.WAITING` event as its
waiting start. Subsequent events or changes to `updated_at` do not reset it.
Legacy records with no waiting event fall back to their stored `updated_at`.

| Field | Type | Meaning |
| --- | --- | --- |
| `run_id` | string | Existing run ID to use for inspection or a response. |
| `session_id` | string | Session that owns the run. |
| `status` | string | Always `WAITING`. |
| `wait_reason` | string | `approval` for a tool approval, or `ask_user` for a question. |
| `summary` | string | Same summary as the Manager's WaitInfo and lifecycle notification: the full pending command text for `run_command`, or the original question for `ask_user`. Other tool approvals use `Waiting for approval: <tool>` as in notifications. |
| `waiting_since` | string | Start of the current wait, as an ISO 8601 / RFC 3339 UTC timestamp. |
| `waiting_seconds` | integer | Whole elapsed seconds at request time, rounded down and clamped to zero if the server clock precedes `waiting_since`. |
| `approval_url` | string, optional | `<approval_base_url>/approval?run=<run-id>` when `notify.approval_base_url` is configured. Trailing base slashes are removed and the run ID is query encoded. Omitted when the base is unset. |

Example request:

```bash
curl -H "Authorization: Bearer $CLOUD_HARNESS_HTTP_TOKEN" \
  http://127.0.0.1:19421/v1/approvals
```

Example HTTP 200 response, with `notify.approval_base_url` set to
`https://harness.example`:

```json
[
  {
    "run_id": "run_approval",
    "session_id": "ses_1",
    "status": "WAITING",
    "wait_reason": "approval",
    "summary": "rm -rf -- obsolete-artifacts",
    "waiting_since": "2026-11-19T11:50:00Z",
    "waiting_seconds": 600,
    "approval_url": "https://harness.example/approval?run=run_approval"
  },
  {
    "run_id": "run_question",
    "session_id": "ses_2",
    "status": "WAITING",
    "wait_reason": "ask_user",
    "summary": "Which deployment region should we use?",
    "waiting_since": "2026-11-19T11:58:00Z",
    "waiting_seconds": 120,
    "approval_url": "https://harness.example/approval?run=run_question"
  }
]
```

An empty queue returns HTTP 200 with `[]`. Authentication failures use the
401 response and Bearer challenge above; storage failures follow the existing
HTTP 500 `{"error":{"code":"internal","message":"request failed"}}` format.

This is a read-only endpoint. Inspect the full pending invocation or question
with `GET /v1/runs/{id}`. Submit approval decisions through the existing
`POST /v1/runs/{id}/responses` endpoint (`{"decision":"approve"}` or
`{"decision":"deny"}`); submit user answers through that endpoint or
`POST /v1/runs/{id}/answer` (`{"answer":"..."}`). An approval URL identifies
the separately provided approval page; this API does not serve that page.
