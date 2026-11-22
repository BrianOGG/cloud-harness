package sqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

const worktreeRunsQuery = `SELECT id, parent_run_id, session_id, turn_id, workspace_id, status, wait_reason, budget_json, version, final_answer, error_json, created_at, updated_at, worktree_base_sha, worktree_branch, worktree_path, worktree_state FROM runs WHERE worktree_path <> '' ORDER BY created_at, id`

// ReadWorktreeRuns is unbounded (unlike the interactive ListRuns) and read-only.
// Doctor and lifecycle reconciliation must not lose records after 200 runs.
func ReadWorktreeRuns(ctx context.Context, path string) ([]storage.Run, error) {
	db, err := openReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := worktreeRunsQuery
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return nil, err
	}
	if version == 7 {
		query = strings.Replace(query, "worktree_state FROM", "'' AS worktree_state FROM", 1)
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []storage.Run{}
	for rows.Next() {
		r, err := scanRun(rows, "")
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetWorktreeState changes only the lifecycle decision, preserving the execution
// status, binding and audit history. Optimistic versioning rejects stale writes.
func (db *DB) SetWorktreeState(ctx context.Context, run storage.Run, state string) error {
	if state != "merged" && state != "discarded" {
		return fmt.Errorf("invalid worktree state %q", state)
	}
	res, err := db.sql.ExecContext(ctx, `UPDATE runs SET worktree_state = ?, version = version + 1, updated_at = ? WHERE id = ? AND version = ? AND worktree_state = '' AND worktree_path <> '' AND status IN (?, ?, ?)`,
		state, formatTime(db.now()), run.ID, run.Version, domain.RunCompleted, domain.RunFailed, domain.RunCancelled)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return domain.E(domain.CodeConflict, "worktree lifecycle requires an unchanged terminal run").WithRef(run.ID)
	}
	return nil
}
