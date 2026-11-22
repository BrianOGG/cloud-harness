package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/migrations"
)

func seedChatRewind(t *testing.T, db *DB, runID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_" + runID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, storage.Run{
		ID: runID, SessionID: "ses_" + runID, Status: domain.RunCompleted,
		WorkspaceID: "workspace", Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		if _, err := db.AppendEvent(ctx, storage.Event{RunID: runID, Type: "test", Persistent: true, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if i == 2 || i == 4 {
			if err := db.SaveCheckpoint(ctx, runID, int64(i), []byte(`{"at":`+strconv.Itoa(i)+`}`), now); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRewindChatBoundaryCheckpointAndRunIsolation(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedChatRewind(t, db, "one")
	seedChatRewind(t, db, "two")
	before, err := db.GetRun(ctx, "one")
	if err != nil || before.WorkspaceID != "workspace" {
		t.Fatalf("run binding: %+v, %v", before, err)
	}
	// Version and timestamp need not be derived from event seq. Use an older
	// timestamp for the later checkpoint to ensure selection uses the seq.
	if _, err := db.sql.ExecContext(ctx, `UPDATE checkpoints SET created_at = ? WHERE run_id = 'one' AND event_seq = 4`, formatTime(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	checkpoint, seq, err := db.LatestCheckpointAtOrBefore(ctx, "one", 3)
	if err != nil || seq != 2 || string(checkpoint) != `{"at":2}` {
		t.Fatalf("checkpoint %s at %d, err=%v", checkpoint, seq, err)
	}
	result, err := db.RewindChat(ctx, "one", 3)
	if err != nil || result.DeletedEvents != 1 || result.FirstSeq != 4 || result.LastSeq != 4 || result.CheckpointSeq != 2 {
		t.Fatalf("rewind result=%+v, err=%v", result, err)
	}
	checkpoint, err = db.LatestCheckpoint(ctx, "one")
	if err != nil || string(checkpoint) != `{"at":2}` {
		t.Fatalf("latest checkpoint %s, err=%v", checkpoint, err)
	}
	events, err := db.ListEvents(ctx, "one", 0)
	if err != nil || len(events) != 3 || events[2].Seq != 3 {
		t.Fatalf("events=%+v, err=%v", events, err)
	}
	if result, err = db.RewindChat(ctx, "one", 3); err != nil || result.DeletedEvents != 0 {
		t.Fatalf("idempotent rewind=%+v, err=%v", result, err)
	}
	after, err := db.GetRun(ctx, "one")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("run changed: %+v, err=%v", after, err)
	}
	events, err = db.ListEvents(ctx, "two", 0)
	if err != nil || len(events) != 4 {
		t.Fatalf("other run events=%+v, err=%v", events, err)
	}
	checkpoint, err = db.LatestCheckpoint(ctx, "two")
	if err != nil || string(checkpoint) != `{"at":4}` {
		t.Fatalf("other run checkpoint %s, err=%v", checkpoint, err)
	}
	// The next append can reuse a deleted seq, as prescribed by AppendEvent.
	next, err := db.AppendEvent(ctx, storage.Event{RunID: "one", Type: "new", Persistent: true, CreatedAt: time.Now()})
	if err != nil || next != 4 {
		t.Fatalf("append after rewind seq=%d, err=%v", next, err)
	}
}

func TestRewindChatTransactionRollsBack(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedChatRewind(t, db, "one")
	// Force the second mutation to fail after events were deleted.
	if _, err := db.sql.ExecContext(ctx, `CREATE TRIGGER fail_rewind BEFORE DELETE ON checkpoints
		BEGIN SELECT RAISE(ABORT, 'forced checkpoint failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RewindChat(ctx, "one", 2); err == nil || !strings.Contains(err.Error(), "forced checkpoint failure") {
		t.Fatalf("expected checkpoint failure: %v", err)
	}
	events, err := db.ListEvents(ctx, "one", 0)
	if err != nil || len(events) != 4 {
		t.Fatalf("partial event deletion: %+v, err=%v", events, err)
	}
	checkpoint, err := db.LatestCheckpoint(ctx, "one")
	if err != nil || string(checkpoint) != `{"at":4}` {
		t.Fatalf("partial checkpoint deletion: %s, err=%v", checkpoint, err)
	}
}

func TestRewindChatRejectsMissingCheckpointWithoutMutation(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	seedChatRewind(t, db, "one")
	for _, input := range []struct {
		run string
		seq int64
	}{{"missing", 2}, {"one", 1}, {"one", -1}} {
		if _, err := db.RewindChat(ctx, input.run, input.seq); err == nil {
			t.Fatalf("accepted %+v", input)
		}
	}
	events, err := db.ListEvents(ctx, "one", 0)
	if err != nil || len(events) != 4 {
		t.Fatalf("events=%+v, err=%v", events, err)
	}
}

func TestMigration005FromExisting004(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v4.db")
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
		if file.Version > 4 {
			continue
		}
		for _, stmt := range splitSQL(file.SQL) {
			if _, err := raw.ExecContext(ctx, stmt); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := raw.ExecContext(ctx, `INSERT INTO schema_migrations VALUES(?, ?, ?)`, file.Version, file.Name, formatTime(time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO sessions VALUES('ses', '2026-11-18', '2026-11-18')`); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"run_a", "run_b", "runXb"} {
		if _, err := raw.ExecContext(ctx, `INSERT INTO runs(id, session_id, status, budget_json, version, created_at, updated_at) VALUES(?, 'ses', 'COMPLETED', '{}', 1, '2026-11-18', '2026-11-18')`, run); err != nil {
			t.Fatal(err)
		}
	}
	old := &DB{sql: raw, now: time.Now}
	for _, rec := range []storage.WorkspaceSnapshot{
		{ID: "sa", WorkspaceID: "ws_a", Label: "run_a/batch-1/pre", TreeOID: "tree"},
		{ID: "sb", WorkspaceID: "ws_b", Label: "run_b/batch-1/pre", TreeOID: "tree"},
		{ID: "sx", WorkspaceID: "ws_x", Label: "runXb/batch-1/pre", TreeOID: "tree"},
	} {
		// Seed the v4 schema directly; the current API writes columns added in v6.
		if _, err := raw.ExecContext(ctx, `INSERT INTO workspace_snapshots
			(id, workspace_id, tree_oid, label, file_count, total_bytes, created_at)
			VALUES(?, ?, ?, ?, 0, 0, ?)`, rec.ID, rec.WorkspaceID, rec.TreeOID, rec.Label, formatTime(time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"run_a": "ws_a", "run_b": "ws_b", "runXb": "ws_x"} {
		run, err := old.GetRun(ctx, id)
		if err != nil || run.WorkspaceID != want {
			t.Fatalf("migration binding %s: %+v, err=%v", id, run, err)
		}
	}
	if err := old.SaveCheckpoint(ctx, "run_a", 1, []byte(`{"messages":[]}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, seq, err := old.LatestCheckpointAtOrBefore(ctx, "run_a", 0); err != nil || seq != 0 {
		t.Fatalf("zero-event checkpoint seq=%d, err=%v", seq, err)
	}
	if err := old.Migrate(ctx); err != nil {
		t.Fatalf("reapply migration: %v", err)
	}
}
