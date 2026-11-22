# Cloud Harness Web UI

The browser app that `cloud-harness serve` hosts at `/ui`. It starts sessions,
sends messages, shows run events, traces, and budgets, answers approvals and
questions, and shows the stats and doctor checks on the observability page.
[FRONTEND_MODULES.md](FRONTEND_MODULES.md) lists the pages and the HTTP API
each one uses.

React 19, TypeScript, Vite, Ant Design, and `@lobehub/ui`. Node.js 22 (22.12 or
later, see `.nvmrc`) and npm.

## Layout

```text
src/            app source: App.tsx, Observability.tsx, RunBudget.tsx, api.ts, budget.ts
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
(`vite.config.ts`). If `serve` runs with an HTTP token, set it for the dev
origin in the browser console, because `localStorage` is per origin and the
token pasted on `http://127.0.0.1:19421/approval` is not visible on port 5173:

```js
localStorage.setItem('cloud-harness.http-token', 'TOKEN')
```

The app is built with `base: '/ui/'`, so asset URLs in `dist/index.html` start
with `/ui/`.

## Checks

```bash
npm run lint         # oxlint src
npm test             # tsx scripts/test-events.ts and scripts/test-observability.ts
npm run build        # tsc -b, then vite build into dist/
npm run check:dist   # build, then fail if dist/ differs from what git has
```

`npm test` runs plain Node scripts that stub `fetch` and `localStorage`. They
cover event-stream parsing, API request headers, budget counters, and the
observability page states. They need no browser and no running `serve`.

CI runs `npm ci`, `npm run lint`, `npm test`, and `npm run check:dist` on
Node.js 22 for every pull request and for pushes to `main` and `dev`.

`scripts/acceptance.sh` is the manual end-to-end pass. It builds the binary,
starts a real `serve` with a scripted provider, drives headless Firefox through
the pages, and writes screenshots under `screenshots/`. It installs Firefox and
geckodriver on first use and is not part of CI.

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
