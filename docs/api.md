# HTTP API

`cloud-harness serve` listens on `127.0.0.1:19421` by default. The `/v1/` API
uses JSON except for the event stream at `GET /v1/runs/{id}/events` (SSE).
Successful JSON responses contain the requested object or array directly.
Errors use an `error` object with `code`, `message`, and an optional `ref`.

## Authentication and listen addresses

Configure an HTTP bearer token with `serve --http-token TOKEN`, the
`CLOUD_HARNESS_HTTP_TOKEN` environment variable, or `http_token` in the JSON
configuration file. A configured token must contain at least 16 characters.
Every `/v1/` route, including `/v1/sessions`, `/v1/approvals`,
`/v1/runs/{id}`, and its `/events` stream, uses the same authentication middleware and constant-time
token comparison. `/healthz`, the static `/approval` page, and `/ui` and its
static resources are available without authentication.

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

## Request checks

These checks run on every route, before authentication, with or without a
token. They stop a web page in the user's browser from driving the API through
a cross-site request or DNS rebinding.

| Check | Applies to | Failure |
| --- | --- | --- |
| `Host` names the listener | Loopback listeners, and any listener with `http_allowed_hosts` set | HTTP 403 `auth_denied` |
| `Origin`, when sent, matches `Host` | Every listener | HTTP 403 `auth_denied` |
| `Content-Type: application/json` | `POST`, `PUT`, `PATCH`, and `DELETE` requests with a body, after authentication | HTTP 415 `invalid_argument` |

- **Host.** A loopback listener accepts `localhost`, any loopback IP address
  (`127.0.0.0/8`, `::1`), and the entries of `http_allowed_hosts`, on any port.
  An entry is `host` (any port) or `host:port` (that port only). Set it with
  `serve --http-allowed-hosts HOST[,HOST]`, `CLOUD_HARNESS_HTTP_ALLOWED_HOSTS`,
  or the JSON `http_allowed_hosts` array when a tunnel or reverse proxy
  forwards another name to the loopback listener, such as the host of
  `notify.approval_base_url`. A non-loopback listener accepts any `Host`
  unless `http_allowed_hosts` is set.
- **Origin.** The host and port of `Origin` must equal the `Host` header. The
  scheme is not compared, so a TLS proxy in front of the listener works.
  `Origin: null` is refused. Requests without `Origin`, such as `curl` and
  other non-browser clients, are not affected. The API sends no CORS headers.
- **Content-Type.** Parameters such as `charset` are accepted. A write request
  without a body, such as `POST /v1/sessions`, needs no `Content-Type`.

```json
{
  "error": {
    "code": "auth_denied",
    "message": "cross-origin request refused"
  }
}
```

## Timeouts and shutdown

`serve` allows 10 seconds to read request headers, 30 seconds to read a whole
request including its body, and 60 seconds to handle a request and write its
response. A connection that exceeds a limit is closed. Idle keep-alive
connections close after 120 seconds.

Two kinds of request have no total time limit. `GET /v1/runs/{id}/events`
stays open, and each event or heartbeat write gets its own 60 seconds. A
request that executes a run (`POST /v1/sessions/{id}/runs`, and the
`/responses`, `/answer`, `/budget`, and `/cancel` routes of a run) is held
open until the run stops, then has 60 seconds to write its response.

`SIGINT` and `SIGTERM` both stop `serve` gracefully: it stops accepting
connections, waits up to 1 second for requests in flight, closes the
remaining connections, and closes the store.

## GET /approval

Open `/approval` to see the waiting queue or `/approval?run=<run-id>` to open
a specific run. Set `notify.approval_base_url` to the `serve` URL your phone
can reach to include the run link in waiting notifications. The link carries
only the run ID. If a reverse proxy adds a path prefix, expose both `/approval`
and `/v1/` under that prefix.

The embedded page returns `Content-Type: text/html; charset=utf-8` and
`Cache-Control: no-store`. Anyone can load the same static HTML, even if the
run ID is unknown. Run data is loaded through the authenticated API. Paste the
configured HTTP token once for this browser and origin. The page stores it in
`localStorage` and sends it in the `Authorization: Bearer <token>` header with
every API request, including the event stream. A 401 shows the token prompt
and the API error. Clear the site's browser data to remove the stored token.

1. `GET /v1/approvals` lists pending items and provides their summaries and wait times.
2. `GET /v1/runs/{id}` loads the selected run. `GET /v1/runs/{id}/events`
   follows updates so the page can show the current wait or final status.
3. `POST /v1/runs/{id}/responses` sends `{"decision":"approve"}` or
   `{"decision":"deny"}`. `POST /v1/runs/{id}/answer` sends `{"answer":"..."}`
   for `ask_user`.

## GET /ui

Open `http://127.0.0.1:19421/ui` on the default `serve` listener to start
sessions, send messages, inspect runs, events, and traces, and view serve
statistics and doctor checks. The page and its assets under `/ui/` are public
and contain no run data. Navigation to an
app route under `/ui/` returns the same `index.html` with `Cache-Control: no-store`.

The browser calls the real `/v1/` API on the same origin. If the server
requires a token, paste it on `/approval` on that origin. The Web UI reads
`cloud-harness.http-token` from `localStorage` and sends `Authorization: Bearer <token>`
with API requests, including the event stream. Without a token, the page still
opens and protected API requests return 401. A reverse proxy serving the Web UI
should forward `/ui`, `/ui/`, and `/v1/` on the same origin.

To rebuild the assets, run `(cd web && npm ci && npm run build)` with Node.js
22 (22.12 or later). This writes `web/dist/`, which is checked in so a Go build
can embed the UI into the executable. Rebuild the Go binary to serve updated
assets.

CI rebuilds the assets and fails when `web/dist/` differs from the result, so
commit the rebuilt `web/dist/` with the source change. Run
`(cd web && npm run check:dist)` to make the same comparison locally. See
[web/README.md](../web/README.md).

## GET /v1/stats and GET /v1/doctor

The Web UI observation page reads these authenticated, read-only endpoints from
the running serve. Both use the same bearer authentication as other `/v1/`
routes. `GET /v1/stats` counts retained sessions, runs (including child runs),
persisted events, active runs (`RUNNING` or `WAITING`), and failed runs. Model
response counts, token totals, cache hit rate, and estimated USD cost cover the
last 30 days. The budget totals sum the used and maximum model/tool calls for
root runs. The response also includes retained `allow`/`ask`/`deny` decisions,
hook counts, and the 12 most recent failed runs, hook warnings, or failed tool
calls. Cost is `null` if there is no recorded usage or model prices do not cover
all of the recorded usage.

```json
{
  "sessions": 4,
  "runs": 5,
  "events": 27,
  "active_runs": 2,
  "failed_runs": 1,
  "model_calls": 8,
  "usage": {
    "input_tokens": 256,
    "output_tokens": 96,
    "cached_tokens": 0,
    "cache_hit_rate": 0,
    "cost": null
  },
  "budget": {"model_used": 8, "model_max": 100, "tool_used": 3, "tool_max": 100},
  "policy": {"allow": 2, "ask": 1, "deny": 0},
  "hooks": {"total": 6, "warnings": 0, "blocked": 0},
  "recent_errors": [
    {
      "run_id": "run_123",
      "session_id": "ses_456",
      "type": "run.failed",
      "message": "Model request failed",
      "at": "2026-11-19T18:30:00Z"
    }
  ]
}
```

`GET /v1/doctor` returns the current environment checks, one `overall` summary
with failure and warning counts, and a `failed_runs` check for retained run
failures. Each item has `name`, `status` (`ok`, `warn`, or `fail`), and a
`detail`. The `failed_runs` check includes `run_id` for the latest recorded
failed run. A completed diagnosis returns HTTP 200 even when a check fails.

## GET /v1/sessions

Lists all sessions as a JSON array. Sessions are ordered by `updated_at`
descending, with `id` ascending as a stable tie-breaker. Each session includes
aggregate information computed by the store in the same listing query.

| Field | Type | Meaning |
| --- | --- | --- |
| `id` | string | Session ID. |
| `created_at` | string | Session creation time in UTC RFC 3339 format. |
| `updated_at` | string | Stored session update timestamp in UTC RFC 3339 format. |
| `run_count` | integer | Total number of runs in the session. |
| `latest_run_status` | string, optional | Status of the newest run by `created_at`; omitted when there are no runs. |
| `latest_run_at` | string, optional | Creation time of the newest run; omitted when there are no runs. |

Example request:

```bash
curl -H "Authorization: Bearer $CLOUD_HARNESS_HTTP_TOKEN" \
  http://127.0.0.1:19421/v1/sessions
```

Example HTTP 200 response:

```json
[
  {
    "id": "ses_2",
    "created_at": "2026-11-19T11:00:00Z",
    "updated_at": "2026-11-19T12:00:00Z",
    "run_count": 2,
    "latest_run_status": "COMPLETED",
    "latest_run_at": "2026-11-19T11:59:00Z"
  },
  {
    "id": "ses_1",
    "created_at": "2026-11-19T10:00:00Z",
    "updated_at": "2026-11-19T10:00:00Z",
    "run_count": 0
  }
]
```

An empty database returns HTTP 200 with `[]`.

## GET /v1/sessions/{id}/runs

Lists all runs for the requested session as a JSON array, ordered by
`created_at` ascending (with `id` ascending to order equal timestamps). Each
entry has exactly the same fields as the run view from `GET /v1/runs/{id}`.

| Field | Type | Meaning |
| --- | --- | --- |
| `id` | string | Run ID. |
| `session_id` | string | Owning session ID. |
| `status` | string | Current run status. |
| `wait_reason` | string, optional | Reason the run is waiting. |
| `wait_kind` | string, optional | Wait kind, matching `wait_reason` when present. |
| `question` | string, optional | Pending user question for an `ask_user` wait. |
| `pending_invocation` | object, optional | Invocation awaiting approval, with the same structure as the single-run view. |
| `budget` | object | Model, tool, script, and child-run limits and usage counters. |
| `final_answer` | string, optional | Final answer for a completed run. |
| `error` | object, optional | Error details for a failed run. |
| `version` | integer | Run version. |

Example request:

```bash
curl -H "Authorization: Bearer $CLOUD_HARNESS_HTTP_TOKEN" \
  http://127.0.0.1:19421/v1/sessions/ses_2/runs
```

Example HTTP 200 response:

```json
[
  {
    "id": "run_1",
    "session_id": "ses_2",
    "status": "COMPLETED",
    "budget": {
      "max_model_calls": 100, "used_model_calls": 1,
      "max_tool_calls": 300, "used_tool_calls": 0,
      "max_script_calls": 50, "used_script_calls": 0,
      "max_child_runs": 4, "used_child_runs": 0
    },
    "version": 3,
    "final_answer": "Deployment complete."
  },
  {
    "id": "run_2",
    "session_id": "ses_2",
    "status": "WAITING",
    "wait_reason": "USER",
    "wait_kind": "USER",
    "question": "Which region should I use?",
    "budget": {
      "max_model_calls": 100, "used_model_calls": 1,
      "max_tool_calls": 300, "used_tool_calls": 0,
      "max_script_calls": 50, "used_script_calls": 0,
      "max_child_runs": 4, "used_child_runs": 0
    },
    "version": 2
  }
]
```

An existing session with no runs returns HTTP 200 with `[]`. An unknown session
returns HTTP 404 with the standard `not_found` error envelope. Both endpoints
use the `/v1/` bearer authentication described above.

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
`POST /v1/runs/{id}/answer` (`{"answer":"..."}`). Open the `approval_url` in
a browser to handle the wait on the embedded approval page.
