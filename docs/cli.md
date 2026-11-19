# CLI command reference

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
