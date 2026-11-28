package sqlite

import (
	"context"
	"database/sql"

	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func (db *DB) GetSessionPreferences(ctx context.Context, sessionID string) (storage.SessionPreferences, error) {
	if _, err := db.GetSession(ctx, sessionID); err != nil {
		return storage.SessionPreferences{}, err
	}
	p := storage.SessionPreferences{SessionID: sessionID, ApprovalMode: "ask", ModelID: "default", ReasoningEffort: "default"}
	var updated string
	err := db.sql.QueryRowContext(ctx, `SELECT approval_mode, model_id, reasoning_effort, updated_at FROM session_preferences WHERE session_id = ?`, sessionID).Scan(&p.ApprovalMode, &p.ModelID, &p.ReasoningEffort, &updated)
	if err == sql.ErrNoRows {
		return p, nil
	}
	if err != nil {
		return storage.SessionPreferences{}, err
	}
	p.UpdatedAt, err = parseTime(updated)
	return p, err
}

func (db *DB) SaveSessionPreferences(ctx context.Context, p storage.SessionPreferences) error {
	if _, err := db.GetSession(ctx, p.SessionID); err != nil {
		return err
	}
	return db.saveSessionPreferences(ctx, db.sql, p)
}

func (db *DB) saveSessionPreferences(ctx context.Context, writer txer, p storage.SessionPreferences) error {
	if p.ReasoningEffort == "" {
		p.ReasoningEffort = "default"
	}
	_, err := writer.ExecContext(ctx, `INSERT INTO session_preferences(session_id, approval_mode, model_id, reasoning_effort, updated_at) VALUES(?, ?, ?, ?, ?) ON CONFLICT(session_id) DO UPDATE SET approval_mode=excluded.approval_mode, model_id=excluded.model_id, reasoning_effort=excluded.reasoning_effort, updated_at=excluded.updated_at`, p.SessionID, p.ApprovalMode, p.ModelID, p.ReasoningEffort, formatTime(db.now()))
	return err
}
