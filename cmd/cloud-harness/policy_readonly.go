package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"path/filepath"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/storage"
)

// policyAuditReader adapts the existing ListEvents contract for CLI/doctor's
// read-only connection. The normal sqlite.Open configures WAL and migrates;
// this adapter runs only SELECTs and reads only IDs and decision events.
// Keeping it here leaves the storage API, schema and migrations unchanged.
type policyAuditReader struct {
	db *sql.DB
}

var _ agent.PolicyAuditStore = (*policyAuditReader)(nil)

func newPolicyAuditReader(ctx context.Context, path string) (*policyAuditReader, error) {
	db, err := openDiagnosticsDB(ctx, path)
	if err != nil {
		return nil, err
	}
	return &policyAuditReader{db: db}, nil
}

func openDiagnosticsDB(ctx context.Context, path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: abs}
	query := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(1000)"}}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func (r *policyAuditReader) Close() error { return r.db.Close() }

func (r *policyAuditReader) ListSessions(ctx context.Context) ([]storage.SessionSummary, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM sessions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.SessionSummary
	for rows.Next() {
		var session storage.SessionSummary
		if err := rows.Scan(&session.ID); err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

func (r *policyAuditReader) ListRuns(ctx context.Context, sessionID string) ([]storage.Run, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM runs WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.Run
	for rows.Next() {
		var run storage.Run
		if err := rows.Scan(&run.ID); err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (r *policyAuditReader) ListEvents(ctx context.Context, runID string, afterSeq int64) ([]storage.Event, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT run_id, seq, invocation_id, payload_json, created_at
		FROM events WHERE run_id = ? AND seq > ? AND type = 'policy.decided' ORDER BY seq`, runID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.Event
	for rows.Next() {
		var event storage.Event
		var payload, created string
		if err := rows.Scan(&event.RunID, &event.Seq, &event.InvocationID, &payload, &created); err != nil {
			return nil, err
		}
		event.Type, event.Persistent, event.Payload = "policy.decided", true, json.RawMessage(payload)
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}
