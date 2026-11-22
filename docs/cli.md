# CLI command reference

## run

```bash
cloud-harness run --message TEXT [--workspace dir] [--no-repomap]
```

Each model request includes a Repo-Map when the workspace contains parseable
source files. The map follows project rules and is regenerated from the workspace
for each request, with an 8 KiB output limit. Use `--no-repomap` to disable it
for a run; project rules and the remaining context are assembled as usual.

## serve

```bash
cloud-harness serve [--addr 127.0.0.1:19421] [--http-token TOKEN] [--http-allowed-hosts HOST[,HOST]] [--workspace dir] [--db path] [--provider mock|openai] [--worktree]
CLOUD_HARNESS_HTTP_TOKEN='replace-with-a-long-random-token' cloud-harness serve --addr 0.0.0.0:19421
```

The default loopback listener can start without a token for local development.
Non-loopback listeners require an HTTP token of at least 16 characters, set
with `--http-token`, `CLOUD_HARNESS_HTTP_TOKEN`, or the JSON `http_token` field.
Without one, serve exits before opening the store or binding a port with
`non-loopback listen requires an HTTP token` and a token configuration hint.
Errors never echo the token. The legacy `--http-allow-insecure`,
`http_allow_insecure`, `CLOUD_HARNESS_HTTP_ALLOW_INSECURE`, and
`CLOUD_HARNESS_ALLOW_EXTERNAL` settings cannot bypass this requirement.

A loopback listener only answers requests whose `Host` header is `localhost`
or a loopback IP address. To reach it under another name through a tunnel or
reverse proxy, list that name with `--http-allowed-hosts HOST[,HOST]`,
`CLOUD_HARNESS_HTTP_ALLOWED_HOSTS`, or the JSON `http_allowed_hosts` array.
Every listener refuses a request whose `Origin` differs from its `Host`, and
a write request with a body must send `Content-Type: application/json`. See
[request checks](api.md#request-checks).

`serve` stops gracefully on `SIGINT` (Ctrl+C) and `SIGTERM`; see
[timeouts and shutdown](api.md#timeouts-and-shutdown).

When a token is set, `/v1/` endpoints require `Authorization: Bearer <token>`.
Missing or incorrect tokens return HTTP 401 with a `WWW-Authenticate: Bearer`
challenge. `/healthz`, the static `/approval` page, and `/ui` and its assets
are accessible without a token. Paste the configured token once on `/approval`
to see the pending queue, approve or deny a run, or answer an `ask_user`
question. The browser keeps the token in `localStorage` for this origin.
`GET /v1/approvals` lists pending items across sessions, oldest wait first. See the
[HTTP API reference](api.md#get-approval).

Open `/ui` to send messages and inspect runs. It uses the token saved by
`/approval` on the same origin for its API calls. See the
[Web UI build and usage reference](api.md#get-ui).

The authenticated `GET /v1/sessions` and
`GET /v1/sessions/{id}/runs` endpoints list sessions with run aggregates and
the runs in one session; see the [session API reference](api.md#get-v1-sessions).

## Run notifications

`run`, `chat`, and `serve` use the same Manager notification hooks. Configure
`notify.channels` in the harness JSON file to enable delivery; without it,
nothing is sent. Channels support `webhook`, `bark`, and `ntfy`, with per-channel
`type`, HTTP(S) `url`, optional `events`, and `timeout_ms` (default five seconds).
See [notify configuration](config.md#run-notifications) for a complete JSON
example and the one-based credential variables
`CLOUD_HARNESS_NOTIFY_CHANNEL_N_WEBHOOK_BEARER`,
`CLOUD_HARNESS_NOTIFY_CHANNEL_N_BARK_KEY`, and
`CLOUD_HARNESS_NOTIFY_CHANNEL_N_NTFY_TOKEN`.

Event filters are `approval`, `ask_user`, `completion`, and `failure` (which
also includes cancellation). Omitting `events` subscribes to all four. Each
channel is sent asynchronously and concurrently with its own timeout. Failures
are logged and recorded as `notify.failed`; they are not retried and do not
change run status. Set `approval_base_url` to include an approval link in
waiting notifications. The link uses `<approval_base_url>/approval?run=<run-id>`
and opens the selected run on the approval page. If unset, notifications omit
the link.

## agents / agents show

```bash
cloud-harness agents [--workspace dir] [--json]
cloud-harness agents list [--workspace dir] [--json]   # explicit list alias
cloud-harness agents show <name> [--workspace dir] [--json]

cloud-harness agents --workspace ./project
cloud-harness agents show explorer --workspace ./project --json
```

`agents` reads declarative child-agent definitions from the workspace and lists
them in ascending name order. `--workspace` defaults to `.` (as for `review`).
The command only reads definitions: it does not load runtime configuration,
open a database, write files, start a process, or call a provider. The model
can select one of these definitions by name when it calls `delegate_task`.

The human list includes name, source (`builtin` or `file`), an explicit
`OVERRIDES_BUILTIN` boolean, description, tools, and model. Tools display as
`inherit` when undeclared, `none` for an explicit empty set, or a list of tool
names. An undeclared model displays as `—`. `show` prints every definition
field, including the complete System body and optional budgets (shown as `—`
when absent). An unknown name exits with code 1 and prints the sorted available
names to stderr. Invalid arguments exit with code 2.

`--json` emits an array of complete definitions for the list, or one definition
object for `show`. All objects contain `name`, `description`, `tools`, `model`,
`max_model_calls`, `max_tool_calls`, `system`, `source`, and `overrides_builtin`.
Undeclared tools are JSON `null`; an explicit empty set is `[]`. Undeclared
model and budgets are respectively `""` and `0`; a declared budget must be
strictly positive.

### Definition files and fields

Place each definition at `<workspace>/.agents/agents/<name>.md`. The filename
stem is the name and must match `[a-z0-9][a-z0-9-_]*`: lowercase ASCII letters,
digits, `_`, and `-`, starting with a letter or digit. For example,
`code-reader.md` and `review_2.md` are valid; `CodeReader.md` and `_reader.md`
are errors. Only regular files with the exact `.md` extension in that directory
are considered. Other extensions and subdirectories are ignored. Symbolic
links, including symlinked definition directories, are skipped without reading
their targets. A missing definitions directory or an empty workspace passed
to the Go `agents.Load` API yields only the two built-ins.

A file must begin with a line containing exactly `---`; the next such line
closes its frontmatter. LF and CRLF line endings are accepted. Only single-line
`key: value` entries are supported, with optional blank lines. This is a small
handwritten format, not a YAML parser: no nested mappings, multiline values,
anchors, escape expansion, or inline comments. String values may optionally
have matching outer single or double quotes; those outer quotes are removed
and their contents remain literal. Unquoted values are trimmed. Duplicate
keys and unknown fields are errors rather than silent overrides.

| Field | Required | Meaning and validation |
| --- | --- | --- |
| `description` | Yes | Nonempty, single-line description; whitespace-only and quoted empty strings are rejected. |
| `tools` | No | Comma-separated names or a bracketed comma-separated list. Omitted means `nil` (no additional restriction); `[]` means an explicit empty set. Empty values, empty list items, unknown names, `ask_user`, and `delegate_task` are errors. |
| `model` | No | Single-line string that overrides the model for the child run. An empty value inherits the parent's model. |
| `max_model_calls` | No | Upper bound on child model calls. Must be a positive integer. |
| `max_tool_calls` | No | Upper bound on child tool calls. Must be a positive integer. |
| `name` | No | Must exactly match the filename stem, including case. The filename always determines the registry key. |

These tool forms are equivalent (tool names are unquoted):

```text
tools: read_file, grep
tools: [read_file, grep]
```

Omitting `tools` means no *additional* restriction, rather than unrestricted
authority: a definition never grants permissions by itself. `tools: []`
declares no tools. An empty `tools:` is rejected to keep these meanings
distinct. Known names are the workspace catalog (`read_file`, `write_file`,
`edit_file`, `list_files`, `grep`, `git_status`, `git_diff`, `git_log`,
`run_command`) plus eligible built-ins (`finish_turn`, `update_plan`,
`retrieve_history`, `retrieve_resource`, `execute_program`, `poll_tool`).
`ask_user` and `delegate_task` are always forbidden for child agents and are
configuration errors even though the harness knows those tools. MCP servers
are not started or consulted to discover additional names, so an MCP tool name
in `tools` is a configuration error.

The text after the first closing frontmatter delimiter, with leading and
trailing whitespace trimmed, becomes `System`. It may be empty. Internal
newlines, indentation, and later `---` lines are retained as body text.

Example `.agents/agents/code-reader.md`:

```markdown
---
name: code-reader
description: Read-only code exploration and location
tools: [read_file, list_files, grep, git_status, git_diff, git_log]
model: example-model
max_model_calls: 8
max_tool_calls: 20
---
Locate the implementation relevant to the task and cite file paths.

---
Explain any gaps in the available evidence.
```

### Built-ins, overrides, and errors

Two definitions are built into the code: `explorer` for read-only code
exploration/location and `reviewer` for read-only code review. Each declares
exactly `read_file`, `list_files`, `grep`, `git_status`, `git_diff`, and
`git_log`. Both have `source: "builtin"` and `overrides_builtin: false`.

A workspace file with the same name replaces the **entire** built-in
definition and is shown with `source: "file"` and `overrides_builtin: true`.
Fields are not merged: an `explorer.md` that omits `tools` gets `nil`/`inherit`,
not the built-in read-only list. Other workspace definitions have
`overrides_builtin: false`. Loading and list ordering are deterministic;
files are visited in filename order, and loading stops at the first error.
Invalid definitions cause both list and show to exit with code 1, including
the file path and the specific reason in stderr; no partial list is emitted.

Examples of invalid definitions and their diagnostics:

```text
# .agents/agents/code-reader.md frontmatter:
toolz: read_file
-> agents: …/.agents/agents/code-reader.md: line …: unknown field "toolz"

# .agents/agents/code-reader.md frontmatter:
tools: [read_file, ask_user]
-> agents: …/.agents/agents/code-reader.md: line …: tools: tool "ask_user" is forbidden for child agents

# .agents/agents/code-reader.md frontmatter:
max_model_calls: 0
-> agents: …/.agents/agents/code-reader.md: line …: max_model_calls: must be an integer > 0, got "0"

# A file without the opening --- line:
-> agents: …/.agents/agents/code-reader.md: missing frontmatter: file must start with a --- line
```

### Select an agent with `delegate_task`

Pass the optional string `agent` along with the required `goal`. The runtime
looks up the exact name in the parent run's workspace, and the child uses that
same workspace. An omitted or empty `agent` uses ordinary delegation. An
unknown name returns a tool error with the sorted list of available names.
Document workspace agent names in `AGENTS.md` for the model to use.

- The child's grants come from the parent's authorization and the effective
  tool list. When both the definition's `tools` and the caller's `allowed_tools`
  are declared, the effective list is their intersection. If the caller omits
  `allowed_tools`, the definition's list applies. If the definition omits
  `tools`, normal delegation rules apply. An empty intersection or `tools: []`
  leaves the child with `finish_turn` and no workspace grants. Children cannot
  request user input or delegate again.
- Each declared `max_model_calls` or `max_tool_calls` caps the child budget
  at the lower of that value and the budget already derived from the parent's
  remaining budget and the caller's limits. A child that exhausts a cap fails
  through the budget exhaustion path.
- A nonempty `model` selects a model for the child on the same provider. If it
  is empty, the child uses the parent's model selection or the provider default.
  The definition's `System` text follows the base child system prompt and
  precedes the task's success criteria and output contract.

For the `code-reader` definition above, this tool call gives the child only
`grep` and `read_file` and caps it at four model calls and twenty tool calls
if the parent has sufficient budget.

```json
{
  "goal": "Find the code that loads workspace configuration and cite the relevant files.",
  "agent": "code-reader",
  "allowed_tools": ["grep", "read_file"],
  "max_model_calls": 4,
  "max_tool_calls": 30
}
```

The parent's `child.started` and `child.finished` events include `agent` when
the caller names a definition.

## run / chat

```bash
cloud-harness run --message TEXT [--plan] [--stream] [--workspace dir] [--worktree] [--provider mock|openai] [--db path]
cloud-harness chat [--message TEXT] [--session ID] [--stream=false] [--workspace dir] [--worktree] [--provider mock|openai] [--db path]
```

`chat` prints model text while the model is still generating it. `run` does
so only with `--stream`, and then on stderr: its stdout is always the one JSON
line, so a script that reads it gets only the final result.

| Command | Default | Option |
| --- | --- | --- |
| `chat` | stdout: model text as it arrives, including text the model writes before a tool call. The final answer is not printed a second time. | `--stream=false`: stdout has only the final answer of each turn, printed when the turn stops. |
| `run` | stdout: one JSON line when the run stops. Nothing is streamed. | `--stream`: the same JSON line on stdout, and model text on stderr as it arrives. |

Each streamed model response ends with a newline. Only the text of the run
you started is written; child runs execute concurrently and their text is left
out. If a response fails partway, the text already written stays on screen.
The text is never stored: the run's result is still its `final_answer`.

Model text written to the terminal has its control characters removed, apart
from newlines and tabs: the streamed text, the final answer `chat` prints, and
a question the model asks. An escape sequence therefore shows as its printable
remainder (`[31m`) instead of acting on the terminal. The `final_answer` in
the JSON line of `run`, in the HTTP API and in the database is unchanged.

`--plan` runs the turn with a read-only model tool catalog. File writes,
commands, child delegation, scripts, and plan updates are omitted from the
provider request. A call to one of those omitted names is recorded as an
unknown tool before dispatch. Plan runs skip automatic workspace snapshots
and keep the restricted catalog after a resume. The run output includes
`"plan": true`. The event stream records `plan.mode` with `{"enabled": true}`.

`--worktree` is off by default. When enabled, each new run (each chat turn)
checks out the dispatch-time `HEAD` commit in
`<workspace>/.cloud-harness/worktrees/<run-id>/`, on a new branch named
`cloud-harness/<run-id>`. Existing uncommitted changes in the main workspace
are not copied. The workspace must have a `.git` entry and a committed `HEAD`;
a non-Git workspace or an already linked worktree is rejected before opening
the database or creating run records. Nested worktree runs are not supported.

File and command tools, delegated children, and shadow snapshots use the new
worktree. Children share their parent's worktree. The SQLite store and run
events stay in the original workspace (the default database is
`<workspace>/.cloud-harness/harness.db`); an explicit `--db` still selects its
own path. Each run record persists `WorktreeBaseSHA`, `WorktreeBranch`, and
`WorktreePath`, including across approval resumes. `.cloud-harness/` is also
added idempotently to the main repository's `.git/info/exclude` so its Git
status stays clean without editing tracked files.

When a run completes, fails, or is cancelled, its persistent event stream
receives `worktree.summary`: `run_id`, `base_sha`, `branch`, `worktree_path`,
`files`, `stat`, `diff`, and `truncated`. The summary compares against the
merge base of the dispatch SHA and worktree `HEAD` (`base...HEAD`) and includes
pending tracked and untracked edits, without changing the real index or
making a commit. Harness state is excluded. The unified diff is capped at
64 KiB; the file list and stat remain complete. If Git cannot produce the
summary, the event still records its identity and an `error` diagnostic.
Waiting for approval or user input does not end the run or emit its summary.

Worktrees and branches remain after the run; completion does not remove them.
Use the lifecycle commands below to inspect, integrate or discard the result.

## /skill

```bash
cloud-harness /skill NAME [--workspace dir]
```

`/skill NAME` prints the full `SKILL.md`, including its frontmatter. You can
also type `/skill NAME` in `chat` to display the file without starting a run.
The CLI uses the same lookup as the model's `load_skill` tool.

Place a skill at `<workspace>/.agents/skills/<name>/SKILL.md` or under the
same relative path in the global config directory. The global directory is
the parent of `CLOUD_HARNESS_CONFIG` when set. Otherwise it is the OS user
config directory with `cloud-harness` appended. A valid workspace skill
overrides a global skill with the same name.

```markdown
---
name: review
description: Review the current changes for regressions.
---
Read the diff and check how errors propagate.
```

Use UTF-8 text and single-line `key: value` frontmatter between `---` lines.
The name must match the directory and use ASCII letters, digits, `_`, or `-`.
The description must be nonempty. Matching outer quotes are optional. The
harness reads regular files and skips invalid definitions during discovery.

The executor's first request shows only names and descriptions after the
`AGENTS.md` rules and before session history. It shows up to 50 skills in
name order and marks the number omitted. `load_skill` and `/skill` can still
load an omitted name. Both cap the returned text at 64 KiB including the
truncation marker. A missing name produces a clear error.

## worktrees

```bash
cloud-harness worktrees list [--json] [--workspace dir] [--db path]
cloud-harness worktrees diff <run-id> [--json] [--workspace dir] [--db path]
cloud-harness worktrees merge <run-id> [--test-command "go test ./..."] [--provider mock|openai] [--workspace dir] [--db path]
cloud-harness worktrees discard <run-id> [--force] [--workspace dir] [--db path]
```

The run ID may come before flags (as shown) or after all flags. The workspace
defaults to `.`. The default DB is anchored at
`<workspace>/.cloud-harness/harness.db`, matching `run --worktree`; pass the
same explicit `--db` if the run used a different store. No new lifecycle table
is used: `runs.worktree_state` stores only the successful terminal decision
(`merged` or `discarded`); execution status, the original run binding and
`worktree.Lookup` determine the observed state.

### List, diff and lifecycle states

`list` includes run ID, branch, full base SHA, path and state, including
records of merged/discarded worktrees after their directories disappear.
`--json` emits an array with `run_id`, `branch`, `base_sha`, `worktree_path`,
`state`, `run_status`, `exists`, and optional `orphan`/`error` diagnostics.
Reconciliation reads all bound runs, without the interactive run-list limit.
A DB shared by multiple workspaces is filtered to the selected workspace.

| State | Meaning |
| --- | --- |
| `running` (运行中) | Nonterminal run, including approval/user waits; finish or cancel before lifecycle mutations. |
| `pending` (待处理) | Terminal run whose retained worktree is ready for review. |
| `merged` (已合并) | Both merge test gates passed and integration was recorded. |
| `discarded` (已销毁) | An explicit discard removed the worktree and branch; the run record remains. |

Normal transitions are `running → pending → merged` or
`running → pending → discarded`. Missing artifacts are not silently treated
as a successful discard: `orphan: missing_worktree` identifies a bound run
without its worktree; `orphan: missing_run` identifies a worktree/metadata
without a run. Changed bindings, unreadable metadata and artifacts remaining
after a recorded terminal decision are also explicitly diagnosed. `doctor`
performs the same read-only reconciliation at startup and reports orphans as
warnings. It never migrates a DB or removes a worktree. Version 7 worktree
records can be listed/read without migration; the first lifecycle mutation
applies migration 008 through the normal store opener.

`diff` directly reuses `worktree.Summarize`: the complete file list and stat,
plus a unified diff relative to the original dispatch base, including pending
tracked/untracked edits and capped at 64 KiB. `--json` has the same fields as
`worktree.summary`. Merged, discarded, missing or mismatched worktrees produce
an explicit error. List and diff do not create a DB or modify the real index.

### Merge policy (six steps)

1. Determine the main workspace branch with `git symbolic-ref --short HEAD`;
   no branch name is hardcoded, and detached HEAD is rejected. Require a clean
   main workspace with no merge in progress. Commit pending agent file edits
   on the agent branch for audit, then run `git merge <main>` **inside the
   agent worktree first**. Commits use the local identity `Cloud Harness
   <cloud-harness@localhost>` without changing repository configuration.
2. On conflicts, ask the configured provider to resolve files, then run the
   test gate. `worktree.ConflictResolutionAttempts` is **2** by default and is
   a constant, shared across the entire merge invocation (including an older
   retained conflict and any conflict with newer main). Each repair round is capped at 8 model calls and 24 tool calls.
   Repairs use the same worktree and main DB, restricted read/write/edit tools,
   no command/delegation tools and no extra snapshots. Git staging and commits
   remain in the coordinator. Conflict-marker/whitespace checks and tests
   must pass before committing a resolution. The default mock does not repair
   conflicts; select the intended provider explicitly for model repairs.
3. If repair fails, stop and print the conflict file list. Main remains
   unchanged; the agent worktree, merge index and branch are retained. Resolve
   and stage (or commit) conflicts there manually, then retry `merge`. A retry
   completes a staged resolution and also integrates the current main branch
   if main has advanced since the original conflict.
4. Only after the agent branch is clean and its test gate passes, merge it
   into main with a normal `--no-ff` merge. A failed merge is aborted with
   `git merge --abort`; recovery also restores the pre-merge SHA if needed.
   The main workspace keeps its pre-merge content on failure.
5. Run the test gate again on main. A failing gate (or failure to record the
   successful decision) runs `git reset --hard <pre-merge-SHA>` using a
   recovery context independent of cancellation. The run stays pending and
   its worktree remains retryable. Only after both gates pass is `merged`
   recorded; then the worktree, agent branch and private metadata are removed.
6. Merge requests queue behind a **process-wide in-process lock**, covering
   run reconciliation, Git operations, both gates, DB state and cleanup.
   Discard and retain use the same queue. This does not coordinate separate
   operating-system processes: do not run concurrent merge/discard/retain CLI
   processes against the same repository.

Neither `ours`/`theirs` overwrite strategies nor rebase are used. Existing
commits and the original base SHA remain auditable. A failed gate never marks
the run merged. If integration succeeds but cleanup fails, the error states
that the run is merged with cleanup pending; `discard` retries that cleanup.

The required test gate defaults to `go test ./...` in each workspace. Use
`--test-command "go test -p 1 ./..."` or another project test command when
needed. This is an executable and space-separated arguments, **not a shell
expression** (no quote/escape interpretation, pipes or redirects). An empty
command is refused. Each gate has a five-minute timeout, and its output is
written to stderr. Tests should leave the workspace clean.

### Discard and retention

`discard` removes the worktree first, then its branch and metadata, retaining
the run and its audit history. Pending/unmerged runs require explicit
`--force`, including committed or empty pending branches; the flag authorizes
loss of their result. It never overrides an active/waiting run: finish/cancel
that run first. Already merged/discarded cleanup is idempotent and needs no
force; a merged record keeps its `merged` decision. No run row is deleted by
`discard`.

Orphan cleanup uses existing `retain --prune-runs-days N` invocations, with no
new schedule or cleanup switch. Worktrees of runs selected by that prune are
reclaimed after the DB prune. For worktrees already missing their run record,
the later of directory/metadata modification times is compared with the same
UTC cutoff. New orphans and worktrees of surviving runs are retained.
`--dry-run` reports `worktree_reclamation` entries without deleting worktrees,
branches or metadata; actual entries contain `removed` and optional `error`.
Unreadable or mismatched artifacts are reported for manual inspection.

### serve --worktree

```bash
cloud-harness serve --worktree [--workspace dir] [--db path] [--provider mock|openai] [--addr 127.0.0.1:19421]
```

`serve --worktree` uses the same `openRunManager` isolation path as run/chat.
Every run created over HTTP gets its own worktree, with the DB anchored in
main. Approval resumes, recovery and cancellation continue through the
persisted worktree manager routing. Terminal runs emit `worktree.summary`
and retain their worktrees. HTTP exposes no merge or discard endpoints;
use the four CLI commands for lifecycle management. Without the flag, serve
keeps its existing shared-workspace behavior.

## Images in chat and run (`@image:<path>`)

Attach local images by putting `@image:<path>` in a user message. `run
--message`, `chat --message`, and interactive chat input support the same
syntax. An answer to an `ask_user` question in chat can also contain images.

```bash
cloud-harness run --workspace ./project --provider openai \
  --message 'Explain this error @image:errors.png'
cloud-harness chat --workspace ./project --provider openai \
  --message 'Compare these screens @image:"my shot.png" @image:/tmp/after.webp'

# At the interactive chat prompt, type the message itself (no outer shell quotes):
Explain the UI @image:"screenshots/my shot.png"
```

Paths resolve from `--workspace` (the current directory when omitted), or can
be absolute. Tokens may occur anywhere outside the code forms below. An
unquoted path ends at Unicode whitespace; use double quotes immediately after
the colon to include spaces. Within a quoted path, `\"` means a literal quote
and `\\` means a literal backslash. Other backslashes stay literal; unquoted
paths also keep their backslashes. A closing double quote ends the path.
Paths are not shell-expanded: `~`, `$VARIABLE`, and globs are literal. When
using `--message`, quote the whole argument for your shell as in the examples
so the inner double quotes reach the CLI.

The parser removes attachment tokens from the text and preserves image order.
Multiple tokens attach multiple images, including repeated references to the
same file. Text alone may be empty when there are images. To discuss token
syntax literally, write `\@image:example.png`; the backslash is removed and
the literal token is sent as text. Matched inline backtick spans and fenced
code blocks are copied verbatim without parsing tokens or escapes. Fences
use at least three backticks or tildes at the beginning of a line with up to
three leading spaces; their closing fence uses the same character and at
least the opening length. An unclosed fence protects the rest of the input.
An unmatched inline backtick and indented code without a fence do not protect
tokens. This lets fenced error logs include `@image:` without opening files.
Use `--message` for multiline messages; interactive chat still reads one line
per message.

Supported formats are PNG, JPG/JPEG, WebP, and GIF. Format and MIME type come
from file-header magic, not the filename extension. Image headers must also
provide valid dimensions. Limits apply to the original file bytes, before
base64 encoding: **10 MB (10 × 1,024 × 1,024 bytes) per image** and **20 MB
per turn**, inclusive. Repeated attachments each count toward the total;
images added to `ask_user` answers count with the same turn's earlier images.
Empty files, directories, unsupported signatures, unreadable paths, and
oversized inputs are rejected before a model request. A bad initial message
does not create a turn or run; a bad answer does not resume the waiting run.

Example diagnostics (printed to stderr with exit code 1):

```text
@image:                     -> invalid_argument: @image: requires a non-empty path
@image:""                   -> invalid_argument: @image: requires a non-empty path
@image:"unclosed.png        -> invalid_argument: @image: has an unterminated quoted path
@image:missing.png          -> invalid_argument: image "…/missing.png" cannot be read: …
@image:notes.txt            -> invalid_argument: image "…/notes.txt" has an unsupported or invalid file signature …
@image:large.png            -> invalid_argument: image "…/large.png" exceeds the 10 MB per-image limit
# Multiple valid files over 20 MB in total:
                            -> invalid_argument: images exceed the 20 MB per-turn limit
```

OpenAI-compatible requests send the remaining text as a `text` content block
and each image as an `image_url` block containing an inline base64 data URL.
The wire structure follows the [official OpenAI vision guide](https://developers.openai.com/api/docs/guides/images-vision).
Text-only messages retain the existing string content representation. The
mock provider makes no network request and records only image references,
count, byte sizes, dimensions, MIME types, and hashes for test assertions.
Providers must explicitly support image input; otherwise the turn fails
before execution. Established text-only OpenAI models (such as `gpt-3.5-turbo`,
the original `gpt-4`, and `o3-mini`) are rejected locally. For a text-only
compatible endpoint or custom model, set `OPENAI_DISABLE_IMAGES=true` or
`"openai_disable_images": true` in the config file to reject attachments
locally. Other compatible model names receive image blocks; if the server
rejects them, the error explicitly says `provider/model rejected image input`
and no text-only fallback is attempted.

Image bytes and data URLs are transient and are not written to SQLite.
Checkpoints keep only the absolute local path, MIME type, byte size,
dimensions, and SHA-256 hash. Resuming a waiting run re-reads those references
and fails before the next model request if a file is missing or changed.
Files must therefore remain available for the lifetime of a waiting run.
Storage tables, workspace snapshot behavior, retain, and rewind are unchanged.

## doctor

```bash
cloud-harness doctor [--db path] [--workspace dir] [--provider mock|openai]
cloud-harness doctor --json
```

`doctor` runs read-only checks for configuration, SQLite, Git and the
workspace, provider connectivity, and MCP configuration. Each line reports a
`name`, `status` (`ok`, `warn`, or `fail`), and `detail`; the final `overall`
line is `fail` when any check failed. A missing database is reported as
`warn: 尚未创建` and is not created. A nonzero exit code is returned only when
there is a failure, so warnings remain usable in a fresh workspace.

The database check opens SQLite with `mode=ro`, reads the migration version,
journal mode (WAL), and `PRAGMA foreign_key_check`, and never runs migrations,
checkpoints, or writes. Git checks only executable discovery and, when a
snapshot shadow repository already exists, readability of its `HEAD`; it does
not run `gc` or `fsck`. Worktree inventory is reconciled with run records and
orphans are reported as warnings (see `worktrees`); nothing is reclaimed. Each
configured MCP server is started, asked for its tool list, and stopped again,
with a 10 second limit per server; the `mcp` line lists every server with its
tool count or the reason it is unavailable, and is `fail` when any server is
unavailable. `--json` adds a `servers` array with `name`, `readonly`, `state`
(`running` or `failed`), `tools`, `warnings`, and `error`. The `/v1/doctor`
endpoint of `serve` does not start anything: it reports the servers that the
`serve` process already owns, including one that exited after startup. With `--provider openai`, a configured key gets one short
`GET /models` probe; a missing `OPENAI_API_KEY` is a warning. API key material
is redacted from diagnostics.

`--json` emits an array of the same objects, including the final `overall`
object, so scripts can inspect the checks without parsing human text.
The `oracle` check also reports consultation count, estimated cost in USD, and
the number of `oracle.failed` events whose reason is `budget`. Its JSON object
contains an `oracle` summary with stable `consultations`,
`estimated_cost_usd`, and `budget_failures` fields. These totals cover all
recorded runs; unpriced models add zero cost.

The `policy` check lists the workspace, global, and builtin policy layers. The
`workspace_trust` check reports `missing`, `trusted`, `untrusted`, or `changed`
for the workspace `.cloud-harness/hooks.json` and `policy.json`, with the
content hash. An untrusted or changed file turns both checks into `warn` and
names `cloud-harness trust approve`; file contents are never printed.

## trust

```bash
cloud-harness trust status  [--workspace dir] [--json]
cloud-harness trust approve [--workspace dir] [--yes]
cloud-harness trust revoke  [--workspace dir]
```

The workspace files `.cloud-harness/hooks.json` and `.cloud-harness/policy.json`
can run shell commands and skip approvals, and an approved shell command can
write them. They therefore take effect only after you trust their exact
content. Agent file tools cannot read or write anything under
`.cloud-harness/`; see [policy](policy.md#效果与内置基线).

Trust is recorded per workspace and per file as a SHA-256 of the content in
`trust.json` in the configuration directory (the directory of
`CLOUD_HARNESS_CONFIG`, or `os.UserConfigDir()/cloud-harness`), mode `0600`.
That directory is outside the workspace, so file tools cannot reach it. If it
is inside the workspace, nothing is trusted and `trust approve` refuses to
write. Runs in `.cloud-harness/worktrees/<run>` use the decision recorded for
the original workspace; the content hash must still match.

| State | Meaning | Effect |
| --- | --- | --- |
| `missing` | The file does not exist. | Nothing to apply. |
| `trusted` | The current content was approved. | Hooks run; all policy rules apply. |
| `untrusted` | The file exists and was never approved. | Hooks are not run. Policy `allow` and `ask` rules are skipped; `deny` rules still apply. |
| `changed` | Different content was approved earlier. | Same as `untrusted` until approved again. |

`trust status` prints one line per file and the trust store path; `--json`
emits an array of `{file, path, workspace, state, sha256, reason}`. It never
writes.

`trust approve` validates both files, then prints the path, hash, and full
content of each file that is waiting, and asks
`Trust this content for <workspace>? [y/N]` (default no). The recorded hash is
the hash of the content shown. Without a terminal it requires `--yes`;
otherwise it records nothing and exits 1. A file that fails validation cannot
be trusted. With nothing waiting it prints `nothing to trust`.

`trust revoke` removes every decision for the workspace.

While a file is untrusted or changed, `run`, `chat`, and `serve` print a
`warning:` line on stderr at startup, each run records a persistent
`config.untrusted` event (file, path, state, hash, and message; no content),
and `doctor` reports `warn`.

Exit codes: 2 for usage errors, 1 when trust cannot be read or written or the
approval was not confirmed.

## Approval warnings for behavior files

Writes to `AGENTS.md` (at any depth) and to `.agents/**` (skills and agent
definitions) are ordinary workspace writes, but later runs load those files as
instructions. Their approval item therefore carries a warning, in addition to
the usual tool, arguments, and resources:

```text
Approval required: tool=write_file args=... resources=...
!!! WARNING: this writes AGENTS.md. AGENTS.md files are project instructions loaded into every later run, so approving it changes how later runs in this workspace behave. Review the full content before approving.
Approve? [y/n]:
```

The same text is in `pending_invocation.warnings` in the HTTP run view, in the
`summary` of `GET /v1/approvals` and of approval notifications, in a
highlighted alert on `/approval`, and above the pending tool call in `/ui`.
Names are matched case-insensitively and through symlinks. The warning does
not change the decision: the write still needs approval or a matching policy
rule. A policy `allow` rule that covers these paths skips the approval and the
warning with it.

## stats

```bash
cloud-harness stats [--days N] [--by day|model|run] [--db path]
cloud-harness stats --days 30 --by model --json
```

`stats` defaults to the last 30 days and groups by UTC day. It reads the
already persisted `model_responses.usage_json` rows (one row per model call)
through a read-only SQLite connection. It reports model calls, input, output,
cached input tokens, `cached/input` hit rate, and an optional cost estimate.
`--by run` uses the run id; `--by model` uses the configured OpenAI model for
OpenAI rows and the existing provider value for other rows. The current schema
does not persist a model id, so this model attribution is a documented proxy;
no new persistence is introduced by this command. Calls that fail before a
response with persisted usage exists are not included in these totals.

Costs are shown only when `model_prices` contains a matching model. Without a
matching price, the cost column is `—`. The estimate uses
`(input-cached)*input_price + cached*cached_price + output*output_price`, with
prices expressed per million tokens. When total input exceeds 100,000 tokens
and the hit rate is below 20%, the report adds a warning about a potentially
unstable prompt prefix. The 100,000-token threshold is intentionally fixed in
the command and is included in JSON warnings.

The report also includes an all-run model usage ledger. It groups persisted
model responses by their attributed model and reports `model_calls` and the
numeric `estimated_cost_usd` in `model_usage_by_model`; this ledger is useful
when the time-windowed rows need to be compared with the complete run history.
An unpriced model contributes zero to this numeric breakdown.

The oracle ledger is reported separately from response usage. It counts
`oracle.asked` events, groups calls by the exact model recorded in each event,
and estimates cost from its input/output tokens using `model_prices`. An
unpriced model contributes zero. This ledger covers all recorded runs and is
not limited by `--days`, so a run's cumulative estimate matches the history
used by `oracle_max_cost_usd`. Text output shows USD to four decimal places;
JSON provides numeric `oracle_estimated_cost_usd` and an `oracle_by_model`
array with `model`, `model_calls`, and `estimated_cost_usd` fields. Empty
ledgers return zero totals and an empty model array.

## export

```bash
cloud-harness export RUN_ID [--out path] [--sanitize|--raw] [--force] [--json] [--db path]
cloud-harness export RUN_ID --json
```

`export` reads the selected run's saved turn, model response items, tool
invocations/results, approval rows, and persistent event log. It copies the
database and any WAL into a private system temporary snapshot, then queries
that snapshot; it removes the temporary files on exit. It does not run
migrations, checkpoint or open the source database through SQLite, invoke a
provider, restore or inspect workspace files, or write under `.cloud-harness`.
Run metadata includes status and timestamps, providers,
available model attribution, and summed persisted input/output/cached-token
usage. The schema does not save a model id per response, so an OpenAI model name
is identified as coming from the current configuration; it may differ from
the model used by an older run.

The default output is a Markdown timeline on stdout. `--json` emits the same
run metadata, usage totals, and timeline entries as structured JSON.
Redaction is enabled by default; `--sanitize` makes that explicit. It masks
long `sk-` tokens, `hsurr:` values, bearer credentials, values assigned to
`api_key`, `apikey`, `token`, `secret`, or `password`, Authorization header
credentials, and the configured `OPENAI_API_KEY` value. Redaction applies to
messages, tool arguments and results, event payloads, and stored errors while
preserving the surrounding export structure. `--raw` disables all masking and
prints `exporting without redaction` to stderr.

`--out path` writes the rendered export to a new file and refuses to overwrite
an existing path. Add `--force` with `--out` to replace it. The database remains
read-only in either output mode; the database and its SQLite sidecars cannot be
used as the output path.

## review

```bash
cloud-harness review [--workspace dir] [--json] [--diff]
cloud-harness review --workspace ./project --json --diff
```

`review` summarizes the Git worktree that contains `--workspace`, which
defaults to the current directory. Markdown shows risks first, then file and
line totals and a table of paths. JSON reports `clean`, `file_count`, `staged`,
`unstaged`, `untracked`, `additions`, `deletions`, `files`, and `risks`. Group
counts can overlap when a path has both staged and unstaged edits. The total
counts each path once. Line totals add the changes from HEAD to the index,
from the index to the worktree, and from an empty file to each untracked file.
Both views display adds (`A`), modifications (`M`), deletes (`D`), and
untracked paths (`??`). Renames appear as an add and a delete. Unresolved
paths are marked `U` and counted as unstaged.

`--diff` adds the patch for staged, unstaged, and untracked files in that
order. Use `--json --diff` to receive it as a `diff` string. The patch masks
recognized credentials using the text rules from `export`. Binary changes
use Git's binary markers and contribute no line counts. Submodule commit
changes are shown at the parent path. Review the submodule worktree or a
nested untracked repository separately to inspect its files.

Risks are advisory and keep exit code 0. The report flags each binary change,
each path with more than 500 deleted lines across both comparisons, and each
changed regular file whose index or worktree version exceeds 1,000,000 bytes
(1 MB). It also scans newly added text files, including untracked ones, for
the same patterns as `export`, such as long `sk-` tokens, `hsurr:` values,
bearer credentials, and `api_key` assignments. Each suspected match reports
its path, source version, and line number. A clean worktree report says
`工作区干净` and exits 0. An unavailable Git executable, a directory outside a
Git worktree, or a failed Git read exits 1 with the reason on stderr.

The command reads the existing worktree and limits Git to read-only commands.

- `git rev-parse --show-toplevel` and `git rev-parse --git-path index` locate the worktree and index.
- `git config --null --name-only --get-regexp` identifies clean filters to disable for the review.
- `git status --porcelain=v1 -z --untracked-files=all` lists individual changes.
- `git diff --numstat` and `git diff --cached --numstat` supply line counts. `git diff --no-index --numstat` compares untracked regular files against `/dev/null`. Untracked symlinks use `readlink` to generate line counts and `new file mode 120000` patches from the link text without following file or directory targets.
- `git ls-files --stage`, `git cat-file --batch-check`, and `git cat-file blob` read staged paths, blob sizes, and newly added text.
- `git diff --patch`, `git diff --cached --patch`, and `git diff --no-index --patch` read patches only when `--diff` is set.

Git runs with optional index locking, pagers, filesystem monitor hooks,
external diff drivers, text conversion, and external clean filters disabled.
The review compares worktree contents without external clean filters. Active
split indexes return exit 1 because Git updates their shared index timestamp
even during reads.

## retain

```bash
cloud-harness retain [--days N] [--prune-runs-days N] [--dry-run] [--db path] [--workspace dir]
cloud-harness retain --prune-runs-days 7 --dry-run --db path --workspace dir
```

`--days N` controls history-index retention and defaults to `retention_days`
from configuration (`CLOUD_HARNESS_RETENTION_DAYS` overrides the file). A value
of zero disables history pruning. History pruning alone keeps runs, events,
checkpoints, and workspace snapshots.

`--prune-runs-days N` is a separate, opt-in policy; its default is zero
(disabled). A positive value selects `COMPLETED`, `FAILED`, and `CANCELLED`
runs whose `updated_at` is strictly before the UTC cutoff N calendar days ago.
It deletes those runs and their associated child rows in one SQLite transaction,
including their automatic and rewind-safety snapshot records. It also selects
orphan snapshot records with an identifiable run ID but no matching run row,
regardless of snapshot age. Records with no identifiable run association are
preserved, as are snapshots of surviving runs. Legacy run associations are
recovered from recognized automatic and `rewind-safety/...` labels.

`--dry-run` previews the enabled policies without deleting persistent rows,
removing snapshot refs, expiring reflogs, or running Git garbage collection.
The command prints JSON to stdout. With a positive `--prune-runs-days`, the
report uses these counts:

| Field | Meaning |
| --- | --- |
| `dry_run` | Whether this is a preview |
| `pruned` | History-index documents selected in a preview, or actually removed in a real pass; not the sum of FTS/exact-index rows |
| `runs_to_delete` | Number of eligible run rows; emitted in a dry-run |
| `deleted_runs`, `pruned_runs` | Aliases for run rows actually removed; both zero in a dry-run |
| `snapshots_to_delete` | Number of selected snapshot rows, including expired-run snapshots, their safety snapshots, and orphans; multiple rows may reference one tree |
| `deleted_snapshots` | Snapshot rows actually removed; zero in a dry-run |
| `unassigned_retained` | Snapshot rows across the database with no identifiable run association, which are preserved |
| `affected_workspaces` | Distinct workspace IDs among selected snapshot rows, including any whose reclamation fails |
| `reclaimed_workspaces` | Workspace reclamation batches completed without error in a real pass, even if zero objects are eligible; always zero in a dry-run |
| `reclamation_failures` | Workspace batches with an `error`, including failed estimates in a dry-run |
| `workspace_reclamation` | One entry per affected workspace, sorted by workspace ID |
| `worktree_reclamation` | Worktrees selected by the same run cutoff or already orphaned and older than it; includes run binding, `dry_run`, `removed`, and optional `error` |
| `tables`, `deleted_details` | Aliases for per-table counts; a dry-run previews only `runs` and `workspace_snapshots`, while a real pass reports actual deletions across the child tables |

`retention_days` and optional `before` describe history pruning;
`prune_runs_days` and `prune_runs_before` describe run pruning. A real run-prune
pass may also report the best-effort post-commit `wal_checkpoint` result.
With history pruning alone, the JSON contains `pruned`, `retention_days`,
and `before` (plus `dry_run: true` for a preview). If neither policy is enabled,
the command returns the existing no-op report: `pruned: 0` and `retention_days`.

### Snapshot object reclamation

After the SQLite run/snapshot deletion commits, selected snapshots are grouped
by workspace. For each group, retain opens the separate shadow Git repository
at `WORKSPACE/.cloud-harness/snapshots/WORKSPACE_ID/`, removes the selected
`refs/cloud-harness/snapshots/SNAPSHOT_ID` refs, expires its reflogs, and runs
`git gc --prune=now`. This does not restore or edit workspace files or change
the workspace's own Git repository. Surviving snapshot refs protect shared
trees and blobs from reclamation.

Each `workspace_reclamation` entry includes `workspace_id`, `workspace`
(resolved workspace path), `snapshots` (selected record count), `objects`,
`bytes`, and an optional `error`. Both preview and real-pass object counts are
estimates made before garbage collection: `objects` counts distinct objects
reachable from selected snapshot trees but not from surviving snapshot refs.
`bytes` sums their Git `objectsize` values (uncompressed object payload sizes).
Shared objects are counted once, and objects reachable from retained snapshots
are excluded. These are not measured disk savings: compression, packing, other
Git roots, and other unreachable objects can make the filesystem delta differ.

New snapshot records save their absolute workspace path. For legacy records
without one, `--workspace` (default current directory) supplies the path only
when its workspace ID matches. A missing shadow repository contributes zero
objects and bytes. Missing/mismatched workspace paths or Git errors populate
the workspace's `error` and increase `reclamation_failures`. In a real pass,
these errors also produce stderr warnings; they do not roll back committed
database deletions or make an otherwise successful retain exit nonzero. Check
the JSON failure count even when the command exits zero. In a dry-run,
`reclaimed_workspaces` stays zero even when estimates succeed.

## rewind

```bash
cloud-harness rewind RUN_ID --to SNAPSHOT_ID [--all|--code-only|--chat-only] [--yes] [--db path] [--workspace dir]
cloud-harness rewind --to SNAPSHOT_ID --chat-only --yes --db path --workspace dir RUN_ID
cloud-harness rewind --help
```

All flags must appear together before or after the run ID. `--db` follows the
usual configuration/environment rules; `--workspace` defaults to the current
directory and must identify the run's original workspace. Git is required for
code preview/restore, but not for `--chat-only`. `disable_snapshots` disables
automatic capture only; it does not disable explicit rewind or its safety
snapshot.

| Mode | Workspace files | Persistent run events and checkpoint |
| --- | --- | --- |
| `--code-only` | Restore the target tree through `Snapshotter.Restore` | Unchanged |
| `--chat-only` | Unchanged | Delete events with `seq > target.event_seq`; restore the latest whole saved checkpoint at or before that seq |
| `--all` (default) | Restore the target tree | Apply the chat rewind above |

Modes are mutually exclusive. The run and snapshot must exist, and the target
snapshot, run, and selected workspace must have the same `workspace_id`.
Chat rewind also requires a non-null target `event_seq` and an eligible saved
checkpoint. These failures return nonzero without restoring files, deleting
events, or creating a safety record. A live run lease also blocks execution.

### Preview and confirmation

Before execution, code modes compare the live workspace to the target through
`Snapshotter.Diff` and print each changed path: `A` adds/restores, `M` modifies,
and `D` deletes. Chat modes print the number and inclusive seq range of events
to delete, along with the selected checkpoint seq. `--yes` still shows this
preview.

Without `--yes`, stdin must be a terminal. The prompt is `Apply rewind? [y/N]:`;
only `y` or `yes` (case insensitive) proceeds. Empty input, EOF, and other answers
cancel. Piped input and `/dev/null` are rejected with an instruction to add
`--yes`, even if the pipe contains `y`.

### Safety snapshot and undoing code changes

After confirmation and before code restore, rewind captures the current
workspace with label `rewind-safety/RUN_ID/UTC_TIMESTAMP`. Its ID is printed
immediately and repeated in the final summary. If capture fails, no restore or
chat deletion is attempted. This safety snapshot also captures files created
since the last automatic snapshot, subject to the existing ignore rules.
Consequently, those newly captured paths are tracked for the ensuing restore.
Ignored paths and paths never captured in any snapshot retain the existing
`Restore` behavior.

To restore the pre-rewind code state, use the printed ID:

```bash
cloud-harness rewind RUN_ID --to SAFETY_SNAPSHOT_ID --code-only --yes --db path --workspace dir
```

The safety snapshot protects workspace files; deleted chat events/checkpoints
are not archived for redo. The final summary includes `mode`, `target_snapshot`,
`code_files`, `deleted_events`, and `safety_snapshot` (`none` for chat-only).
Code restore and SQLite are separate resources: chat deletion is one SQLite
transaction; if it fails after code restore, rewind attempts to restore the
safety tree. A filesystem restore failure reports an error and leaves the
printed safety ID available for recovery.

### Checkpoint scope and existing databases

Checkpoints store the full agent state (messages, continuation, observations,
tool references, and workspace snapshot markers). Migration 005 adds their
actual `event_seq`; `SaveCheckpoint` captures the current maximum event seq in
the insert statement. A new root run also saves an initial version-zero whole
checkpoint, so the first automatic workspace snapshot has a fallback.

Rewind chooses the latest checkpoint at or before the target event seq and
deletes later checkpoints and events transactionally. It does not reconstruct
an intermediate checkpoint by replaying events. The preview reports both the
target seq and selected checkpoint seq, which can differ. Existing run status,
budget, final answer, tool batches/invocations, session history indexes, memory,
and summaries remain unchanged; this command does not automatically execute
or resume the run. It is scoped to that run's event log and stored checkpoint,
not a rollback of all derived session data.

For pre-005 databases, a run's workspace binding is recovered only when its
existing `RUN_ID/batch-N/...` snapshot labels identify exactly one workspace.
Unbound or ambiguous runs are rejected. Old checkpoints without an event seq
remain readable by existing resume behavior, but cannot be selected by rewind;
timestamps and run versions are not treated as event seqs. Such a run can still
use `--code-only` when its workspace binding is known.
