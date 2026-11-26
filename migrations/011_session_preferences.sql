CREATE TABLE session_preferences (
  session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  approval_mode TEXT NOT NULL DEFAULT 'ask' CHECK (approval_mode IN ('ask', 'safe', 'workspace-write')),
  model_id TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);
