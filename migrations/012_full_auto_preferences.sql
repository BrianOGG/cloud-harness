-- Rebuild the preferences table to extend its CHECK without changing applied 011.
CREATE TABLE session_preferences_full_auto (
  session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  approval_mode TEXT NOT NULL DEFAULT 'ask' CHECK (approval_mode IN ('ask', 'safe', 'workspace-write', 'full-auto')),
  model_id TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);

INSERT INTO session_preferences_full_auto(session_id, approval_mode, model_id, updated_at)
SELECT session_id, approval_mode, model_id, updated_at FROM session_preferences;

DROP TABLE session_preferences;
ALTER TABLE session_preferences_full_auto RENAME TO session_preferences;
