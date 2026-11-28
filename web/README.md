# Cloud Harness Web UI

The browser app that `cloud-harness serve` hosts at `/ui`. It is a chat
interface: a sidebar of conversations, a thread where each message is one run,
and a composer. A turn shows the run's model steps and tool calls above its
Markdown answer, and asks for approvals, answers, and budget in place. A drawer
shows a run's budget, trace, and events. Approvals and observability (stats and
doctor checks) are sidebar pages.
[FRONTEND_MODULES.md](FRONTEND_MODULES.md) lists the modules and the HTTP API
each one uses.

React 19, TypeScript, Vite, and `react-markdown`. Node.js 22 (22.12 or later,
see `.nvmrc`) and npm.

## Layout

```text
src/            app source
  App.tsx         state, sidebar, pages, run details drawer
  Chat.tsx        a turn (work block, wait cards, answer) and the composer
  Markdown.tsx    Markdown rendering for model output
  timeline.ts     run events -> the rows of a turn's work block
  api.ts          HTTP client and event stream reader
  Observability.tsx, RunBudget.tsx, budget.ts, format.ts, Icon.tsx
public/         files copied into dist/ unchanged
scripts/        unit tests, the dist check, and the browser acceptance scripts
screenshots/    acceptance screenshots, by milestone
dist/           checked-in production build, embedded into the Go binary by embed.go
ui_test.go      Go test that serves the embedded dist/ through the HTTP handler
```

## Develop

```bash
# terminal 1, from the repository root
go build -o cloud-harness ./cmd/cloud-harness
./cloud-harness serve

# terminal 2
cd web
npm ci
npm run dev
```

Open `http://127.0.0.1:5173/ui/`. The API sends no CORS headers, so the Vite
dev server proxies `/v1` and `/healthz` to `serve` on `127.0.0.1:19421`
(`vite.config.ts`). The proxy forwards the browser's `Host` unchanged
(`changeOrigin: false`), because `serve` refuses a request whose `Origin`
differs from its `Host`. If `serve` runs with an HTTP token, set it for the dev
origin in the browser console, because `localStorage` is per origin and the
token pasted on `http://127.0.0.1:19421/approval` is not visible on port 5173:

```js
localStorage.setItem('cloud-harness.http-token', 'TOKEN')
```

The app is built with `base: '/ui/'`, so asset URLs in `dist/index.html` start
with `/ui/`.

### Without a model key

The default `mock` provider answers in one step by repeating the message.
`scripts/demo-provider.py` is an OpenAI-compatible provider with canned replies
that exercises tool calls, approval, `ask_user`, a child run, and a failed run,
picked by words in the message (the welcome screen suggests one of each):

```bash
python3 web/scripts/demo-provider.py          # listens on 127.0.0.1:19422

CLOUD_HARNESS_PROVIDER=openai OPENAI_API_KEY=demo OPENAI_MODEL=demo \
OPENAI_BASE_URL=http://127.0.0.1:19422/v1 ./cloud-harness serve
```

## 在对话中使用技能

在新对话或已有对话的输入框中键入 `/`，查看工作区与全局技能的名称和简介。
继续输入名称可筛选技能。用 ↑、↓ 移动，Enter、Tab 或点击条目可将
`/skill 名称 ` 插入草稿。按 Esc 收起列表。补充任务后点击发送，例如
`/skill review 检查新接口的错误处理`。也可以直接输入完整命令。
发送时使用 CLI 的加载规则读取所选技能，保留当前模型与审批设置。

## How a run is followed

- `POST /v1/sessions/{id}/runs` can return only when the run stops. While it
  is pending the app polls the session's run list, adopts the new run, and
  follows it, so steps show up before the POST returns. A response that
  arrives while the run is still `RUNNING` is followed the same way.
- Each unfinished run of the open conversation has one open
  `GET /v1/runs/{id}/events` stream, reconnected with `Last-Event-ID`, and its
  run view is polled once a second. If the stream is silent while the run's
  version changes (a proxy that buffers), the events are read with short
  requests instead.
- A `stream.delta` block (no `id`, payload `{"text": "..."}`) is appended to
  the turn as it arrives and replaced by `final_answer` when the run ends.
- serve does not return message text. The app keeps what this browser sent in
  `localStorage` (`cloud-harness.web-messages`, `-answers`, `-titles`); a run
  started elsewhere shows a note in place of its message.
- Runs with a `parent_id` are child runs. They are not turns; the parent turn
  lists them.

## Checks

```bash
npm run lint         # oxlint src
npm test             # tsx scripts/test-*.ts
npm run build        # tsc -b, then vite build into dist/
npm run check:dist   # build, then fail if dist/ differs from what git has
```

`npm test` runs plain Node scripts that stub `fetch` and `localStorage`. They
cover event-stream parsing and following, the work block built from run
events, API request headers, budget counters, and the observability page
states. They need no browser and no running `serve`.

CI runs `npm ci`, `npm run lint`, `npm test`, and `npm run check:dist` on
Node.js 22 for every pull request and for pushes to `main` and `dev`.

`scripts/acceptance.sh` is the manual end-to-end pass. It builds the binary,
starts a real `serve` with a scripted provider, drives headless Firefox through
the pages, and writes screenshots under `screenshots/chat/`. It installs Firefox and
geckodriver on first use and is not part of CI.

The browser pass reads the complete `/v1/sessions/{id}/messages` cursor chain
and compares rendered message IDs, run IDs, kinds, and statuses with the server.
It checks persisted inputs, replies, questions, answers, approval decisions,
child-run history, refresh recovery, and session switching. It does not seed
message text in localStorage. The capture script gets the temporary HTTP token
from `CLOUD_HARNESS_HTTP_TOKEN`, never from command arguments.

For cloud-harness acceptance, CH-PM runs the browser pass and final visual and
second-device checks directly on Osaka, after verifying `hostname` is `osaka`.
The scripted provider pass is separate from real Agnes model acceptance.

## `dist/` is checked in

`go build` embeds `web/dist/` (`embed.go`, `//go:embed all:dist`), so a Go
toolchain alone produces a binary with the UI. Node.js is only needed to change
the UI.

After changing anything that affects the build (`src/`, `public/`,
`index.html`, `vite.config.ts`, `package.json`, `package-lock.json`):

```bash
npm ci
npm run build
git add dist
```

Commit `dist/` in the same change as the source. Then rebuild the Go binary to
serve the new assets.

Vite names the JS and CSS files by content hash, and the build is
reproducible: the same source and `package-lock.json` give byte-identical
output. CI rebuilds and fails the `web` job when `dist/` has modified, missing,
or extra files, with this message:

```text
web/dist does not match a fresh build of the Web UI source:
```

Build with `npm ci`, not `npm install`. A different dependency version changes
the bundle and its hash. Do not edit files in `dist/` by hand.
