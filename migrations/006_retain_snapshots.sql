-- Bind shadow snapshots to their run and absolute workspace location so
-- retain can remove records atomically and reclaim the matching Git objects.

ALTER TABLE workspace_snapshots ADD COLUMN run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE workspace_snapshots ADD COLUMN workspace_path TEXT NOT NULL DEFAULT '';

UPDATE workspace_snapshots
SET run_id = CASE
  WHEN label LIKE 'rewind-safety/%' THEN substr(
    substr(label, length('rewind-safety/') + 1),
    1,
    instr(substr(label, length('rewind-safety/') + 1), '/') - 1
  )
  ELSE substr(label, 1, instr(label, '/') - 1)
END
WHERE label LIKE 'rewind-safety/%' OR label LIKE 'run_%/%';

CREATE INDEX idx_workspace_snapshots_run ON workspace_snapshots(run_id, workspace_id);
