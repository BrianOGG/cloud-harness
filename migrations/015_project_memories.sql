-- workspace_id remains the execution directory used by snapshots. project_id
-- binds new runs to their originating project even after a worktree is removed.
ALTER TABLE runs ADD COLUMN project_id TEXT NOT NULL DEFAULT '';

CREATE TABLE project_memories (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL CHECK (project_id <> ''),
  kind TEXT NOT NULL CHECK (kind IN ('fact', 'decision')),
  content TEXT NOT NULL CHECK (length(content) BETWEEN 1 AND 500),
  -- Keep provenance and the user's confirmed text when run history is pruned.
  source_session_id TEXT NOT NULL,
  source_run_id TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX idx_project_memories_recent ON project_memories(project_id, updated_at DESC, id DESC);
