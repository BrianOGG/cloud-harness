package sqlite

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadOnlyOpenDoesNotCreateAndEscapesURIPath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	if _, err := ReadUsage(ctx, missing, time.Now()); err == nil {
		t.Fatal("ReadUsage accepted a missing database")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("read-only open created a missing file: %v", err)
	}

	path := filepath.Join(dir, "db?mode=rw&not-a-query.db")
	uri := url.URL{Scheme: "file", Path: path}
	writer, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`INSERT INTO schema_migrations(version) VALUES(6)`); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InspectReadOnly(ctx, path); err != nil {
		t.Fatalf("special path inspection: %v", err)
	}
	readDB, err := openReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readDB.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES(7)`); err == nil {
		t.Fatal("mode=ro connection allowed a write")
	}
	if err := readDB.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("read-only inspection changed database bytes")
	}
}
