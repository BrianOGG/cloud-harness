package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"unicode/utf8"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func (db *DB) ListProjectMemories(ctx context.Context, projectID string, limit int) ([]storage.ProjectMemory, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := db.sql.QueryContext(ctx, `SELECT m.id, m.kind, m.content, m.source_session_id, m.source_run_id, m.updated_at,
		EXISTS(SELECT 1 FROM runs r WHERE r.id = m.source_run_id AND r.session_id = m.source_session_id)
		FROM project_memories m WHERE m.project_id = ? ORDER BY m.updated_at DESC, m.id DESC LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := []storage.ProjectMemory{}
	for rows.Next() {
		var m storage.ProjectMemory
		if err := rows.Scan(&m.ID, &m.Kind, &m.Content, &m.SourceSessionID, &m.SourceRunID, &m.UpdatedAt, &m.SourceAvailable); err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	return items, rows.Err()
}

func memoryContent(kind, content string) (string, error) {
	content = strings.TrimSpace(content)
	if kind != "fact" && kind != "decision" {
		return "", domain.E(domain.CodeInvalidArgument, "请选择事实或决定")
	}
	if content == "" || utf8.RuneCountInString(content) > 500 || strings.ContainsRune(content, 0) {
		return "", domain.E(domain.CodeInvalidArgument, "请输入 1 到 500 字的记忆")
	}
	return content, nil
}

// source was resolved against the server's workspace by the caller. Match its
// immutable binding again in the insert so a pruned source cannot be promoted.
func (db *DB) CreateProjectMemory(ctx context.Context, projectID string, source storage.Run, kind, content string) (storage.ProjectMemory, error) {
	content, err := memoryContent(kind, content)
	if err != nil {
		return storage.ProjectMemory{}, err
	}
	m := storage.ProjectMemory{ID: domain.NewID("mem_"), Kind: kind, Content: content,
		SourceSessionID: source.SessionID, SourceRunID: source.ID, SourceAvailable: true, UpdatedAt: formatTime(db.now())}
	result, err := db.sql.ExecContext(ctx, `INSERT INTO project_memories(id, project_id, kind, content, source_session_id, source_run_id, updated_at)
		SELECT ?, ?, ?, ?, session_id, id, ? FROM runs WHERE id = ? AND session_id = ? AND
		(project_id = ? OR (project_id = '' AND workspace_id = ? AND workspace_id <> ''))`,
		m.ID, projectID, kind, content, m.UpdatedAt, source.ID, source.SessionID, projectID, source.WorkspaceID)
	if err = memoryAffected(result, err); err != nil {
		return storage.ProjectMemory{}, err
	}
	return m, nil
}

func (db *DB) UpdateProjectMemory(ctx context.Context, projectID, id, kind, content string) error {
	content, err := memoryContent(kind, content)
	if err != nil {
		return err
	}
	return memoryAffected(db.sql.ExecContext(ctx, `UPDATE project_memories SET kind = ?, content = ?, updated_at = ? WHERE project_id = ? AND id = ?`,
		kind, content, formatTime(db.now()), projectID, id))
}

func (db *DB) DeleteProjectMemory(ctx context.Context, projectID, id string) error {
	return memoryAffected(db.sql.ExecContext(ctx, `DELETE FROM project_memories WHERE project_id = ? AND id = ?`, projectID, id))
}

func memoryAffected(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return domain.E(domain.CodeNotFound, "未找到此项目中的记忆或来源")
	}
	return err
}
