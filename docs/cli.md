# CLI command reference

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
