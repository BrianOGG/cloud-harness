package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/migrations"
)

func TestMessagesYearOneCursorPreservesBoundary(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	if err := db.CreateSession(ctx, storage.Session{ID: "year_one"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, created string }{
		{"a", "0001-01-01T00:00:00.000000000Z"},
		{"b", "0001-01-01T00:00:00.000000000Z"},
		{"c", "0001-01-01T00:00:00.000000001Z"},
	} {
		if _, err := db.sql.ExecContext(ctx, `INSERT INTO messages(id, session_id, role, source, kind, status, content, created_at) VALUES(?, 'year_one', 'user', 'turn', 'input', 'recorded', '', ?)`, row.id, row.created); err != nil {
			t.Fatal(err)
		}
	}
	page, err := db.ListMessages(ctx, "year_one", &storage.MessageCursor{ID: "a"}, 1)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != "b" || page.NextCursor == nil || !page.NextCursor.CreatedAt.IsZero() {
		t.Fatalf("year-one page=%+v err=%v", page, err)
	}
	page, err = db.ListMessages(ctx, "year_one", page.NextCursor, 1)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != "c" || page.NextCursor != nil {
		t.Fatalf("next page=%+v err=%v", page, err)
	}
	if _, err := db.ListMessages(ctx, "year_one", &storage.MessageCursor{}, 1); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("empty cursor ID error=%v", err)
	}
}

func TestMessageAnswerSourceSurvivesInternalWorkAndReopen(t *testing.T) {
	for _, tc := range []struct {
		name        string
		actions     []domain.Action
		pruneSource bool
		finishTool  bool
		wantSource  string
	}{
		{"answer_then_internal", []domain.Action{domain.ActionAnswer, domain.ActionPlan, domain.ActionCompact, "ORACLE"}, false, false, "rsp_0"},
		{"equal_execute_is_not_answer", []domain.Action{domain.ActionExecute, domain.ActionPlan}, false, false, ""},
		{"later_action_supersedes_answer", []domain.Action{domain.ActionAnswer, domain.ActionAskUser, domain.ActionPlan}, false, false, ""},
		{"latest_answer_identity", []domain.Action{domain.ActionAnswer, domain.ActionAnswer, domain.ActionPlan}, false, false, "rsp_1"},
		{"pruned_projection_and_source", []domain.Action{domain.ActionAnswer, domain.ActionPlan}, true, false, "rsp_0"},
		{"finish_turn_has_distinct_answer_source", []domain.Action{domain.ActionAnswer}, false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "sources.db")
			db, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			at := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
			mustSource(t, db.CreateSession(ctx, storage.Session{ID: "ses", CreatedAt: at, UpdatedAt: at}))
			run := storage.Run{ID: "run", SessionID: "ses", Status: domain.RunRunning, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: at, UpdatedAt: at}
			mustSource(t, db.CreateRun(ctx, run))
			for i, action := range tc.actions {
				items := []domain.VisibleItem{{Ordinal: 0, Kind: domain.ItemText, Text: "same text"}}
				if tc.finishTool {
					items = append(items, domain.VisibleItem{Ordinal: 1, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{Name: domain.ToolFinishTurn, Arguments: json.RawMessage(`{"final_answer":"same text"}`)}})
				}
				mustSource(t, db.SaveModelResponse(ctx, storage.StoredResponse{ID: "rsp_" + string(rune('0'+i)), RunID: run.ID, Complete: true, Action: action, Items: items, CreatedAt: at}))
			}
			// Incomplete/internal responses cannot change a committed source.
			mustSource(t, db.SaveModelResponse(ctx, storage.StoredResponse{ID: "partial", RunID: run.ID, Complete: false, Action: domain.ActionAnswer, Items: []domain.VisibleItem{{Kind: domain.ItemText, Text: "partial"}}, CreatedAt: at}))
			if tc.pruneSource {
				for _, query := range []string{
					"DELETE FROM messages WHERE source = 'model_response'",
					"DELETE FROM model_response_items",
					"DELETE FROM model_responses",
					"DELETE FROM steps",
				} {
					_, err := db.sql.ExecContext(ctx, query)
					mustSource(t, err)
				}
			}
			mustSource(t, db.Close())
			db, err = Open(ctx, path)
			mustSource(t, err)
			var source string
			mustSource(t, db.sql.QueryRowContext(ctx, "SELECT final_answer_response_id FROM runs WHERE id = 'run'").Scan(&source))
			if source != tc.wantSource {
				t.Fatalf("source=%q, want %q", source, tc.wantSource)
			}
			run.Status, run.FinalAnswer, run.UpdatedAt = domain.RunCompleted, "same text", at.Add(time.Second)
			mustSource(t, db.CommitRunTransition(ctx, run, 1, storage.Event{Type: "run.CoMpLeTeD", Persistent: true, CreatedAt: run.UpdatedAt}))
			page, err := db.ListMessages(ctx, "ses", nil, 200)
			mustSource(t, err)
			fallbacks := 0
			for _, msg := range page.Messages {
				if msg.ID == "msg_answer_run" {
					fallbacks++
					if msg.Event != "" || msg.Content != run.FinalAnswer {
						t.Fatalf("fallback=%+v", msg)
					}
				}
				if msg.Kind == "run_status" && msg.Event != "run.CoMpLeTeD" {
					t.Fatalf("event spelling lost: %+v", msg)
				}
			}
			wantFallbacks := 0
			if tc.wantSource == "" {
				wantFallbacks = 1
			}
			if fallbacks != wantFallbacks {
				t.Fatalf("fallbacks=%d want=%d page=%+v", fallbacks, wantFallbacks, page)
			}
		})
	}
}

func mustSource(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Copy only authoritative facts, with their exact IDs and times, into a real
// schema-008 database. Applying the unchanged 009 reproduces published defects.
func copyMessageSources(t *testing.T, from, to *DB) {
	t.Helper()
	for _, table := range []string{"sessions", "turns", "runs", "steps", "model_responses", "model_response_items", "tool_batches", "tool_invocations", "approvals", "events"} {
		rows, err := from.sql.Query("SELECT * FROM " + table)
		mustSource(t, err)
		columns, err := rows.Columns()
		mustSource(t, err)
		for rows.Next() {
			values := make([]any, len(columns))
			ptrs := make([]any, len(columns))
			for i := range values {
				ptrs[i] = &values[i]
			}
			mustSource(t, rows.Scan(ptrs...))
			var names []string
			var args []any
			for i, column := range columns {
				if column != "final_answer_response_id" {
					names = append(names, column)
					args = append(args, values[i])
				}
			}
			query := "INSERT INTO " + table + "(" + strings.Join(names, ",") + ") VALUES(" + strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + ")"
			_, err := to.sql.Exec(query, args...)
			mustSource(t, err)
		}
		mustSource(t, rows.Err())
		mustSource(t, rows.Close())
	}
}

func TestMigration010UpgradesApplied009LikeFreshStoreWithoutResurrection(t *testing.T) {
	ctx := context.Background()
	fresh := openTest(t)
	at := time.Date(2026, 11, 25, 9, 0, 0, 123, time.UTC)
	mustSource(t, fresh.CreateSession(ctx, storage.Session{ID: "ses", CreatedAt: at, UpdatedAt: at}))
	for _, id := range []string{"linked", "fallback", "command", "deleted", "pruned"} {
		created := at
		if id == "pruned" {
			created = at.Add(-48 * time.Hour)
		}
		run := storage.Run{ID: id, SessionID: "ses", Status: domain.RunRunning, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: created, UpdatedAt: created}
		mustSource(t, fresh.CreateRun(ctx, run))
		if id == "linked" {
			mustSource(t, fresh.SaveModelResponse(ctx, storage.StoredResponse{ID: "visible", RunID: id, Complete: true, Action: domain.ActionAnswer, CreatedAt: at,
				Items: []domain.VisibleItem{{Ordinal: 1, Kind: domain.ItemText, Text: "text"}, {Ordinal: 0, Kind: domain.ItemText, Text: "same "}}}))
			mustSource(t, fresh.SaveModelResponse(ctx, storage.StoredResponse{ID: "plan", RunID: id, Complete: true, Action: domain.ActionPlan, CreatedAt: at,
				Items: []domain.VisibleItem{{Kind: domain.ItemText, Text: "internal"}}}))
		}
		if id == "fallback" {
			mustSource(t, fresh.SaveModelResponse(ctx, storage.StoredResponse{ID: "earlier_execute", RunID: id, Complete: true, Action: domain.ActionExecute, CreatedAt: at,
				Items: []domain.VisibleItem{{Kind: domain.ItemText, Text: "same text"}}}))
			mustSource(t, fresh.SaveModelResponse(ctx, storage.StoredResponse{ID: "finish", RunID: id, Complete: true, Action: domain.ActionAnswer, CreatedAt: at,
				Items: []domain.VisibleItem{{Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{Name: domain.ToolFinishTurn, Arguments: json.RawMessage(`{"final_answer":"same text"}`)}}}}))
		}
		if id == "command" {
			mustSource(t, fresh.SaveModelResponse(ctx, storage.StoredResponse{ID: "command_response", RunID: id, Complete: true, Action: domain.ActionExecute, CreatedAt: at}))
			mustSource(t, fresh.SaveBatch(ctx, storage.StoredBatch{ID: "batch", ModelResponseID: "command_response", RunID: id, Action: domain.ActionExecute, Status: domain.BatchWaitingApproval,
				Invocations: []domain.Invocation{{ID: "inv", ProviderCallID: "call", Name: "run_command", Status: domain.InvPending, Class: domain.ActionExecute,
					Arguments: json.RawMessage(`{"command":"printf 'review me'","cwd":"/private/work","extra":"not a summary"}`)}}}))
			mustSource(t, fresh.SaveApproval(ctx, domain.Approval{ID: "approval", RunID: id, InvocationID: "inv", Tool: "run_command", State: "pending", CreatedAt: at}))
			continue
		}
		run.Status, run.FinalAnswer, run.UpdatedAt = domain.RunCompleted, "same text", created.Add(time.Second)
		mustSource(t, fresh.CommitRunTransition(ctx, run, 1, storage.Event{Type: "run.CoMpLeTeD", CreatedAt: run.UpdatedAt, Persistent: true}))
	}
	path := filepath.Join(t.TempDir(), "applied009.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	mustSource(t, err)
	legacy := &DB{sql: raw, now: func() time.Time { return at }}
	t.Cleanup(func() { _ = legacy.Close() })
	mustSource(t, legacy.configure(ctx))
	_, err = raw.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`)
	mustSource(t, err)
	files, err := migrations.Files()
	mustSource(t, err)
	for _, file := range files {
		if file.Version < 9 {
			mustSource(t, legacy.apply(ctx, file.Version, file.Name, file.SQL))
		}
	}
	copyMessageSources(t, fresh, legacy)
	for _, file := range files {
		if file.Version == 9 {
			mustSource(t, legacy.apply(ctx, file.Version, file.Name, file.SQL))
		}
	}
	var count int
	mustSource(t, raw.QueryRow("SELECT COUNT(*) FROM messages WHERE id = 'msg_answer_linked'").Scan(&count))
	if count != 1 {
		t.Fatal("unchanged 009 did not reproduce duplicate fallback")
	}
	var oldSummary string
	mustSource(t, raw.QueryRow("SELECT content FROM messages WHERE id = 'msg_approval_approval_requested'").Scan(&oldSummary))
	if oldSummary != "Approval requested for run_command" {
		t.Fatalf("old summary=%q", oldSummary)
	}
	// Also reproduce the event value written by the old live projection.
	_, err = raw.Exec("UPDATE messages SET event = 'run.CoMpLeTeD' WHERE id = 'msg_answer_fallback'")
	mustSource(t, err)
	for _, db := range []*DB{fresh, legacy} {
		pruned, err := db.PruneRuns(ctx, at.Add(-24*time.Hour))
		mustSource(t, err)
		if pruned.RunsDeleted != 1 {
			t.Fatalf("pruned=%+v", pruned)
		}
		_, err = db.sql.Exec("DELETE FROM messages WHERE id = 'msg_answer_deleted'")
		mustSource(t, err)
	}
	want, err := fresh.ListMessages(ctx, "ses", nil, 200)
	mustSource(t, err)
	before, err := legacy.ListMessages(ctx, "ses", nil, 200)
	mustSource(t, err)
	version, err := legacy.SchemaVersion(ctx)
	mustSource(t, err)
	if version != 9 {
		t.Fatalf("before upgrade version=%d", version)
	}
	t.Logf("applied schema=009; retained rows=%d; fresh schema=010 rows=%d", len(before.Messages), len(want.Messages))
	mustSource(t, legacy.Close())
	legacy, err = Open(ctx, path)
	mustSource(t, err)
	for reopen := 0; reopen < 2; reopen++ {
		mustSource(t, legacy.Migrate(ctx))
		// Query-only mode proves the read path cannot repair or recreate rows.
		_, err := legacy.sql.Exec("PRAGMA query_only=ON")
		mustSource(t, err)
		got, err := legacy.ListMessages(ctx, "ses", nil, 200)
		mustSource(t, err)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("upgrade/reopen=%d got=%+v want fresh=%+v", reopen, got, want)
		}
		for _, old := range before.Messages {
			for _, msg := range got.Messages {
				if old.ID == msg.ID && !old.CreatedAt.Equal(msg.CreatedAt) {
					t.Fatalf("timestamp reset: before=%+v after=%+v", old, msg)
				}
			}
		}
		for id, source := range map[string]string{"linked": "visible", "fallback": "", "command": "", "deleted": ""} {
			var gotSource string
			mustSource(t, legacy.sql.QueryRow("SELECT final_answer_response_id FROM runs WHERE id = ?", id).Scan(&gotSource))
			if gotSource != source {
				t.Fatalf("run=%s source=%q want=%q", id, gotSource, source)
			}
		}
		t.Logf("upgrade/reopen=%d: %d rows equal to fresh; IDs/times stable; deleted/pruned facts absent; query-only GET succeeds", reopen, len(got.Messages))
		mustSource(t, legacy.Close())
		legacy, err = Open(ctx, path)
		mustSource(t, err)
	}
}
