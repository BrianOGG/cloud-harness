package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

const (
	defaultMessagePageSize = 50
	maxMessagePageSize     = 200
)

func (db *DB) ListMessages(ctx context.Context, sessionID string, after *storage.MessageCursor, limit int) (storage.MessagePage, error) {
	if limit <= 0 {
		limit = defaultMessagePageSize
	}
	if limit > maxMessagePageSize {
		return storage.MessagePage{}, domain.E(domain.CodeInvalidArgument, "message limit must be at most 200")
	}
	if after != nil && (after.ID == "" || after.CreatedAt.IsZero()) {
		return storage.MessagePage{}, domain.E(domain.CodeInvalidArgument, "invalid message cursor")
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return storage.MessagePage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	query := `
		SELECT id, session_id, run_id, turn_id, role, source, kind, status, content, event, created_at
		FROM messages
		WHERE session_id = ?`
	args := []any{sessionID}
	if after != nil {
		created := formatTime(after.CreatedAt)
		query += ` AND (created_at > ? OR (created_at = ? AND id > ?))`
		args = append(args, created, created, after.ID)
	}
	query += ` ORDER BY created_at ASC, id ASC LIMIT ?`
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return storage.MessagePage{}, err
	}
	messages := make([]storage.Message, 0, limit)
	for rows.Next() {
		var message storage.Message
		var created string
		if err := rows.Scan(&message.ID, &message.SessionID, &message.RunID, &message.TurnID, &message.Role, &message.Source, &message.Kind, &message.Status, &message.Content, &message.Event, &created); err != nil {
			_ = rows.Close()
			return storage.MessagePage{}, err
		}
		message.CreatedAt, err = parseTime(created)
		if err != nil {
			_ = rows.Close()
			return storage.MessagePage{}, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return storage.MessagePage{}, err
	}
	if err := rows.Close(); err != nil {
		return storage.MessagePage{}, err
	}
	page := storage.MessagePage{Messages: messages}
	if len(messages) > limit {
		last := messages[limit-1]
		page.Messages = messages[:limit]
		page.NextCursor = &storage.MessageCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func insertMessage(ctx context.Context, tx txer, message storage.Message) error {
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	}
	_, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO messages(id, session_id, run_id, turn_id, role, source, kind, status, content, event, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		message.ID, message.SessionID, message.RunID, message.TurnID, message.Role, message.Source, message.Kind, message.Status, message.Content, message.Event, formatTime(message.CreatedAt))
	return err
}

type txer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}
