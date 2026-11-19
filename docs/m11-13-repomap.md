# M11-13 Repo-Map context assembly

Run context now includes a bounded Repo-Map when the workspace has supported,
parseable source files. It follows the project rules section and precedes the
dynamic memory/history context. The map uses the existing `internal/repomap`
scanner and renderer, has an 8 KiB limit, and retains the renderer's
`# TRUNCATED omitted_files=N` marker. Empty maps add no section.

The map is rebuilt before every executor model request, so workspace changes
from an earlier tool call are reflected on the next request. It stays outside
the stable prefix and is not written into checkpoints. `run --no-repomap`
disables the map; scan failures skip it and append a `repomap.skipped`
diagnostic event without failing the run. Per-file parse/read diagnostics are
recorded as `repomap.diagnostics` while successfully mapped files remain
available.

Tests cover ordering after project rules, equality with the `repomap` CLI
renderer, Go and Python inputs, truncation, dynamic regeneration, disabled and
empty workspaces, scan failure isolation, and the CLI opt-out flag.

The full acceptance command output and exit codes are recorded in
`.cloud-harness/m11-13/acceptance-final.log`.
