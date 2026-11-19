-- Bind runs and checkpoints to the workspace/event boundary used by rewind.

ALTER TABLE runs ADD COLUMN workspace_id TEXT NOT NULL DEFAULT '';

ALTER TABLE checkpoints ADD COLUMN event_seq INTEGER;

CREATE INDEX idx_checkpoints_run_event ON checkpoints(run_id, event_seq, version);

UPDATE runs
SET workspace_id = COALESCE((
  SELECT MIN(workspace_id) FROM workspace_snapshots
  WHERE substr(workspace_snapshots.label, 1, length(runs.id) + 1) = runs.id || '/'
  HAVING COUNT(DISTINCT workspace_id) = 1
), '')
WHERE workspace_id = '';
