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
	if after != nil && after.ID == "" {
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
		// A non-nil cursor carries a logical boundary, including year one.
		// formatTime treats a zero time as an omitted write timestamp.
		created := after.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
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

func (db *DB) SyncMessages(ctx context.Context, sessionID string, generation, revision int64, limit int) (storage.MessageSyncPage, error) {
	var page storage.MessageSyncPage
	if limit < 1 || limit > maxMessagePageSize {
		return page, domain.E(domain.CodeInvalidArgument, "invalid sync page size")
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0) FROM message_sync WHERE session_id = ? AND message_id = ''`, sessionID).Scan(&page.Generation); err != nil {
		return page, err
	}
	page.Revision = revision
	if generation != page.Generation {
		page.Reset = true
		page.Revision = 0
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT s.revision, m.id, m.session_id, m.run_id, m.turn_id, m.role, m.source, m.kind, m.status, m.content, m.event, m.created_at
		FROM message_sync s JOIN messages m ON m.id = s.message_id
		WHERE s.session_id = ? AND s.revision > ? ORDER BY s.revision LIMIT ?`, sessionID, page.Revision, limit+1)
	if err != nil {
		return page, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if len(page.Messages) == limit {
			page.More = true
			break
		}
		var m storage.Message
		var created string
		if err := rows.Scan(&page.Revision, &m.ID, &m.SessionID, &m.RunID, &m.TurnID, &m.Role, &m.Source, &m.Kind, &m.Status, &m.Content, &m.Event, &created); err != nil {
			return page, err
		}
		m.CreatedAt, err = parseTime(created)
		if err != nil {
			return page, err
		}
		page.Messages = append(page.Messages, m)
	}
	return page, rows.Err()
}
