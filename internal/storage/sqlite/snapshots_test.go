package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/migrations"
)

func TestWorkspaceSnapshotCRUD(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seq := int64(42)
	created := time.Date(2026, 11, 18, 12, 30, 0, 0, time.UTC)
	rec, err := db.CreateWorkspaceSnapshot(ctx, storage.WorkspaceSnapshot{
		WorkspaceID: "workspace_a", TreeOID: "abc123", Label: "before", EventSeq: &seq,
		FileCount: 3, TotalBytes: 17, CreatedAt: created,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.ID) < 5 || rec.ID[:5] != "snap_" {
		t.Fatalf("snapshot ID %q does not use snap_ prefix", rec.ID)
	}
	loaded, err := db.GetWorkspaceSnapshot(ctx, rec.ID)
	if err != nil || loaded.ID != rec.ID || loaded.WorkspaceID != rec.WorkspaceID || loaded.TreeOID != rec.TreeOID || loaded.Label != rec.Label || loaded.EventSeq == nil || *loaded.EventSeq != seq || loaded.FileCount != 3 || loaded.TotalBytes != 17 || !loaded.CreatedAt.Equal(created) {
		t.Fatalf("loaded snapshot %+v, error %v", loaded, err)
	}
	if _, err := db.CreateWorkspaceSnapshot(ctx, storage.WorkspaceSnapshot{WorkspaceID: "workspace_a", TreeOID: "def456", Label: "after", CreatedAt: created.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateWorkspaceSnapshot(ctx, storage.WorkspaceSnapshot{WorkspaceID: "workspace_b", TreeOID: "other", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	listed, err := db.ListWorkspaceSnapshots(ctx, "workspace_a")
	if err != nil || len(listed) != 2 || listed[0].ID != rec.ID || listed[1].Label != "after" {
		t.Fatalf("workspace list %+v, error %v", listed, err)
	}
	if err := db.DeleteWorkspaceSnapshotForWorkspace(ctx, "wrong_workspace", rec.ID); err == nil {
		t.Fatal("workspace-scoped delete succeeded for a different workspace")
	}
	byID, err := db.CreateWorkspaceSnapshot(ctx, storage.WorkspaceSnapshot{WorkspaceID: "workspace_a", TreeOID: "ghi789"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteWorkspaceSnapshot(ctx, byID.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteWorkspaceSnapshotForWorkspace(ctx, "workspace_a", rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetWorkspaceSnapshot(ctx, rec.ID); err == nil {
		t.Fatal("scoped-deleted snapshot still exists")
	}
	if _, err := db.GetWorkspaceSnapshot(ctx, byID.ID); err == nil {
		t.Fatal("ID-deleted snapshot still exists")
	}
	if err := db.DeleteWorkspaceSnapshot(ctx, rec.ID); err == nil {
		t.Fatal("deleting a missing snapshot by ID unexpectedly succeeded")
	}
	if err := db.DeleteWorkspaceSnapshotForWorkspace(ctx, "workspace_a", rec.ID); err == nil {
		t.Fatal("deleting a missing workspace snapshot unexpectedly succeeded")
	}
	if err := db.DeleteWorkspaceSnapshotsByWorkspace(ctx, "workspace_a"); err != nil {
		t.Fatal(err)
	}
	listed, err = db.ListWorkspaceSnapshots(ctx, "workspace_a")
	if err != nil || len(listed) != 0 {
		t.Fatalf("workspace records remain after delete: %+v, %v", listed, err)
	}
	listed, err = db.ListWorkspaceSnapshots(ctx, "workspace_b")
	if err != nil || len(listed) != 1 || listed[0].TreeOID != "other" {
		t.Fatalf("deleting one workspace affected another: %+v, %v", listed, err)
	}
}

func TestMigration004FromExisting001To003Database(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	files, err := migrations.Files()
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Version > 3 {
			continue
		}
		for _, stmt := range splitSQL(file.SQL) {
			if _, err := raw.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("apply %s: %v", file.Name, err)
			}
		}
		if _, err := raw.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, ?)`, file.Version, file.Name, formatTime(time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	db := &DB{sql: raw, now: time.Now}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	version, err := db.SchemaVersion(ctx)
	if err != nil || version != 8 {
		t.Fatalf("schema version %d, error %v", version, err)
	}
	if _, err := db.CreateWorkspaceSnapshot(ctx, storage.WorkspaceSnapshot{WorkspaceID: "old", TreeOID: "tree"}); err != nil {
		t.Fatalf("snapshot table unavailable after upgrade: %v", err)
	}
}
