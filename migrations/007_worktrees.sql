-- Worktree runs keep their database in the parent workspace. Only working
-- files move; preserve the dispatch commit and branch for later review.
ALTER TABLE runs ADD COLUMN worktree_base_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN worktree_branch TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN worktree_path TEXT NOT NULL DEFAULT '';
