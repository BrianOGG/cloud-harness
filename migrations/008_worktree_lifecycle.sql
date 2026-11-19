-- Only terminal lifecycle decisions are stored on the existing run record.
-- Running/pending and orphans are derived by reconciling runs with Git.
ALTER TABLE runs ADD COLUMN worktree_state TEXT NOT NULL DEFAULT '' CHECK(worktree_state IN ('', 'merged', 'discarded'));
