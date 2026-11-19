package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/migrations"
)

func TestMigration007PreservesExistingRunsAndWorktreeBinding(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v6.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	files, err := migrations.Files()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Version > 6 {
			continue
		}
		for _, stmt := range splitSQL(file.SQL) {
			if _, err := raw.Exec(stmt); err != nil {
				t.Fatalf("apply %s: %v", file.Name, err)
			}
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations VALUES(?, ?, ?)`, file.Version, file.Name, formatTime(time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO sessions VALUES('ses', '2026-11-19', '2026-11-19')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO runs(id, session_id, status, budget_json, version, created_at, updated_at) VALUES('old', 'ses', 'RUNNING', '{}', 1, '2026-11-19', '2026-11-19')`); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	old, err := db.GetRun(ctx, "old")
	if err != nil || old.WorktreeBaseSHA != "" || old.WorktreeBranch != "" || old.WorktreePath != "" {
		t.Fatalf("old run changed: %+v, %v", old, err)
	}
	want := storage.Run{ID: "run_wt", SessionID: "ses", Status: domain.RunRunning, Version: 1,
		Budget: domain.DefaultBudget(), WorktreeBaseSHA: "0123456789abcdef", WorktreeBranch: "cloud-harness/run_wt",
		WorktreePath: "/original/.cloud-harness/worktrees/run_wt"}
	if err := db.CreateRun(ctx, want); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.GetRun(ctx, want.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Status = domain.RunWaiting
	loaded.WaitReason = domain.WaitUser
	if err := db.CompareAndSwapRun(ctx, loaded, loaded.Version); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if version, err := db.SchemaVersion(ctx); err != nil || version != 7 {
		t.Fatalf("version %d, err %v", version, err)
	}
	for _, session := range []string{"", "ses"} {
		runs, err := db.ListRuns(ctx, session)
		if err != nil || len(runs) != 2 {
			t.Fatalf("list %q: %+v, %v", session, runs, err)
		}
		for _, run := range runs {
			if run.ID == want.ID && (run.WorktreeBaseSHA != want.WorktreeBaseSHA || run.WorktreeBranch != want.WorktreeBranch || run.WorktreePath != want.WorktreePath || run.Status != domain.RunWaiting) {
				t.Fatalf("binding lost: %+v", run)
			}
		}
	}
	if runs, err := db.ListExpiredRunningRuns(ctx, time.Now()); err != nil || len(runs) != 1 || runs[0].ID != old.ID {
		t.Fatalf("expired runs: %+v, %v", runs, err)
	}
}
