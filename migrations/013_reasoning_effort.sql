ALTER TABLE session_preferences ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT 'default' CHECK (reasoning_effort IN ('default', 'off', 'low', 'medium', 'high'));
