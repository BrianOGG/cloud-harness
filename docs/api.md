# HTTP API

`cloud-harness serve` listens on `127.0.0.1:19421` by default. The `/v1/` API
uses JSON for data, SSE for `GET /v1/runs/{id}/events`, and a browser page for the MCP OAuth callback.
Successful JSON responses contain the requested object or array directly.
Errors use an `error` object with `code`, `message`, and an optional `ref`.

## Authentication and listen addresses

Configure an HTTP bearer token with `serve --http-token TOKEN`, the
`CLOUD_HARNESS_HTTP_TOKEN` environment variable, or `http_token` in the JSON
configuration file. A configured token must contain at least 16 characters.
API routes under `/v1/`, including `/v1/sessions`, `/v1/approvals`,
`/v1/runs/{id}`, and its `/events` stream, uses the same authentication middleware and constant-time
token comparison. The browser redirect at `GET /v1/mcp/oauth/callback` uses a one-time OAuth state and matching HttpOnly cookie, together with a PKCE code exchange. `/healthz`, the legacy `/approval` redirect, and `/ui` and its
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
request that executes a run and waits for it (`POST /v1/sessions/{id}/runs`,
or the `/responses`, `/answer`, or `/budget` route of a run, called with
`?wait=1`) and `/cancel` are held open until the run stops, then have 60
seconds to write the response.

`SIGINT` and `SIGTERM` both stop `serve` gracefully. It refuses new runs and
cancels every run that is executing, all at the same time. A run that is
waiting on a child run it delegated to counts as executing, and its child is
cancelled with it. Each of those runs is recorded as `CANCELLED` with a
`run.cancelled` event, so none is left `RUNNING`, and open event streams
receive that event. `serve` then waits up to 5 seconds for the cancelled runs
to unwind. A run that is `WAITING` for approval, an answer, or budget is not
touched and can be resumed after a restart.

Cancelling a run executes its `Stop` hooks before the run is recorded, as
`POST /v1/runs/{id}/cancel` does. Shutdown waits for those hooks even past the
5 seconds, so a hook's own timeout (10 seconds unless `timeout_ms` is set) is
the longest a run can hold shutdown.
`serve` then stops accepting connections, waits up to 1 second for requests in
flight, closes the remaining connections, and closes the store.

## Running a run: POST /v1/sessions/{id}/runs

Optional `model_id`, `approval_mode` and `reasoning_effort` select this run's model, approval behavior and thinking level. Omitted values use the saved
session preferences, initially `default`, `ask` and `default`. Explicit values are validated and persisted for later
runs. A run's checkpoint freezes the resolved model name, provider ID and
approval mode and reasoning effort; changing session preferences does not change a waiting run.

The body is `{"message":"..."}`. The response is HTTP 201 with the run view
described under [GET /v1/sessions/{id}/runs](#get-v1sessionsidruns).

To invoke a skill, send `{"skill":"review","message":"Check the new handler"}`.
The server loads the named file with the CLI skill loader and supplies it as
user context for this run. With a skill, the message is its task argument and
may be empty. The recorded user input is `/skill review Check the new handler`.
A missing, invalid or unreadable skill returns HTTP 400 before run creation.

A run executes in the server, not in the request that created it. Closing the
connection does not stop the run: it keeps executing and its result is read
with `GET /v1/runs/{id}` or the event stream. The only way to stop a run from
a client is `POST /v1/runs/{id}/cancel`.

The request answers as soon as the run is recorded and has started, so the
`status` in the response is usually `RUNNING`. Follow the run on its event
stream, or read `GET /v1/runs/{id}` until it stops.

```bash
curl -s -X POST -H "Content-Type: application/json" \
  -H "Authorization: Bearer $CLOUD_HARNESS_HTTP_TOKEN" \
  -d '{"message":"summarize the README"}' \
  http://127.0.0.1:19421/v1/sessions/ses_2/runs
```

```json
{
  "id": "run_3",
  "session_id": "ses_2",
  "status": "RUNNING",
  "budget": {
    "max_model_calls": 100, "used_model_calls": 0,
    "max_tool_calls": 300, "used_tool_calls": 0,
    "max_script_calls": 50, "used_script_calls": 0,
    "max_child_runs": 4, "used_child_runs": 0,
    "max_tokens": 0, "used_input_tokens": 0, "used_output_tokens": 0,
    "used_cached_input_tokens": 0, "used_cache_creation_input_tokens": 0,
    "used_reasoning_tokens": 0, "cache_reported": false, "reasoning_reported": false,
    "max_cost_usd": 0, "used_cost_usd": 0
  },
  "version": 1
}
```

The `budget` returned by the single-run endpoint and run snapshots includes
`used_cache_creation_input_tokens` and `used_reasoning_tokens`. Input includes
cache hits and cache writes; reasoning is included in output, so the limit
still applies only to input plus output. `cache_reported` and
`reasoning_reported` are cumulative: any call reporting an optional field
(even zero) sets its marker to true. Missing markers on old JSON mean not
reported; a positive legacy cached-input value can still be displayed.
Unknown optional counts should be hidden; reported zero counts can be shown.

The `wait` query parameter chooses when the request answers:

| Request | Answers when | `status` in the response |
| --- | --- | --- |
| `?wait=0`, or no `wait` | the run is recorded and has started | usually `RUNNING` |
| `?wait=1` | the run has stopped executing | `COMPLETED`, `FAILED`, `CANCELLED`, or `WAITING` |

Any other value of `wait` returns HTTP 400 and creates no run.

`POST /v1/runs/{id}/responses`, `/answer`, and `/budget` resume a waiting run
and take the same `wait` parameter with the same default. They return HTTP 200.

`/budget` takes `extra_model_calls`, `extra_tokens`, and `extra_cost_usd`, at
least one of them positive; `/responses` accepts the same three fields. See
[run token and cost limits](config.md#run-token-and-cost-limits).

**Changed in M13-2.** These four requests used to answer only once the run had
stopped, with the final answer or the wait in the response. A client that
relies on that adds `?wait=1`:

```bash
curl -s -X POST -H "Content-Type: application/json" \
  -H "Authorization: Bearer $CLOUD_HARNESS_HTTP_TOKEN" \
  -d '{"message":"summarize the README"}' \
  'http://127.0.0.1:19421/v1/sessions/ses_2/runs?wait=1'
```

## GET /v1/runs/{id}/events

A `text/event-stream` response. It first replays the run's persisted events,
then stays open and sends new ones as they are committed. It ends once the run
is `COMPLETED`, `FAILED`, or `CANCELLED` and `serve` has finished executing
it. A run records events after its status turns terminal, such as
`turn.summary` and `worktree.summary`, and those arrive on the same connection
before it closes. For a run that another process is executing against the
same database, the stream ends when it first sees the terminal status; send
`Last-Event-ID` on a new request to read what that process wrote afterwards.
For a run that is `WAITING` the stream stays open, and continues when the run
is resumed.
A `: ping` comment is sent while nothing else is.

The stream carries two kinds of event:

| Kind | `id:` line | Stored | Examples |
| --- | --- | --- | --- |
| Persisted event | the event's sequence number | yes | `run.created`, `model.completed`, `tool.batch`, `run.completed` |
| Live text | none | no | `stream.delta`, `stream.reset` |

**Persisted events** are the record of what the run did. Send the last `id`
you received as the `Last-Event-ID` header to resume after it; every later
event is delivered exactly once, in order. They report that a model response
or the run completed, not the text itself: `run.completed` carries the status,
and the answer is the `final_answer` of `GET /v1/runs/{id}`. A client that
missed live text therefore loses nothing it needs. After a reconnect it reads
the remaining events, and when the run has stopped it reads the run view.

**Live text** is the model response in progress, sent as it is generated:

- `stream.delta` has the data `{"text":"..."}`, the next piece of the
  response. Pieces arrive in the order the model produced them; concatenate
  them. A `model.completed` event marks the end of the response, and the next
  `stream.delta` after it starts a new one. The text of the last response of
  a completed run is its `final_answer`.
- `stream.reset` means the text sent for the current response will not appear
  in any persisted event, because the response failed or the connection fell
  too far behind. Discard what you have shown for it.

Live text has no cursor and is never replayed from the store. A new connection
made while a response is in progress starts with one `stream.delta` holding
that response's text so far, so a client that reconnects must drop the partial
text it was showing rather than append to it. Text from a child run is sent on
the child's stream, not its parent's.

```bash
curl -N -H "Authorization: Bearer $CLOUD_HARNESS_HTTP_TOKEN" \
  -H "Last-Event-ID: 4" http://127.0.0.1:19421/v1/runs/run_3/events
```

```text
event: stream.delta
data: {"text":"The README "}

event: stream.delta
data: {"text":"describes"}

id: 5
event: model.completed
data: {...}

id: 6
event: run.completed
data: {"status":"COMPLETED"}
```

Subscribers are woken when an event is committed by this `serve` process. As
a fallback each open stream also reads the store every 2 seconds, which is how
it learns of events written by another process, such as
`cloud-harness runs resume` against the same database.

## GET /v1/runs/{id}/trace

Returns a read-only `{session_id, run}` execution trace. `run.steps` contains
model response identities and their ordered `batches`; each batch has its
ordered `calls`. A call contains `id`, `provider_call_id`, `name`, `status`,
`ordinal`, optional `arguments`, and optional `result`. `arguments` is the
persisted tool argument JSON, including the command and requested working
directory when applicable. `result` is the paired persisted
`{ok, data?, error?}`; `error` has `code`, `message`, and optional `ref`. This
preserves a denied invocation's recorded error rather than inferring its
outcome from a preceding approval request. A call without a result omits it;
clients must not invent a successful result from an approval decision.

Use this endpoint when expanding a turn's process to inspect command
parameters, tool output, or recorded denial errors. Invocation status events
do not carry these full arguments and results. The trace does not include
provider opaque bytes, continuation records, or run checkpoints.

## Run file changes and restore

`GET /v1/runs/{id}/changes` returns `{terminal, incomplete, batches}`. Each batch contains its number, immutable `before` and `after` snapshot IDs, and `files: [{path, status}]` with Git name-status codes. The inspector compares a batch’s pre-dispatch capture with its last recorded post-dispatch capture. Changes made between batches or after the last capture do not enter the diff. An empty list means there is no reviewable pair with changes. Missing pairs and known overlaps with other captures in the workspace are omitted and set `incomplete`. As with CLI snapshots, edits by external programs during a capture interval can appear in that interval.

`GET /v1/runs/{id}/changes/diff?before=…&after=…&path=…` returns `{diff, truncated}` for one listed path. URL-encode all three parameters. Git renders text as a unified diff and binary edits as a binary difference. The response retains the first 64 KiB of patch output.

`POST /v1/runs/{id}/changes/restore` accepts a confirmed selection:

```json
{"before":"snap_pre","after":"snap_post","path":"src/example.go","confirm":true}
```

The operation restores that file’s pre-batch contents, type and owner execute bit. Git does not store full Unix permissions. Reviewed regular files are restored owner-only, retaining existing owner read/write restrictions on a replacement. A deleted regular file is recreated with mode `0600` or `0700`. An addition is removed. Success returns `{"safety_snapshot":"snap_…"}`. The inspector refreshes the historical review and displays this receipt. The safety snapshot works with the existing code-only CLI rewind, using the run’s actual workspace and database.

Restore requires a terminal run, the original workspace or retained worktree, and the exact current batch pair. The server checks the live file against the saved post-batch image and preserves unrelated working files, the real Git index and chat. A missing confirmation returns 400. An unknown run or unlisted path returns 404. A stale pair, edited or staged target, merge conflict, busy workspace, or finalized worktree returns 409. File-to-directory transitions require manual resolution. HTTP restores and CLI worktree lifecycle operations coordinate through the run database. External editors are outside that lock.

## GET /approval (legacy redirect)

Old `/approval?run=<run-id>` links return **303 See Other** to
`/ui?run=<run-id>`; `/approval` redirects to `/ui`. The redirect is public,
uses `Cache-Control: no-store`, and contains no run data or credential.
There is no separate approval product page. New notifications use the
conversation UI directly. The authenticated `/v1/approvals` read API remains
available to clients.

## GET /ui

Open `http://127.0.0.1:19421/ui` on the default `serve` listener to start
sessions, send messages, inspect runs, events, and traces, and view serve
statistics and doctor checks. The page and its assets under `/ui/` are public
and contain no run data. Navigation to an
app route under `/ui/` returns the same `index.html` with `Cache-Control: no-store`.

The browser calls the real `/v1/` API on the same origin. If the server
requires a token, configure it for that origin in browser storage. The Web UI reads
`cloud-harness.http-token` from `localStorage` and sends `Authorization: Bearer <token>`
with API requests, including the event stream. Without a token, the page still
opens and protected API requests return 401. A reverse proxy serving the Web UI
should forward `/ui`, `/ui/`, and `/v1/` on the same origin.

Notification links use `/ui?run=<run-id>` to open the run's session and scroll
to its in-conversation WaitCard. `/ui?session=<session-id>` opens a session.
Approve/deny remain in that WaitCard; the sidebar offers conversations and
observability. Work details and uncommitted live drafts are collapsed by
default, including during active runs. The saved response transcript remains
visible independently.

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
| `turn_id` | string, optional | Persisted turn identity; use with `parent_id` to group root and child runs without inferring relationships from message text. |
| `parent_id` | string, optional | ID of the run that delegated this child run. Absent for a run started by a message. |
| `reasoning_effort` | string, optional | Thinking level frozen in the run checkpoint: `default`, `off`, `low`, `medium`, `high`. |
| `approval_mode` | string, optional | Actual `ask`, `safe`, `workspace-write`, or `full-auto` mode frozen in this run's checkpoint. It is independent of the session's next-turn preference. Omitted when a legacy or unavailable checkpoint cannot establish the mode; clients must not substitute current session preferences. |
| `workspace` | string, optional | Execution workspace root from this run's checkpoint policy. A command's optional `pending_invocation.arguments.cwd` is relative to this workspace (or an absolute path subject to the existing boundary check). Omitted when no checkpoint policy is available. |
| `status` | string | Current run status. |
| `wait_reason` | string, optional | Reason the run is waiting. |
| `wait_kind` | string, optional | Wait kind, matching `wait_reason` when present. |
| `question` | string, optional | Pending user question for an `ask_user` wait. |
| `pending_invocation` | object, optional | Invocation awaiting approval, with the same structure as the single-run view. Its optional `warnings` string array is set when the write targets `AGENTS.md` or `.agents/**`, which changes later runs; clients must show it next to the decision. See [approval warnings](cli.md#approval-warnings-for-behavior-files). |
| `budget` | object | Model, tool, script, and child-run limits and usage counters, plus the token and estimated-cost limits and usage (`max_tokens`, `used_input_tokens`, `used_output_tokens`, `used_cached_input_tokens`, `used_cache_creation_input_tokens`, `used_reasoning_tokens`, `cache_reported`, `reasoning_reported`, `max_cost_usd`, `used_cost_usd`). Input includes cache reads and writes; reasoning is included in output. A limit of `0` on tokens or cost is unlimited. |
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

## GET /v1/sessions/{id}/messages

**CP-1 frozen contract; implementation target, not a rollout claim.** This is
new functionality relative to baseline
`b81b569c5780766257ee26e9406c615317f551db`. That baseline has no session
message endpoint or storage pagination interface. This specification does not
assert that the handler, route registration, migrations, or persistence paths
are integrated or accepted on `dev`. Those require separate implementation
and backend verification. Existing authentication, request checks, run APIs,
and SSE behavior remain unchanged.

The endpoint reads the retained, durable, user-visible transcript across all
runs and turns of the session. It uses the same `/v1/` bearer authentication
and outer Host/Origin checks; it is not a public exception. A child run is
included only when its own `session_id` equals the requested session, not
merely because its parent belongs to that session. This is a read-only query:
it must not execute, resume, retry, or materialize missing messages on a GET.

### Page and message fields

HTTP 200 returns one JSON object, not a bare array or a run view:

| Page field | Type | Meaning |
| --- | --- | --- |
| `messages` | array of message objects | At most `limit` records in ascending transcript order. Always an array, including `[]`, never `null`. |
| `next_cursor` | string | Exclusive continuation after the last returned record if another record exists in the same read. Otherwise `""`; never omitted or `null`. |

There is no `total`, `has_more`, or embedded run snapshot. Every message has
all of these fields, including explicit null associations and an empty event:

| Message field | Type | Meaning |
| --- | --- | --- |
| `id` | nonempty ASCII string | Stable, source-derived message identity, independent of page size, position, current run status, or process lifetime. |
| `session_id` | nonempty string | Exactly the session ID in the request path. |
| `run_id` | string or `null` | Run that produced the fact; `null` only when no durable run association exists. Never `""` or omitted. |
| `turn_id` | string or `null` | Owning turn from the durable source/run link; `null` only when no such link exists. Never `""` or omitted. |
| `role` | string | One of `user`, `assistant`, `system`, as defined below; never an inferred model-provider role. |
| `kind` | string | One of the seven transcript kinds in the source table below. |
| `status` | string | `recorded`, `failed`, or `cancelled`; the status of this durable fact, not the run's current status. |
| `content` | string | User-visible text, with original text and line breaks preserved. Approval decisions use the exact string `approve` or `deny`. No HTML rendering is implied. |
| `event` | string | Exact persisted event type when an event is the source, otherwise `""`. Not an SSE ID or sequence number, and not a newly invented event name. |
| `created_at` | string | Immutable stored fact time, normalized to UTC as described below. Never computed from the GET time. |

`run_id` and `turn_id` refer to the source's own run/turn, not the latest run
or the parent of a child run. A non-null run must belong to `session_id`; a
non-null turn must belong to that session and agree with the run's turn link
when that link exists. A turn's initial input belongs to its originating root
run, not every later attempt; a legacy input with no unambiguous originating
run keeps `run_id: null`. A standalone retained turn can therefore have a
non-null `turn_id` and null `run_id`. A run with no turn has a null `turn_id`.
Absent associations are not fabricated, and internal empty strings are
converted to JSON null. Internal projection sequence counters are not fields
of this API and do not determine order.

### Durable sources, identity, and status

The following is the complete CP-1 kind/role mapping. Source IDs are the
already persisted IDs, not newly generated IDs on each projection or read.
Run and invocation IDs retain their distinct source namespaces in
`msg_answer_...` identities.

| Durable source | Stable message ID | `role` / `kind` | `status` and content |
| --- | --- | --- | --- |
| Turn's original user input | `msg_user_<turn-id>` | `user` / `input` | `recorded`; the stored original input, not an expanded prompt or a summary. |
| Committed, complete, user-visible model response | `msg_response_<response-id>` | `assistant` / `response` | `recorded`; concatenate its visible text items in stored ordinal order, without adding separators. No text record is made for a response without visible text. |
| Run's persisted final answer not represented by a linked visible response | `msg_answer_<run-id>` | `assistant` / `response` | `recorded`; the final-answer text once, as a fallback, not a second copy of a linked response. |
| Persisted `ask_user` invocation | `msg_question_<invocation-id>` | `assistant` / `question` | `recorded`; the original stored question. |
| Accepted, persisted answer to that invocation | `msg_answer_<invocation-id>` | `user` / `answer` | `recorded`; the accepted answer, not an HTTP attempt or a rejected answer. |
| Persisted approval request | `msg_approval_<approval-id>_requested` | `system` / `approval_requested` | `recorded`; the user-visible approval summary, not raw tool arguments. |
| Persisted terminal human approval decision | `msg_approval_<approval-id>_decision` | `system` / `approval_decision` | `recorded` for both approval and denial; content is `approve` or `deny`. A pending request has no decision record. |
| Persisted terminal run state | `msg_status_<run-id>` | `system` / `run_status` | `recorded` for `COMPLETED`, `failed` for `FAILED`, `cancelled` for `CANCELLED`. Content is the stored user-visible failure reason when available, otherwise the exact terminal run status. |

Only the `run_status` kind may have `failed` or `cancelled` status. All other
kinds have `recorded` status, including an approval denial or an unanswered
question. A recorded assistant response does not become failed when its run
later fails, and input does not become cancelled when the run is cancelled.
`RUNNING` and `WAITING` are run states, never message statuses: read the run
API for the current wait, pending approval, or progress. A question record
does not itself assert that the question is still pending. Completion emits
one terminal-state fact in addition to any response, but never copies the
final answer into that fact. Preserve the actual stored event spelling in
`event` (including legacy case); determine terminal status from the durable
run state, not by guessing from the spelling.

Within a source, the ID and ordering timestamp are immutable once persisted.
The projection must enforce one record per listed source identity. Replaying
an event, retrying a persistence transaction, re-running an idempotent answer
or decision, rebuilding a projection, or restarting the server must not mint
a new message ID or duplicate that fact. A source fact and its projection
must become visible atomically; an API read must not acknowledge a source
commit while silently omitting its required transcript record. Historical
backfill runs before the endpoint is made available, not as a read-time union
of all runs and events.

Deduplicate by source identity/linkage, never by content equality: identical
text in two turns, two responses, or distinct accepted attempts remains two
records. The run final-answer field, its terminal event, and the response that
produced that answer must not each become another assistant response. A
retained linked response is authoritative over the final-answer fallback.
Choose and persist that provenance at source commit or initial backfill;
later pruning a linked response must not manufacture a fallback with a new
ID. An `ask_user`
question/answer and an approval request/decision are each represented once
from their source, not again from a replayed event or raw tool payload.

Resuming a `WAITING` run retains its run ID and earlier messages. A genuinely
new retry run/response/invocation has its own source IDs and therefore new
records, even when it repeats earlier text. A retry sharing an existing turn
does not duplicate that turn's original input. Failed and cancelled runs
retain their committed input, responses, questions, accepted answers, and
approval facts plus their terminal-state record. A failed provider attempt
without a complete committed response has no assistant response to invent.
Uncommitted or partial generated text is not a durable response.

### Time, order, and completeness boundary

`created_at` is stored when the fact is committed and retains the source's
original fact time: turn creation for input, response creation for text,
invocation/question creation for a question, and acceptance/decision/terminal
transition time for the corresponding later facts. It is not the latest
mutable `run.updated_at`. When an older retained source has no per-fact time,
backfill captures its available persisted timestamp once: approval creation
for a request, response creation for a question, and the owning run's stored
`updated_at` for an answer, approval decision, final-answer fallback, or
terminal state. That legacy fallback is deterministic but is not evidence of
the exact original decision time. Missing legacy content or links must not
be invented to improve the apparent transcript.

History timestamps, including the timestamp inside a cursor, use the fixed
UTC RFC 3339 form `YYYY-MM-DDTHH:mm:ss.nnnnnnnnnZ`, for example
`2026-11-25T09:00:01.000000000Z`. Normalize source offsets to UTC and preserve
nanoseconds; do not round to a client clock, omit the fractional part, or use
an offset such as `+08:00` on this endpoint. This matches the store's fixed
nine-digit timestamp encoding, so textual key order is time order.

Sort the entire session by `(created_at ASC, id ASC)`, with bytewise ASCII ID
comparison for equal timestamps. Do not group by run or turn, reverse a page,
use an unstable offset, or use run update time. Equal timestamps can order
facts differently from causal order; IDs break ties, not prove causality.
Across pages the cursor excludes its boundary tuple strictly.

Completeness means all eligible facts whose durable sources are still
retained, not an unlimited historical archive. Compaction summaries, session
memory, search/history indexes, internal `COMPACT`/`PLAN`/`ORACLE` responses,
provider opaque data, hidden prompts, raw tool traces, temporary SSE
`stream.delta`/`stream.reset` text, and fixture/demo data are not transcript
messages. A summary cannot stand in for missing original text. Retention,
rewind, or source deletion must remove the corresponding projection records
consistently; the endpoint must not expose ghost messages from removed
sources. Migration cannot restore text that was never committed or was
already pruned. Clients must not describe a retained or legacy-backfilled
transcript as a lossless record of everything ever generated.

### Query parameters and cursor v1

| Parameter | Omitted | Accepted value | Rejected value |
| --- | --- | --- | --- |
| `limit` | `50` | One ASCII decimal integer from `1` through `200`, written without a sign, leading zeros, or whitespace. | Empty, `0`, negative, greater than `200`, non-integer, overflow, or repeated, even when repeated values are equal. |
| `cursor` | Start at the oldest retained message. | One empty string (also the first page), or one cursor v1 value below. | Repeated, whitespace-only, malformed, unsupported-version, or wrong-session cursor. |

The optional `sync` mode described below has its own response shape.
Decode query escaping once using normal URL query rules. Other query keys
are ignored; they do not filter runs/turns or change the ordering. In
particular `limit=0` is not the omitted default. A new page may change the
limit while continuing the same cursor.

A nonempty cursor is unpadded RFC 4648 base64url of one UTF-8 JSON object.
The wire alphabet is `[A-Za-z0-9_-]`; reject padding, whitespace, ordinary
base64 `+`/`/`, invalid trailing bits, and invalid UTF-8. The JSON has exactly
four members, each once: integer `v` equal to `1`, nonempty string
`session_id`, canonical history timestamp string `created_at`, and nonempty
ASCII message ID string `id`. Reject extra or missing members, duplicate JSON
keys, nulls, wrong types, invalid dates, or trailing JSON. Object member order
and insignificant JSON whitespace need not be canonical on input. Encoders
emit the members in the following order with compact JSON:

```json
{"v":1,"session_id":"ses_demo","created_at":"2026-11-25T09:00:01.000000000Z","id":"msg_response_rsp_demo"}
```

This synthetic cursor encodes exactly as:

```text
eyJ2IjoxLCJzZXNzaW9uX2lkIjoic2VzX2RlbW8iLCJjcmVhdGVkX2F0IjoiMjAyNi0xMS0yNVQwOTowMDowMS4wMDAwMDAwMDBaIiwiaWQiOiJtc2dfcmVzcG9uc2VfcnNwX2RlbW8ifQ
```

Require the decoded `session_id` to equal the path ID, including case. A
cursor from another session is HTTP 400 `invalid_argument`, not an empty
page. The cursor is a continuation boundary, not an authorization credential
or a signed snapshot; the same bearer and session lookup checks still apply. It has no
time-based expiry, and restarts do not invalidate it. An otherwise valid
cursor whose boundary ID is absent, unknown, or deleted is still accepted as
a logical boundary; do not require an anchor-row lookup or return 404 for it.

Return up to `limit` records with keys strictly greater than the cursor key.
Determine the records and whether another record exists from one consistent
committed read. A nonempty `next_cursor` encodes the last returned record,
never the extra look-ahead record. Fewer than `limit` records, exactly `limit`
with no further record, or no record all produce `next_cursor: ""`.

Each page is consistent, but the cursor does not freeze a snapshot across
requests. Concurrent inserts with keys greater than the consumed boundary
may appear on later pages; inserts or backfill at or before it require a new
traversal. Deletion may shorten a later page, including deletion of the cursor
anchor, without invalidating the cursor. There is no guarantee of a fixed
total or of catching late/backdated facts in one traversal. Follow run/SSE
state separately, and refresh from the first page and merge by message ID
when a complete current view is needed. An empty `next_cursor` means no more
records in that read, not that future messages cannot arrive.

### Incremental transcript sync

Request `?sync=0.0&limit=200` for the initial load. In this mode the response
contains `messages`, a `sync` token for the next request, and boolean `more`
and `reset` fields. Keep the token with its session's in-memory rows. Send it
again on refresh. Insertions and updates arrive in database revision order,
including messages with earlier creation times. Merge by ID and display in
the chronological order above. A reset signals deletion of retained rows:
discard previously accumulated rows (including earlier pages in this sync).
Continue while `more` is true, then publish the complete result and token
together. The normal page size limits apply. Combining `cursor` and `sync`,
or supplying a repeated or malformed sync token, returns HTTP 400.

### Errors and validation priority

The existing outer Host check, then Origin check, then bearer authentication
run before endpoint-specific query validation or session lookup. Their
existing HTTP 403/401 `auth_denied` behavior and Bearer challenge are unchanged.
After those checks, use the following order, independent of query key order:

| Priority | Check | Failure |
| --- | --- | --- |
| 1 | Parse URL query escaping. | HTTP 400 `invalid_argument`, message `invalid message query`. |
| 2 | Validate `limit` multiplicity, syntax, and range. | HTTP 400 `invalid_argument`, message `invalid message limit`. |
| 3 | Validate `cursor` multiplicity, encoding, schema/version, and session binding. | HTTP 400 `invalid_argument`, message `invalid message cursor`. |
| 4 | Look up the requested session. | Unknown session: HTTP 404 `not_found`, message `record not found`, `ref` equal to the path session ID. |
| 5 | Read its message page. | Unexpected storage/encoding failure: HTTP 500 `internal`, message `request failed`; no partial page or internal diagnostics. |

Use the existing `{"error":{"code":"...","message":"..."}}` envelope
(plus `ref` for not-found), not a successful empty page on an error. Thus bad
credentials beat a bad limit; a bad limit beats a bad cursor; a bad cursor,
including a session mismatch, beats an unknown session. An existing session
with no messages, including one with no runs, returns HTTP 200 with
`{"messages":[],"next_cursor":""}`. A valid beyond-end cursor returns that
same empty page for an existing session, but never hides an unknown session.

### Synthetic page example

All IDs, text, times, and continuation data below are synthetic examples, not
production messages, test results, or evidence that this endpoint is deployed.
Assume this example session has additional retained messages after these two.

```bash
curl -H "Authorization: Bearer $CLOUD_HARNESS_HTTP_TOKEN" \
  'http://127.0.0.1:19421/v1/sessions/ses_demo/messages?limit=2'
```

```json
{
  "messages": [
    {
      "id": "msg_user_trn_demo",
      "session_id": "ses_demo",
      "run_id": "run_demo",
      "turn_id": "trn_demo",
      "role": "user",
      "kind": "input",
      "status": "recorded",
      "content": "Summarize the sample document.",
      "event": "",
      "created_at": "2026-11-25T09:00:00.000000000Z"
    },
    {
      "id": "msg_response_rsp_demo",
      "session_id": "ses_demo",
      "run_id": "run_demo",
      "turn_id": "trn_demo",
      "role": "assistant",
      "kind": "response",
      "status": "recorded",
      "content": "The sample describes a controlled runtime.",
      "event": "",
      "created_at": "2026-11-25T09:00:01.000000000Z"
    }
  ],
  "next_cursor": "eyJ2IjoxLCJzZXNzaW9uX2lkIjoic2VzX2RlbW8iLCJjcmVhdGVkX2F0IjoiMjAyNi0xMS0yNVQwOTowMDowMS4wMDAwMDAwMDBaIiwiaWQiOiJtc2dfcmVzcG9uc2VfcnNwX2RlbW8ifQ"
}
```

An individual failed-run fact can look like this (also synthetic). This run
has no durable turn association; its retained event uses the legacy spelling:

```json
{
  "id": "msg_status_run_demo_failed",
  "session_id": "ses_demo",
  "run_id": "run_demo_failed",
  "turn_id": null,
  "role": "system",
  "kind": "run_status",
  "status": "failed",
  "content": "The sample provider request failed.",
  "event": "run.FAILED",
  "created_at": "2026-11-25T09:00:02.000000000Z"
}
```

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
| `summary` | string | Same summary as the Manager's WaitInfo and lifecycle notification: the full pending command text for `run_command`, or the original question for `ask_user`. Other tool approvals use `Waiting for approval: <tool>` as in notifications, followed on new lines by any `pending_invocation.warnings`. |
| `waiting_since` | string | Start of the current wait, as an ISO 8601 / RFC 3339 UTC timestamp. |
| `waiting_seconds` | integer | Whole elapsed seconds at request time, rounded down and clamped to zero if the server clock precedes `waiting_since`. |
| `approval_url` | string, optional | `<approval_base_url>/ui?run=<run-id>` when `notify.approval_base_url` is configured. Trailing base slashes are removed and the run ID is query encoded. Omitted when the base is unset. |

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
    "approval_url": "https://harness.example/ui?run=run_approval"
  },
  {
    "run_id": "run_question",
    "session_id": "ses_2",
    "status": "WAITING",
    "wait_reason": "ask_user",
    "summary": "Which deployment region should we use?",
    "waiting_since": "2026-11-19T11:58:00Z",
    "waiting_seconds": 120,
    "approval_url": "https://harness.example/ui?run=run_question"
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

## GET /v1/skills

Returns the CLI skill catalog as a name-sorted array of summaries, for example
`[{"name":"review","description":"Review a change"}]`. Workspace entries take
precedence over global entries. Discovery skips invalid files and returns
`[]` for an empty catalog. If discovery fails to read any valid summaries,
the response is HTTP 503. This endpoint works before a session is created.

## GET /v1/models

Returns an array of `{id, name, model, fallback_model?, provider, provider_id?, provider_name?, legacy_id?, display_name?, thinking?, reasoning_type?, reasoning_values?, category?}`.
IDs are `default`, `profile:<name>`, or `custom:<providerID>/<model>` for each
model enabled on a custom service. Split custom IDs at the first slash; model
names may contain further slashes. `provider` is the actual protocol
(`openai`, `anthropic` or `gemini`). The default model of each custom provider
also has `legacy_id: "custom:<providerID>"`; existing preferences using that
ID remain valid. `thinking` is `supported`, `unsupported` or `unknown`.
`reasoning_type` is `toggle` or `effort` when advertised; `reasoning_values`
contains that selected catalog record's effort values. `category` is `chat`
or `other`. Optional fields also appear in saved/detected catalog entries.
Credentials are never returned. Unknown or disabled model IDs return 400.

## Project memories

`GET /v1/project-memories` returns the server workspace's confirmed facts and decisions, newest edit first. Each entry contains `id`, `kind`, `content`, `source_session_id`, `source_run_id`, `source_available` and `updated_at` (UTC). The project comes from the configured workspace. With a shared database, sessions and linked Git worktrees of that project share its entries. A separate clone or workspace has its own entries.

| Method | Path | JSON body | Success |
|---|---|---|---|
| POST | `/v1/project-memories` | `{"kind":"decision","content":"Use SQLite","source_session_id":"ses_…","source_run_id":"run_…"}` | 201 with the saved entry |
| PUT | `/v1/project-memories/{id}` | `{"kind":"fact","content":"SQLite stores local history"}` | 204 |
| DELETE | `/v1/project-memories/{id}` | — | 204 |

`kind` accepts `fact` or `decision`. The trimmed text must contain 1–500 Unicode characters. Creation checks the source run against both its session and the server's project. A missing or mismatched source or an entry outside the project returns 404. Source IDs stay attached through corrections. Opening `/ui/?session={source_session_id}&run={source_run_id}` follows the source. If retention has pruned the run, its memory remains editable and `source_available` becomes false.

Each new root run reads up to 12 recently confirmed or corrected entries within a 4 KiB budget, presented as reference data ahead of the user's current message. Saving a project memory requires an explicit POST with user-confirmed text.

## GET /v1/sessions/{id}/preferences
## PUT /v1/sessions/{id}/preferences

The equivalent `/v1/sessions/{id}/settings` routes are also available.
GET initially returns `{"approval_mode":"ask","model_id":"default","reasoning_effort":"default"}`.
PUT accepts those fields and returns saved values. `approval_mode` is one of
`ask`, `safe`, `workspace-write`, `full-auto`. `reasoning_effort` is one of
`default`, `off`, `low`, `medium`, `high`; omission means `default` (service
default for effort models, on for toggle models). `model_id` must be a configured model
ID or a legacy custom provider ID; an empty ID means `default`.
Invalid settings return 400; unknown sessions return 404. Migration 013 adds
`reasoning_effort` to `session_preferences` while preserving prior choices.
Changes apply only to the next turn, including after waiting/resuming an
older run. The run view reports its frozen choices separately.

## GET /v1/providers
## PUT /v1/providers

GET returns `{id, name, base_url, model, has_api_key, protocol, protocol_source, models, catalog, catalog_fetched_at?}`.
PUT accepts the same configuration with `api_key?` instead of `has_api_key`.
A new service requires a key; an omitted/empty key during edit retains it.
Keys are never returned. Old files without the new fields load as OpenAI
compatible with the original default model enabled.

`protocol` is `openai`, `anthropic` or `gemini`; `protocol_source` is `auto` or
`manual`. `models` contains the enabled IDs and always includes `model`.
`catalog` holds up to 1000 `{id, display_name?, thinking?, reasoning_type?, reasoning_values?, category?}` entries, with the
last successful fetch time in `catalog_fetched_at`. Refreshing the catalog
does not silently enable models or replace the default.

Manage services in **设置 → 模型服务**. A single searchable list supports
manual ID addition, enable checkboxes and an inline default radio. The default
is always enabled; select another default before unchecking it. Non-chat
models live in collapsed **其他**, while enabled/default entries stay visible
outside that group. Models.dev friendly names take precedence over IDs.
Select models and thinking effort in
the **conversation header**; the composer retains its approval-mode chip.
IDs use 1–64 ASCII letters, digits, underscores or hyphens. Base URLs require
HTTPS; only loopback hosts permit HTTP. User info, queries and fragments are
rejected. Configuration is atomically saved in `compatible-providers.json`
with mode 0600 and must not be committed. All settings routes use the same
bearer, Host/Origin, JSON content type and body limits as other `/v1/` routes.

## POST /v1/providers/detect

Accepts `{base_url, api_key?, provider_id?, protocol?}`. An empty key with a
saved `provider_id` reuses that key privately. No settings are written and
no inference is performed. URL validation is identical to save.
Official Gemini/Anthropic hosts select their protocol directly; other hosts
are probed concurrently using Bearer, `x-api-key` + `anthropic-version`, or
`x-goog-api-key` headers. Keys are never added to query strings. A manual
`protocol` probes only that protocol. Each probe is accepted only when its
own response matches that protocol. When more than one probe succeeds,
auto-detection prefers OpenAI compatible, then Anthropic, then Gemini, and
returns the other valid protocols in `alternatives`. An OpenAI catalog whose
`supported_endpoint_types` all exclude `openai` but name exactly one of
`anthropic` or `gemini` follows that hint. The settings protocol menu keeps
this in its title (for example, “该服务也兼容 Anthropic”) and can still
override the choice.

Each request times out after 10 seconds, reads at most 1 MiB and refuses
redirects. Returns `{protocol, models:[{id, display_name?, thinking?, reasoning_type?, reasoning_values?, category?}], fetched_at, alternatives?}`.
Gemini lists exclude models without `generateContent` and strip the
`models/` prefix. HTTP, invalid/oversized catalog and network errors return
502 with a fixed Chinese `error` and numeric `upstream_status` (0 when no
HTTP response arrived). No upstream bodies, network errors or keys are exposed.
Services without model lists can still be saved using a manual model ID.

## POST /v1/providers/{id}/refresh-models

Fetches the saved protocol's catalog and atomically updates only its cache.
Returns public provider metadata. Failures retain the last successful cache.
A concurrent URL/key/protocol edit causes a conflict instead of saving stale
results. Bounds and errors match detection. The settings card uses one
**刷新模型列表** action.

## POST /v1/providers/{id}/test

Retained for older clients. Reads the saved protocol's list without changing
configuration and returns `{ok:true, model_available:boolean}`. It checks
catalog access only; it never starts inference.

## DELETE /v1/providers/{id}

Deletes the saved service and credential, returning 204; unknown IDs return
404. Atomic replacement retains mode 0600. Preferences and run snapshots
remain intact, so sessions using deleted services must choose another model.

### Thinking parameter negotiation

Without catalog metadata, OpenAI compatible services receive
`reasoning_effort` for low/medium/high; `off` and `default` omit it. Catalog
effort models send only advertised values (including `none/off` for off). Anthropic receives thinking budgets of
4000/10000/24000 with `max_tokens >= budget + 8000`, without temperature or
forced tool choice. Gemini uses its OpenAI compatibility endpoint at
`<base_url>/openai` and `extra_body.google.thinking_config` with budgets
0/1024/8192/24576 for off/low/medium/high and `include_thoughts:false`.
`default` leaves effort-based service defaults intact. Toggle models expose only
**关/开** in the header: `default` (and legacy low/medium/high preferences)
means on. Agnes OpenAI-compatible requests use the documented
`chat_template_kwargs.enable_thinking` boolean, including explicit `false`
for off ([Agnes Thinking mode](https://agnes-ai.com/en/docs/agnes-30-flash)).
Native Anthropic toggles use `thinking.type: enabled/disabled`. Effort menus
disable unavailable values: Step 3.5 Flash has low/high, Step 5 Preview has
low/medium/high. `none/off` map to off; `minimal` is used for low only when
`low` is absent. `max/xhigh` never expand the high option. A saved preference
missing from the current capability record falls back to the service default.

Known unsupported models omit these parameters and disable the header
control. Unknown models may try them. An upstream HTTP 400 identifying a
thinking parameter causes one retry without it, caches unsupported status
for that provider/model in this process, and records
`provider.reasoning_unsupported` with only protocol and model. The model
menu reads the same runtime capability cache. No raw error body is traced.

### Public models.dev metadata

Settings list/save/detect/refresh and `GET /v1/models` enrich models from
`GET https://models.dev/api.json`. Only this fixed public GET is used, with
no Authorization, API key, custom base URL, query or referrer; redirects are
refused and the request has a five-second timeout and a 32 MiB bound. A public
`models-dev-cache.json` beside `compatible-providers.json` lasts 24 hours.
Failures retain stale data; no cache falls back to name heuristics and does
not fail settings or manual saves. Inference reads metadata without fetching.

Matching strips provider namespaces (for example `stepfun/` or
`nvidia/stepfun-ai/`) then compares exact case-sensitive model IDs. Candidate
providers first match the configured host and the longest boundary-aligned
API path prefix. Thus `/v1` and `/step_plan/v1` on `api.stepfun.com` select
different records; `api.stepfun.ai` stays separate from China. Without an
address match, known first-party providers precede aggregators; ties are
stable by provider ID. Exactly one record supplies capabilities, never their
union. Official StepFun low/medium/high cannot inherit Requesty's none/max.

Text-output models belong in chat unless audio-only input, audio dialogue
without tool calls, router or realtime. Other modalities live in **其他**.
Unmatched tts/asr/realtime/whisper/embedding/image/video/router names use the
same other group; unfamiliar manual IDs default to chat. Reasoning=false
is authoritative, while unmatched IDs retain the existing thinking heuristic
and the runtime HTTP 400 fallback. Default unit tests use a small local
fixture and loopback HTTP servers, with no external requests.


## MCP connections

| Request | Result |
| --- | --- |
| `GET /v1/mcp/servers` | Lists configured servers with transport, authorization method, status, diagnostics and tool names. |
| `POST /v1/mcp/servers/{name}/connect` | Opens the configured connection and refreshes its tools. Returns 204 on success. |
| `POST /v1/mcp/servers/{name}/disconnect` | Removes the tools and clears the connection and its credentials. Returns 204 on success. |
| `POST /v1/mcp/servers/{name}/authorize` | Starts an OAuth flow for a configured public client. Sets the state cookie and returns a JSON object with the browser authorization URL in `url`. |

The settings page opens the authorization URL in the same browser. The issuer redirects back to `GET /v1/mcp/oauth/callback`. A successful exchange reconnects the server and displays a link back to the workspace.
