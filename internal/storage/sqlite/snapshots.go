package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

// SnapshotRunBounds returns the possible execution lifetime in this workspace.
// Cancellation can precede the executor's last write. An outstanding invocation
// leaves the end unknown even after lease release. Otherwise include events
// committed as the worker unwound, which can postdate the cancellation.
func (db *DB) SnapshotRunBounds(ctx context.Context, workspaceID, runID string) (start, end time.Time, err error) {
	var created, ended string
	err = db.sql.QueryRowContext(ctx, `SELECT r.created_at,
		CASE WHEN l.expires_at > ? THEN ''
		WHEN r.status IN ('COMPLETED','FAILED') THEN r.updated_at
		WHEN r.status = 'CANCELLED' AND NOT EXISTS
		  (SELECT 1 FROM tool_invocations WHERE run_id = r.id AND status = 'RUNNING')
		THEN MAX(r.updated_at, COALESCE(l.expires_at, ''),
		  COALESCE((SELECT MAX(created_at) FROM events WHERE run_id = r.id), ''))
		ELSE '' END
		FROM runs r LEFT JOIN run_leases l ON l.run_id = r.id
		WHERE r.id = ? AND r.workspace_id = ?`, formatTime(db.now()), runID, workspaceID).Scan(&created, &ended)
	if err != nil {
		return
	}
	if start, err = parseTime(created); err == nil {
		end, err = parseTime(ended)
	}
	return
}

// CreateWorkspaceSnapshot persists a captured shadow tree. Missing IDs and
// timestamps are assigned using the store's standard conventions.
func (db *DB) CreateWorkspaceSnapshot(ctx context.Context, rec storage.WorkspaceSnapshot) (storage.WorkspaceSnapshot, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID("snap_")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = db.now()
	}
	var eventSeq any
	if rec.EventSeq != nil {
		eventSeq = *rec.EventSeq
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO workspace_snapshots(id, run_id, workspace_id, workspace_path, tree_oid, label, event_seq, file_count, total_bytes, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.RunID, rec.WorkspaceID, rec.WorkspacePath, rec.TreeOID, rec.Label, eventSeq, rec.FileCount, rec.TotalBytes, formatTime(rec.CreatedAt))
	return rec, err
}

// ListWorkspaceSnapshots returns a workspace's records in chronological order.
func (db *DB) ListWorkspaceSnapshots(ctx context.Context, workspaceID string) ([]storage.WorkspaceSnapshot, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, run_id, workspace_id, workspace_path, tree_oid, label, event_seq, file_count, total_bytes, created_at
		FROM workspace_snapshots WHERE workspace_id = ? ORDER BY created_at ASC, id ASC`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []storage.WorkspaceSnapshot
	for rows.Next() {
		rec, err := scanWorkspaceSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// GetWorkspaceSnapshot retrieves a record by its primary key.
func (db *DB) GetWorkspaceSnapshot(ctx context.Context, id string) (storage.WorkspaceSnapshot, error) {
	rec, err := scanWorkspaceSnapshot(db.sql.QueryRowContext(ctx, `
		SELECT id, run_id, workspace_id, workspace_path, tree_oid, label, event_seq, file_count, total_bytes, created_at
		FROM workspace_snapshots WHERE id = ?`, id))
	if err != nil {
		return storage.WorkspaceSnapshot{}, mapErr(err, id)
	}
	return rec, nil
}

// DeleteWorkspaceSnapshot removes one record by ID.
func (db *DB) DeleteWorkspaceSnapshot(ctx context.Context, id string) error {
	return deleteWorkspaceSnapshot(ctx, db.sql, `DELETE FROM workspace_snapshots WHERE id = ?`, id)
}

// DeleteWorkspaceSnapshotForWorkspace removes one record only when it belongs
// to the specified workspace.
func (db *DB) DeleteWorkspaceSnapshotForWorkspace(ctx context.Context, workspaceID, id string) error {
	return deleteWorkspaceSnapshot(ctx, db.sql, `DELETE FROM workspace_snapshots WHERE workspace_id = ? AND id = ?`, workspaceID, id)
}

// DeleteWorkspaceSnapshotsByWorkspace removes all records for a workspace.
func (db *DB) DeleteWorkspaceSnapshotsByWorkspace(ctx context.Context, workspaceID string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM workspace_snapshots WHERE workspace_id = ?`, workspaceID)
	return err
}

type snapshotScanner interface {
	Scan(dest ...any) error
}

func scanWorkspaceSnapshot(row snapshotScanner) (storage.WorkspaceSnapshot, error) {
	var rec storage.WorkspaceSnapshot
	var eventSeq sql.NullInt64
	var created string
	if err := row.Scan(&rec.ID, &rec.RunID, &rec.WorkspaceID, &rec.WorkspacePath, &rec.TreeOID, &rec.Label, &eventSeq, &rec.FileCount, &rec.TotalBytes, &created); err != nil {
		return storage.WorkspaceSnapshot{}, err
	}
	if eventSeq.Valid {
		rec.EventSeq = &eventSeq.Int64
	}
	rec.CreatedAt, _ = parseTime(created)
	return rec, nil
}

func deleteWorkspaceSnapshot(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, query string, args ...any) error {
	res, err := exec.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return mapErr(sql.ErrNoRows, "snapshot")
	}
	return nil
}
