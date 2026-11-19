-- Workspace shadow Git snapshots.

CREATE TABLE workspace_snapshots (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  tree_oid TEXT NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  event_seq INTEGER,
  file_count INTEGER NOT NULL,
  total_bytes INTEGER NOT NULL,
  created_at TEXT NOT NULL
);

CREATE INDEX idx_workspace_snapshots_time ON workspace_snapshots(workspace_id, created_at, id);
