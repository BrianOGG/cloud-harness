package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/migrations"
)

func TestSessionPreferencesDefaultsAndPersistence(t *testing.T) {
	ctx := t.Context()
	file := filepath.Join(t.TempDir(), "preferences.db")
	db, err := Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, storage.Session{ID: "one", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	p, err := db.GetSessionPreferences(ctx, "one")
	if err != nil || p.ModelID != "default" || p.ApprovalMode != "ask" {
		t.Fatalf("defaults: %+v %v", p, err)
	}
	for _, mode := range []string{"safe", "workspace-write", "ask", "full-auto"} {
		if err := db.SaveSessionPreferences(ctx, storage.SessionPreferences{SessionID: "one", ModelID: "custom:example", ApprovalMode: mode}); err != nil {
			t.Fatal(err)
		}
		if got, err := db.GetSessionPreferences(ctx, "one"); err != nil || got.ApprovalMode != mode {
			t.Fatalf("mode=%s got=%+v err=%v", mode, got, err)
		}
	}
	for _, mode := range []string{"unsafe", "unknown", "", "FULL-AUTO"} {
		if err := db.SaveSessionPreferences(ctx, storage.SessionPreferences{SessionID: "one", ApprovalMode: mode}); err == nil {
			t.Fatalf("invalid mode %q persisted", mode)
		}
	}
	if err := db.SaveSessionPreferences(ctx, storage.SessionPreferences{SessionID: "missing", ApprovalMode: "safe"}); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	if _, err := db.GetSessionPreferences(ctx, "missing"); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	p, err = db.GetSessionPreferences(ctx, "one")
	if err != nil || p.ModelID != "custom:example" || p.ApprovalMode != "full-auto" || p.UpdatedAt.IsZero() {
		t.Fatalf("reopened: %+v %v", p, err)
	}
}

func TestFullAutoPreferencesMigrationPreservesExisting011(t *testing.T) {
	ctx := t.Context()
	file := filepath.Join(t.TempDir(), "existing-011.db")
	raw, err := sql.Open("sqlite", "file:"+file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	legacy := &DB{sql: raw, now: time.Now}
	if err := legacy.configure(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	files, err := migrations.Files()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Version <= 11 {
			if err := legacy.apply(ctx, f.Version, f.Name, f.SQL); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := map[string]storage.SessionPreferences{}
	for _, mode := range []string{"ask", "safe", "workspace-write"} {
		if err := legacy.CreateSession(ctx, storage.Session{ID: mode, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		updated := time.Now().UTC().Truncate(time.Microsecond)
		if _, err := raw.ExecContext(ctx, `INSERT INTO session_preferences(session_id, approval_mode, model_id, updated_at) VALUES(?,?,?,?)`, mode, mode, "profile:"+mode, formatTime(updated)); err != nil {
			t.Fatal(err)
		}
		want[mode] = storage.SessionPreferences{SessionID: mode, ApprovalMode: mode, ModelID: "profile:" + mode, ReasoningEffort: "default", UpdatedAt: updated}
	}
	if _, err := raw.ExecContext(ctx, `UPDATE session_preferences SET approval_mode='full-auto' WHERE session_id='ask'`); err == nil {
		t.Fatal("011 fixture unexpectedly accepts full-auto")
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for mode, before := range want {
		got, err := db.GetSessionPreferences(ctx, mode)
		if err != nil || got != before {
			t.Fatalf("migration changed %s preference: got=%+v want=%+v err=%v", mode, got, before, err)
		}
	}
	if err := db.SaveSessionPreferences(ctx, storage.SessionPreferences{SessionID: "ask", ModelID: "default", ApprovalMode: "full-auto"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetSessionPreferences(ctx, "ask"); err != nil || got.ApprovalMode != "full-auto" {
		t.Fatalf("migration reapply changed full-auto: %+v err=%v", got, err)
	}
	if err := db.SaveSessionPreferences(ctx, storage.SessionPreferences{SessionID: "ask", ApprovalMode: "unknown"}); err == nil {
		t.Fatal("upgraded CHECK accepted unknown mode")
	}
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM sessions WHERE id = 'ask'`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_preferences WHERE session_id = 'ask'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("preference foreign-key cascade lost: count=%d err=%v", count, err)
	}
	if violations, err := db.ForeignKeyCheck(ctx); err != nil || len(violations) != 0 {
		t.Fatalf("migration foreign keys: %+v err=%v", violations, err)
	}
}

func TestReasoningMigrationPreservesExisting012AndReopen(t *testing.T) {
	ctx := t.Context()
	file := filepath.Join(t.TempDir(), "existing-012.db")
	raw, err := sql.Open("sqlite", "file:"+file)
	if err != nil {
		t.Fatal(err)
	}
	legacy := &DB{sql: raw, now: time.Now}
	if err := legacy.configure(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	files, err := migrations.Files()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Version <= 12 {
			if err := legacy.apply(ctx, f.Version, f.Name, f.SQL); err != nil {
				t.Fatal(err)
			}
		}
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := legacy.CreateSession(ctx, storage.Session{ID: "old", CreatedAt: at, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO session_preferences(session_id,approval_mode,model_id,updated_at) VALUES('old','full-auto','custom:old',?)`, formatTime(at)); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	p, err := db.GetSessionPreferences(ctx, "old")
	if err != nil || p.ApprovalMode != "full-auto" || p.ModelID != "custom:old" || p.ReasoningEffort != "default" || !p.UpdatedAt.Equal(at) {
		t.Fatalf("migration changed preference: %+v %v", p, err)
	}
	for _, effort := range []string{"off", "low", "medium", "high", "default"} {
		p.ReasoningEffort = effort
		if err := db.SaveSessionPreferences(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	p.ReasoningEffort = "invalid"
	if err := db.SaveSessionPreferences(ctx, p); err == nil {
		t.Fatal("constraint accepted invalid effort")
	}
	p.ReasoningEffort = "high"
	if err := db.SaveSessionPreferences(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	p, err = db.GetSessionPreferences(ctx, "old")
	if err != nil || p.ReasoningEffort != "high" || p.ApprovalMode != "full-auto" || p.ModelID != "custom:old" {
		t.Fatalf("reopen lost effort: %+v %v", p, err)
	}
}
