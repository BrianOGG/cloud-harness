package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

// ReadOnlyInfo is the database state needed by the doctor command. All fields
// are collected with SELECT/PRAGMA reads from a mode=ro connection.
type ReadOnlyInfo struct {
	SchemaVersion        int
	JournalMode          string
	ForeignKeyViolations []string
}

// UsageRecord is one already-persisted model response and its provider usage.
// The schema intentionally has no model column, so Provider is the only model
// attribution available to read-only consumers.
type UsageRecord struct {
	ID        string
	RunID     string
	Provider  string
	CreatedAt time.Time
	Usage     domain.Usage
}

func openReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	uri := url.URL{Scheme: "file", Path: abs}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Add("_pragma", "busy_timeout(1000)")
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

// InspectReadOnly opens path with SQLite's explicit read-only mode and does
// not run configure, migrations, checkpoints, or any other write operation.
func InspectReadOnly(ctx context.Context, path string) (ReadOnlyInfo, error) {
	db, err := openReadOnly(ctx, path)
	if err != nil {
		return ReadOnlyInfo{}, err
	}
	defer db.Close()
	var out ReadOnlyInfo
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&out.SchemaVersion); err != nil {
		return ReadOnlyInfo{}, fmt.Errorf("read schema version: %w", err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&out.JournalMode); err != nil {
		return ReadOnlyInfo{}, fmt.Errorf("read journal mode: %w", err)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return ReadOnlyInfo{}, fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return ReadOnlyInfo{}, fmt.Errorf("foreign_key_check row: %w", err)
		}
		out.ForeignKeyViolations = append(out.ForeignKeyViolations,
			fmt.Sprintf("table=%s rowid=%v parent=%s fkid=%d", table, rowid, parent, fkid))
	}
	if err := rows.Err(); err != nil {
		return ReadOnlyInfo{}, fmt.Errorf("foreign_key_check rows: %w", err)
	}
	return out, nil
}

// ReadUsage returns persisted model-response usage rows at or after since.
// It never opens the normal migrating store and performs no writes.
func ReadUsage(ctx context.Context, path string, since time.Time) ([]UsageRecord, error) {
	db, err := openReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `
		SELECT id, run_id, provider, usage_json, created_at
		FROM model_responses WHERE created_at >= ? ORDER BY created_at ASC, id ASC`, formatTime(since))
	if err != nil {
		return nil, fmt.Errorf("read model usage: %w", err)
	}
	defer rows.Close()
	var out []UsageRecord
	for rows.Next() {
		var rec UsageRecord
		var raw, created string
		if err := rows.Scan(&rec.ID, &rec.RunID, &rec.Provider, &raw, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &rec.Usage); err != nil {
			return nil, fmt.Errorf("decode usage for %s: %w", rec.ID, err)
		}
		rec.CreatedAt, err = parseTime(created)
		if err != nil {
			return nil, fmt.Errorf("decode created_at for %s: %w", rec.ID, err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
