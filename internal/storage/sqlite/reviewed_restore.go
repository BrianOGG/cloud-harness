package sqlite

import (
	"context"
	"os"
	"path/filepath"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

// WithReviewedRestore reserves the SQLite writer before checking activity.
// A second process can't create/resume a run or start a lifecycle operation
// between this check and the filesystem replacement. No DB state is changed.
func (db *DB) WithReviewedRestore(ctx context.Context, before, after storage.WorkspaceSnapshot, path, owner string, restore func() error) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := formatTime(db.now())
	res, err := tx.ExecContext(ctx, `UPDATE run_leases SET owner_id=owner_id WHERE run_id=? AND owner_id=? AND expires_at>?`, before.RunID, owner, now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return domain.E(domain.CodeConflict, "还原操作已过期，请刷新")
	}
	var available bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE id=? AND workspace_id=? AND worktree_state='' AND status IN ('COMPLETED','FAILED','CANCELLED'))
	AND NOT EXISTS(SELECT 1 FROM runs WHERE workspace_id=? AND worktree_state<>'')
	AND (SELECT COUNT(*) FROM workspace_snapshots WHERE run_id=? AND workspace_id=? AND id IN (?,?))=2`, before.RunID, before.WorkspaceID, before.WorkspaceID, before.RunID, before.WorkspaceID, before.ID, after.ID).Scan(&available)
	if err != nil {
		return err
	}
	if !available {
		return domain.E(domain.CodeConflict, "运行或快照状态已改变，请刷新")
	}
	var busy bool
	prefix := filepath.Join(before.WorkspacePath, ".cloud-harness", "worktrees") + string(filepath.Separator)
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs r LEFT JOIN run_leases l ON l.run_id=r.id
	WHERE r.id<>? AND (((r.workspace_id=? OR r.workspace_id='') AND (r.status IN ('RUNNING','WAITING') OR l.expires_at>?))
	OR (l.owner_id GLOB 'lifecycle_*' AND l.expires_at>? AND substr(r.worktree_path,1,length(?))=?)))`,
		before.RunID, before.WorkspaceID, now, now, prefix, prefix).Scan(&busy)
	if err != nil {
		return err
	}
	if busy {
		return domain.E(domain.CodeConflict, "工作区有其他运行或工作树操作，请稍后再试")
	}
	// A non-default --db path can put the live database and its sidecars
	// among ordinary workspace snapshots.
	var databasePath string
	if err := tx.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name='main'").Scan(&databasePath); err != nil {
		return err
	}
	target := filepath.Join(before.WorkspacePath, filepath.FromSlash(path))
	targetInfo, _ := os.Stat(target)
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		protected := databasePath + suffix
		protectedInfo, _ := os.Stat(protected)
		if target == protected || (targetInfo != nil && protectedInfo != nil && os.SameFile(targetInfo, protectedInfo)) {
			return domain.E(domain.CodeConflict, "无法还原正在使用的运行数据库")
		}
	}
	return restore()
}
