package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/cloud-harness/cloud-harness/internal/storage"
)

func (db *DB) GetHistory(ctx context.Context, sourceID string) (storage.HistoryDoc, error) {
	var doc storage.HistoryDoc
	var meta, created string
	err := db.sql.QueryRowContext(ctx, `
		SELECT source_id, session_id, raw_text, metadata_json, created_at
		FROM history_docs WHERE source_id = ?`, sourceID).Scan(&doc.SourceID, &doc.SessionID, &doc.RawText, &meta, &created)
	if err != nil {
		return storage.HistoryDoc{}, mapErr(err, sourceID)
	}
	if meta != "" && meta != "null" {
		doc.Metadata = json.RawMessage(meta)
	}
	doc.CreatedAt, _ = parseTime(created)
	return doc, nil
}

func (db *DB) LatestMemory(ctx context.Context, sessionID string) (string, int, error) {
	var state string
	var version int
	err := db.sql.QueryRowContext(ctx, `
		SELECT state_json, version FROM session_memories
		WHERE session_id = ? ORDER BY version DESC LIMIT 1`, sessionID).Scan(&state, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	return state, version, nil
}

func (db *DB) CountTurns(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns WHERE session_id = ?`, sessionID).Scan(&n)
	return n, err
}

func (db *DB) SaveSummaryJob(ctx context.Context, job storage.SummaryJob) error {
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO summary_jobs(id, session_id, turn_id, run_id, status, attempts, last_error, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.SessionID, job.TurnID, job.RunID, job.Status, job.Attempts, job.LastError,
		formatTime(job.CreatedAt), formatTime(job.UpdatedAt))
	return err
}

func (db *DB) ListOpenSummaryJobs(ctx context.Context, sessionID string) ([]storage.SummaryJob, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, session_id, turn_id, run_id, status, attempts, last_error, created_at, updated_at
		FROM summary_jobs
		WHERE session_id = ? AND status != 'done' AND attempts < 3
		ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.SummaryJob
	for rows.Next() {
		var job storage.SummaryJob
		var created, updated string
		if err := rows.Scan(&job.ID, &job.SessionID, &job.TurnID, &job.RunID, &job.Status, &job.Attempts, &job.LastError, &created, &updated); err != nil {
			return nil, err
		}
		job.CreatedAt, _ = parseTime(created)
		job.UpdatedAt, _ = parseTime(updated)
		out = append(out, job)
	}
	return out, rows.Err()
}

func (db *DB) UpdateSummaryJob(ctx context.Context, job storage.SummaryJob) error {
	res, err := db.sql.ExecContext(ctx, `
		UPDATE summary_jobs SET status = ?, attempts = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		job.Status, job.Attempts, job.LastError, formatTime(job.UpdatedAt), job.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return mapErr(sql.ErrNoRows, job.ID)
	}
	return nil
}
