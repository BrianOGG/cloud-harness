// Package sqlite persists the harness in SQLite through modernc.org/sqlite.
//
// The driver is pure Go (no cgo). FTS5 is compiled in. A cgo driver such as
// mattn/go-sqlite3 is faster and tracks the system SQLite build, but it needs
// a C toolchain and makes cross-compilation the caller's problem. The pure-Go
// build is the M0 choice so `go test` and cross-builds do not depend on libsqlite.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/migrations"
	_ "modernc.org/sqlite"
)

// DB implements the persisted source of truth.
var _ storage.Store = (*DB)(nil)

// DB is the SQLite store. One connection keeps writers serialized.
type DB struct {
	sql *sql.DB
	now func() time.Time
}

// Open creates the file, enables WAL and foreign keys, and migrates.
func Open(ctx context.Context, path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	db := &DB{sql: sqlDB, now: time.Now}
	if err := db.configure(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if err := db.Migrate(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) Close() error { return db.sql.Close() }

func (db *DB) configure(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return err
	}
	if _, err := db.sql.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		return err
	}
	return nil
}

// Migrate applies embedded scripts that are not yet in schema_migrations.
// Each script runs in one transaction and can be reopened safely.
func (db *DB) Migrate(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return err
	}
	files, err := migrations.Files()
	if err != nil {
		return err
	}
	for _, f := range files {
		var n int
		if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, f.Version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if err := db.apply(ctx, f.Version, f.Name, f.SQL); err != nil {
			return fmt.Errorf("migration %s: %w", f.Name, err)
		}
	}
	return nil
}

func (db *DB) apply(ctx context.Context, version int, name, script string) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range splitSQL(script) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%w (sql: %.80q)", err, stmt)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, ?)`, version, name, formatTime(db.now())); err != nil {
		return err
	}
	return tx.Commit()
}

// SchemaVersion is the highest applied migration.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := db.sql.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

// JournalMode returns the SQLite journal mode.
func (db *DB) JournalMode(ctx context.Context) (string, error) {
	var mode string
	err := db.sql.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode)
	return mode, err
}

// ForeignKeys reports whether foreign_keys is on.
func (db *DB) ForeignKeys(ctx context.Context) (bool, error) {
	var v int
	err := db.sql.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&v)
	return v == 1, err
}

// ForeignKeyCheck runs PRAGMA foreign_key_check and returns any violations found.
func (db *DB) ForeignKeyCheck(ctx context.Context) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var violations []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return nil, err
		}
		violations = append(violations, fmt.Sprintf("table=%s rowid=%v parent=%s fkid=%d", table, rowid, parent, fkid))
	}
	return violations, rows.Err()
}

func splitSQL(script string) []string {
	var stmts []string
	var b strings.Builder
	for _, line := range strings.Split(script, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "--") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
		if strings.HasSuffix(trim, ";") {
			stmt := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(b.String()), ";"))
			if stmt != "" {
				stmts = append(stmts, stmt)
			}
			b.Reset()
		}
	}
	if rest := strings.TrimSpace(b.String()); rest != "" {
		stmts = append(stmts, strings.TrimSuffix(rest, ";"))
	}
	return stmts
}

// formatTime uses a fixed-width UTC timestamp so SQLite text comparisons
// match chronological order. RFC3339Nano drops trailing zeros and does not sort.
func formatTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

func mapErr(err error, id string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return domain.E(domain.CodeNotFound, "record not found").WithRef(id)
	}
	return err
}
