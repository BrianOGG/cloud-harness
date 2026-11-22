package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/storage"
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

func (db *DB) RecentTurnSummaries(ctx context.Context, sessionID string, limit int) ([]storage.TurnSummary, error) {
	if limit <= 0 {
		limit = 3
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT ts.turn_id, ts.summary, ts.source_refs, t.created_at
		FROM turn_summaries ts
		JOIN turns t ON t.id = ts.turn_id
		WHERE t.session_id = ?
		ORDER BY t.created_at DESC, ts.rowid DESC
		LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.TurnSummary
	for rows.Next() {
		var ts storage.TurnSummary
		var created string
		if err := rows.Scan(&ts.TurnID, &ts.Summary, &ts.SourceRefs, &created); err != nil {
			return nil, err
		}
		ts.CreatedAt, _ = parseTime(created)
		out = append(out, ts)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (db *DB) ListHistoryCatalog(ctx context.Context, sessionID string, limit int) ([]storage.HistoryCatalogEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT source_id, raw_text, metadata_json, created_at
		FROM history_docs
		WHERE session_id = ?
		ORDER BY created_at DESC, rowid DESC
		LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []storage.HistoryCatalogEntry
	for rows.Next() {
		var id, raw, meta, created string
		if err := rows.Scan(&id, &raw, &meta, &created); err != nil {
			rows.Close()
			return nil, err
		}
		createdAt, _ := parseTime(created)
		seen[id] = true
		out = append(out, storage.HistoryCatalogEntry{
			SourceID:  id,
			Kind:      extractDocKind(id, meta),
			CreatedAt: createdAt,
			Summary:   catalogExcerpt(raw, 80),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	sumRows, err := db.sql.QueryContext(ctx, `
		SELECT ts.turn_id, ts.summary, t.created_at
		FROM turn_summaries ts
		JOIN turns t ON t.id = ts.turn_id
		WHERE t.session_id = ?
		ORDER BY t.created_at DESC, ts.rowid DESC
		LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer sumRows.Close()
	for sumRows.Next() {
		var turnID, summary, created string
		if err := sumRows.Scan(&turnID, &summary, &created); err != nil {
			return nil, err
		}
		createdAt, _ := parseTime(created)
		sourceID := "sum_" + turnID
		if !seen[turnID] {
			sourceID = turnID
		} else if seen[sourceID] {
			continue
		}
		seen[sourceID] = true
		out = append(out, storage.HistoryCatalogEntry{
			SourceID:  sourceID,
			Kind:      "turn_summary",
			CreatedAt: createdAt,
			Summary:   catalogExcerpt(summary, 80),
		})
	}
	if err := sumRows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func extractDocKind(id, meta string) string {
	if meta != "" && meta != "null" {
		var m map[string]string
		if err := json.Unmarshal([]byte(meta), &m); err == nil && m["kind"] != "" {
			return m["kind"]
		}
	}
	switch {
	case strings.HasPrefix(id, "sum_"):
		return "summary"
	case strings.HasPrefix(id, "trn_"):
		return "user"
	case strings.HasPrefix(id, "inv_"):
		return "tool"
	case strings.HasPrefix(id, "ans_"):
		return "answer"
	case strings.HasPrefix(id, "rsp_"):
		return "model"
	default:
		return "doc"
	}
}

func catalogExcerpt(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	rs := []rune(s)
	if len(rs) <= maxRunes {
		return s
	}
	return string(rs[:maxRunes])
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
