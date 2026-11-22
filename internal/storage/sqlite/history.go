package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/history"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func (db *DB) IndexHistory(ctx context.Context, doc storage.HistoryDoc) error {
	return db.indexHistory(ctx, doc, true)
}

func (db *DB) indexHistory(ctx context.Context, doc storage.HistoryDoc, commit bool) (err error) {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	meta := "{}"
	if len(doc.Metadata) > 0 {
		meta = string(doc.Metadata)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM history_exact WHERE source_id = ?`, doc.SourceID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM history_fts WHERE source_id = ?`, doc.SourceID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO history_docs(source_id, session_id, raw_text, metadata_json, created_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(source_id) DO UPDATE SET
			session_id = excluded.session_id,
			raw_text = excluded.raw_text,
			metadata_json = excluded.metadata_json,
			created_at = excluded.created_at`,
		doc.SourceID, doc.SessionID, doc.RawText, meta, formatTime(doc.CreatedAt)); err != nil {
		return err
	}
	if err = writeIndex(ctx, tx, doc.SourceID, doc.SessionID, doc.RawText); err != nil {
		return err
	}
	if !commit {
		err = errForcedRollback
		return err
	}
	err = tx.Commit()
	return err
}

var errForcedRollback = errors.New("forced rollback")

func writeIndex(ctx context.Context, tx *sql.Tx, sourceID, sessionID, raw string) error {
	analysis := history.Analyze(raw)
	for _, ex := range analysis.Exact {
		if _, err := tx.ExecContext(ctx, `INSERT INTO history_exact(term, source_id, kind) VALUES(?, ?, ?)`, ex.Term, sourceID, string(ex.Kind)); err != nil {
			return err
		}
	}
	tokens := strings.Join(analysis.FTSTokens, " ")
	_, err := tx.ExecContext(ctx, `INSERT INTO history_fts(source_id, session_id, tokens) VALUES(?, ?, ?)`, sourceID, sessionID, tokens)
	return err
}

func (db *DB) SearchHistory(ctx context.Context, sessionID, query string, limit int) ([]storage.Hit, error) {
	plan := history.PlanQuery(query)
	if plan.FTSMatch == "" && len(plan.Exact) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	seen := map[string]struct{}{}
	var hits []storage.Hit
	add := func(id, raw, kind string) {
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		hits = append(hits, storage.Hit{SourceID: id, SessionID: sessionID, RawText: snippet(raw), MatchKind: kind})
	}
	for _, term := range plan.Exact {
		rows, err := db.sql.QueryContext(ctx, `
			SELECT d.source_id, d.raw_text
			FROM history_exact e
			JOIN history_docs d ON d.source_id = e.source_id
			WHERE e.term = ? AND d.session_id = ?`, term, sessionID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, raw string
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			add(id, raw, "exact")
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	if !plan.ExactOnly && plan.FTSMatch != "" {
		rows, err := db.sql.QueryContext(ctx, `
			SELECT source_id FROM history_fts
			WHERE history_fts MATCH ? AND session_id = ?`, plan.FTSMatch, sessionID)
		if err != nil {
			return nil, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			var raw, sid string
			if err := db.sql.QueryRowContext(ctx, `SELECT session_id, raw_text FROM history_docs WHERE source_id = ?`, id).Scan(&sid, &raw); err != nil {
				return nil, err
			}
			if sid != sessionID {
				continue
			}
			add(id, raw, "fts")
		}
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func (db *DB) RebuildHistory(ctx context.Context, sessionID string) error {
	rows, err := db.sql.QueryContext(ctx, `SELECT source_id, raw_text FROM history_docs WHERE session_id = ?`, sessionID)
	if err != nil {
		return err
	}
	type doc struct{ id, raw string }
	var docs []doc
	for rows.Next() {
		var d doc
		if err := rows.Scan(&d.id, &d.raw); err != nil {
			rows.Close()
			return err
		}
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, d := range docs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM history_exact WHERE source_id = ?`, d.id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM history_fts WHERE source_id = ?`, d.id); err != nil {
			return err
		}
		if err := writeIndex(ctx, tx, d.id, sessionID, d.raw); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneHistory deletes history rows created before the cutoff. Run rows stay.
// A zero time deletes nothing. The count is the number of history documents removed.
func (db *DB) PruneHistory(ctx context.Context, before time.Time) (int, error) {
	if before.IsZero() {
		return 0, nil
	}
	rows, err := db.sql.QueryContext(ctx, `SELECT source_id FROM history_docs WHERE created_at < ?`, formatTime(before))
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM history_exact WHERE source_id = ?`, id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM history_fts WHERE source_id = ?`, id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM history_docs WHERE source_id = ?`, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// PreviewPruneHistory reports the number of history rows that meet the cutoff
// without changing the database.
func (db *DB) PreviewPruneHistory(ctx context.Context, before time.Time) (int, error) {
	if before.IsZero() {
		return 0, nil
	}
	var count int
	err := db.sql.QueryRowContext(ctx, `SELECT COUNT(1) FROM history_docs WHERE created_at < ?`, formatTime(before)).Scan(&count)
	return count, err
}

// DeleteHistory removes the given source_ids from history_docs, history_exact, and history_fts.
func (db *DB) DeleteHistory(ctx context.Context, sourceIDs ...string) error {
	if len(sourceIDs) == 0 {
		return nil
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range sourceIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM history_exact WHERE source_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM history_fts WHERE source_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM history_docs WHERE source_id = ?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func snippet(s string) string {
	rs := []rune(s)
	if len(rs) <= 240 {
		return s
	}
	return string(rs[:240])
}
