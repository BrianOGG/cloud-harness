package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestMessagesPersistAndPaginateAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "messages.db")
	now := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	db.now = func() time.Time { return now }
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_history", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_other", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTurn(ctx, storage.Turn{ID: "turn_1", SessionID: "ses_history", UserInput: "first question", Status: "completed", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	run := storage.Run{ID: "run_1", SessionID: "ses_history", TurnID: "turn_1", Status: domain.RunRunning, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveModelResponse(ctx, storage.StoredResponse{
		ID: "response_1", RunID: run.ID, Provider: "mock", Complete: true, Action: domain.ActionAnswer,
		Items: []domain.VisibleItem{{Ordinal: 0, Kind: domain.ItemText, Text: "first reply"}}, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	question := storage.StoredBatch{
		ID: "batch_1", ModelResponseID: "response_1", RunID: run.ID, Action: domain.ActionAskUser,
		Status: domain.BatchWaitingUser, Invocations: []domain.Invocation{{
			ID: "inv_question", Ordinal: 0, ProviderCallID: "call_question", Name: domain.ToolAskUser,
			Arguments: json.RawMessage(`{"question":"Which region?"}`), Class: domain.ActionAskUser,
			Status: domain.InvPending,
		}},
	}
	if err := db.SaveBatch(ctx, question); err != nil {
		t.Fatal(err)
	}
	question.Invocations[0].Status = domain.InvSucceeded
	question.Invocations[0].Result = &domain.CallResult{OK: true, Data: json.RawMessage(`{"answer":"eu-west"}`)}
	question.Status = domain.BatchComplete
	if err := db.UpdateBatch(ctx, question); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateBatch(ctx, question); err != nil {
		t.Fatal(err)
	}
	approval := domain.Approval{ID: "approval_1", RunID: run.ID, InvocationID: "inv_write", Tool: "write_file", State: "pending", CreatedAt: now}
	if err := db.SaveApproval(ctx, approval); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateApprovalState(ctx, run.ID, approval.InvocationID, "approved"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateApprovalState(ctx, run.ID, approval.InvocationID, "approved"); err != nil {
		t.Fatal(err)
	}
	next := run
	next.Status = domain.RunFailed
	next.Version = 2
	next.Error = domain.E(domain.CodeProviderHTTP, "provider stopped")
	next.UpdatedAt = now
	if err := db.CommitRunTransition(ctx, next, 1, storage.Event{Type: "run.FAILED", CreatedAt: now, Persistent: true}); err != nil {
		t.Fatal(err)
	}

	var all []storage.Message
	var cursor *storage.MessageCursor
	seen := map[string]bool{}
	for {
		page, err := db.ListMessages(ctx, "ses_history", cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range page.Messages {
			if seen[message.ID] {
				t.Fatalf("duplicate message %q", message.ID)
			}
			seen[message.ID] = true
			all = append(all, message)
			if message.SessionID != "ses_history" || message.RunID != "run_1" && message.RunID != "" {
				t.Fatalf("message association %+v", message)
			}
		}
		if page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
	}
	if len(all) != 7 {
		t.Fatalf("message count %d: %+v", len(all), all)
	}
	wantKinds := map[string]bool{"input": true, "response": true, "question": true, "answer": true, "approval_requested": true, "approval_decision": true, "run_status": true}
	for _, message := range all {
		if !wantKinds[message.Kind] || message.ID == "" || message.Role == "" || message.CreatedAt.IsZero() {
			t.Fatalf("invalid message %+v", message)
		}
		if message.Status != "recorded" && message.Status != "failed" && message.Status != "cancelled" {
			t.Fatalf("invalid status %+v", message)
		}
	}
	byKind := map[string]storage.Message{}
	for _, message := range all {
		byKind[message.Kind] = message
	}
	if byKind["input"].RunID != run.ID || byKind["input"].Status != "recorded" {
		t.Fatalf("input projection=%+v", byKind["input"])
	}
	if byKind["response"].Content != "first reply" || byKind["response"].Status != "recorded" {
		t.Fatalf("response projection=%+v", byKind["response"])
	}
	if byKind["question"].Status != "recorded" || byKind["answer"].Status != "recorded" {
		t.Fatalf("ask_user projections question=%+v answer=%+v", byKind["question"], byKind["answer"])
	}
	if byKind["approval_requested"].Content != "Approval requested for write_file" || byKind["approval_decision"].Content != "approve" {
		t.Fatalf("approval projections requested=%+v decision=%+v", byKind["approval_requested"], byKind["approval_decision"])
	}
	if byKind["run_status"].Status != "failed" || byKind["run_status"].Content != "provider stopped" {
		t.Fatalf("terminal projection=%+v", byKind["run_status"])
	}
	if page, err := db.ListMessages(ctx, "ses_other", nil, 50); err != nil || len(page.Messages) != 0 {
		t.Fatalf("session isolation page=%+v err=%v", page, err)
	}
	if _, err := db.ListMessages(ctx, "ses_history", &storage.MessageCursor{ID: "not-a-cursor"}, 2); err == nil || !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("invalid cursor err=%v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	page, err := db.ListMessages(ctx, "ses_history", nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != len(all) {
		t.Fatalf("reopened count %d, want %d", len(page.Messages), len(all))
	}
	for i := range all {
		if page.Messages[i].ID != all[i].ID || page.Messages[i].Content != all[i].Content {
			t.Fatalf("reopened[%d]=%+v, want %+v", i, page.Messages[i], all[i])
		}
	}
}

func TestMessageProjectionUsesOrdinalAndSourceIdentity(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	db := openTest(t)
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_projection", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	run := storage.Run{ID: "run_projection", SessionID: "ses_projection", Status: domain.RunRunning, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveModelResponse(ctx, storage.StoredResponse{
		ID: "response_ordered", RunID: run.ID, Provider: "mock", Complete: true, Action: domain.ActionAnswer, CreatedAt: now,
		Items: []domain.VisibleItem{
			{Ordinal: 2, Kind: domain.ItemText, Text: "C"},
			{Ordinal: 0, Kind: domain.ItemText, Text: "A"},
			{Ordinal: 1, Kind: domain.ItemText, Text: "B"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveModelResponse(ctx, storage.StoredResponse{
		ID: "response_partial", RunID: run.ID, Provider: "mock", Complete: false, Action: domain.ActionAnswer, CreatedAt: now,
		Items: []domain.VisibleItem{{Ordinal: 0, Kind: domain.ItemText, Text: "not committed"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveModelResponse(ctx, storage.StoredResponse{
		ID: "response_plan", RunID: run.ID, Provider: "mock", Complete: true, Action: domain.ActionPlan, CreatedAt: now,
		Items: []domain.VisibleItem{{Ordinal: 0, Kind: domain.ItemText, Text: "internal plan"}},
	}); err != nil {
		t.Fatal(err)
	}
	page, err := db.ListMessages(ctx, run.SessionID, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].ID != "msg_response_response_ordered" || page.Messages[0].Content != "ABC" {
		t.Fatalf("messages=%+v", page.Messages)
	}

	next := run
	next.Status = domain.RunCompleted
	next.Version = 2
	next.FinalAnswer = "ABC"
	next.UpdatedAt = now.Add(time.Second)
	if err := db.CommitRunTransition(ctx, next, 1, storage.Event{Type: "run.completed", CreatedAt: next.UpdatedAt, Persistent: true}); err != nil {
		t.Fatal(err)
	}
	page, err = db.ListMessages(ctx, run.SessionID, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	responseCount, fallbackCount, statusCount := 0, 0, 0
	for _, message := range page.Messages {
		if message.Kind == "response" {
			responseCount++
			if message.ID == "msg_answer_"+run.ID {
				fallbackCount++
			}
		}
		if message.Kind == "run_status" {
			statusCount++
			if message.Status != "recorded" || message.Content != string(domain.RunCompleted) {
				t.Fatalf("completed status=%+v", message)
			}
		}
	}
	if responseCount != 2 || fallbackCount != 1 || statusCount != 1 {
		t.Fatalf("source projection counts responses=%d fallback=%d status=%d messages=%+v", responseCount, fallbackCount, statusCount, page.Messages)
	}

	fallbackRun := storage.Run{ID: "run_fallback", SessionID: run.SessionID, Status: domain.RunRunning, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateRun(ctx, fallbackRun); err != nil {
		t.Fatal(err)
	}
	fallback := fallbackRun
	fallback.Status = domain.RunCompleted
	fallback.Version = 2
	fallback.FinalAnswer = "ABC"
	fallback.UpdatedAt = now.Add(2 * time.Second)
	if err := db.CommitRunTransition(ctx, fallback, 1, storage.Event{Type: "run.completed", CreatedAt: fallback.UpdatedAt, Persistent: true}); err != nil {
		t.Fatal(err)
	}
	page, err = db.ListMessages(ctx, run.SessionID, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	fallbackCount = 0
	for _, message := range page.Messages {
		if message.ID == "msg_answer_"+fallbackRun.ID {
			fallbackCount++
		}
	}
	if fallbackCount != 1 {
		t.Fatalf("fallback count=%d messages=%+v", fallbackCount, page.Messages)
	}
}

func TestTurnInputIsBoundOnlyToFirstRootRun(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	db := openTest(t)
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_retry", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTurn(ctx, storage.Turn{ID: "turn_retry", SessionID: "ses_retry", UserInput: "same input", Status: "completed", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	root := storage.Run{ID: "run_root", SessionID: "ses_retry", TurnID: "turn_retry", Status: domain.RunRunning, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now}
	retry := root
	retry.ID = "run_retry"
	retry.ParentRunID = root.ID
	if err := db.CreateRun(ctx, root); err != nil {
		t.Fatal(err)
	}
	secondRoot := root
	secondRoot.ID = "run_second_root"
	secondRoot.CreatedAt = now.Add(time.Second)
	secondRoot.UpdatedAt = secondRoot.CreatedAt
	if err := db.CreateRun(ctx, secondRoot); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, retry); err != nil {
		t.Fatal(err)
	}
	page, err := db.ListMessages(ctx, "ses_retry", nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].ID != "msg_user_turn_retry" || page.Messages[0].RunID != root.ID {
		t.Fatalf("input=%+v", page.Messages)
	}
}

func TestMessagePaginationHasStableLongBoundaries(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 8, 0, 0, 123456789, time.UTC)
	db := openTest(t)
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_long", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 237; i++ {
		message := storage.Message{
			ID: fmt.Sprintf("msg_%03d", i), SessionID: "ses_long", Role: "user", Source: "test",
			Kind: "input", Status: "recorded", Content: fmt.Sprintf("item %03d", i), CreatedAt: now,
		}
		if err := insertMessage(ctx, tx, message); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var cursor *storage.MessageCursor
	got := make([]string, 0, 237)
	for len(got) < 237 {
		page, err := db.ListMessages(ctx, "ses_long", cursor, 17)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Messages) == 0 {
			t.Fatalf("premature empty page after %d records", len(got))
		}
		for _, message := range page.Messages {
			got = append(got, message.ID)
		}
		if page.NextCursor == nil {
			break
		}
		cursor = page.NextCursor
	}
	if len(got) != 237 {
		t.Fatalf("pagination returned %d records", len(got))
	}
	for i, id := range got {
		if want := fmt.Sprintf("msg_%03d", i); id != want {
			t.Fatalf("pagination[%d]=%q, want %q", i, id, want)
		}
	}
	first, err := db.ListMessages(ctx, "ses_long", nil, 17)
	if err != nil || len(first.Messages) != 17 || first.NextCursor == nil {
		t.Fatalf("first page=%+v err=%v", first, err)
	}
	if _, err := db.sql.ExecContext(ctx, "DELETE FROM messages WHERE id = ?", first.Messages[16].ID); err != nil {
		t.Fatal(err)
	}
	continued, err := db.ListMessages(ctx, "ses_long", first.NextCursor, 17)
	if err != nil {
		t.Fatal(err)
	}
	if len(continued.Messages) != 17 || continued.Messages[0].ID != "msg_017" {
		t.Fatalf("continuation after deleted anchor=%+v", continued)
	}
}

func TestMigration009BackfillsTranscriptStatusesAndOrder(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	db := openTest(t)
	db.now = func() time.Time { return now }
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_backfill", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTurn(ctx, storage.Turn{ID: "turn_backfill", SessionID: "ses_backfill", UserInput: "original input", Status: "completed", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	run := storage.Run{ID: "run_backfill", SessionID: "ses_backfill", TurnID: "turn_backfill", Status: domain.RunRunning, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveModelResponse(ctx, storage.StoredResponse{
		ID: "response_backfill", RunID: run.ID, Provider: "mock", Complete: true, Action: domain.ActionAskUser, CreatedAt: now,
		Items: []domain.VisibleItem{{Ordinal: 1, Kind: domain.ItemText, Text: "B"}, {Ordinal: 0, Kind: domain.ItemText, Text: "A"}},
	}); err != nil {
		t.Fatal(err)
	}
	batch := storage.StoredBatch{
		ID: "batch_backfill", ModelResponseID: "response_backfill", RunID: run.ID, Action: domain.ActionAskUser,
		Status: domain.BatchWaitingUser, Invocations: []domain.Invocation{{
			ID: "inv_backfill", Ordinal: 0, ProviderCallID: "call_backfill", Name: domain.ToolAskUser,
			Arguments: json.RawMessage("{\"question\":\"Select region\"}"), Class: domain.ActionAskUser, Status: domain.InvPending,
		}},
	}
	if err := db.SaveBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	batch.Status = domain.BatchComplete
	batch.Invocations[0].Status = domain.InvSucceeded
	batch.Invocations[0].Result = &domain.CallResult{OK: true, Data: json.RawMessage("{\"answer\":\"eu-west\"}")}
	if err := db.UpdateBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveApproval(ctx, domain.Approval{ID: "approval_backfill", RunID: run.ID, InvocationID: "inv_write", Tool: "write_file", State: "pending", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateApprovalState(ctx, run.ID, "inv_write", "denied"); err != nil {
		t.Fatal(err)
	}
	next := run
	next.Status = domain.RunFailed
	next.Version = 2
	next.Error = domain.E(domain.CodeProviderHTTP, "provider unavailable")
	next.UpdatedAt = now.Add(time.Second)
	if err := db.CommitRunTransition(ctx, next, 1, storage.Event{Type: "run.FAILED", CreatedAt: next.UpdatedAt, Persistent: true}); err != nil {
		t.Fatal(err)
	}
	fallbackRun := storage.Run{ID: "run_backfill_fallback", SessionID: "ses_backfill", Status: domain.RunRunning, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateRun(ctx, fallbackRun); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveModelResponse(ctx, storage.StoredResponse{
		ID: "response_fallback_visible", RunID: fallbackRun.ID, Provider: "mock", Complete: true, Action: domain.ActionAnswer, CreatedAt: now,
		Items: []domain.VisibleItem{{Ordinal: 0, Kind: domain.ItemText, Text: "same final text"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveModelResponse(ctx, storage.StoredResponse{
		ID: "response_fallback_plan", RunID: fallbackRun.ID, Provider: "mock", Complete: true, Action: domain.ActionPlan, CreatedAt: now.Add(time.Millisecond),
		Items: []domain.VisibleItem{{Ordinal: 0, Kind: domain.ItemText, Text: "internal plan"}},
	}); err != nil {
		t.Fatal(err)
	}
	fallback := fallbackRun
	fallback.Status = domain.RunCompleted
	fallback.Version = 2
	fallback.FinalAnswer = "same final text"
	fallback.UpdatedAt = now.Add(time.Second)
	if err := db.CommitRunTransition(ctx, fallback, 1, storage.Event{Type: "run.completed", CreatedAt: fallback.UpdatedAt, Persistent: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, "DROP TABLE messages"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = 9"); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := db.ListMessages(ctx, "ses_backfill", nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]storage.Message, len(page.Messages))
	for _, message := range page.Messages {
		byID[message.ID] = message
	}
	if len(byID) != 10 {
		t.Fatalf("backfilled messages=%+v", page.Messages)
	}
	if message, ok := byID["msg_answer_"+fallbackRun.ID]; !ok || message.Content != "same final text" || message.Status != "recorded" {
		t.Fatalf("linked fallback message=%+v exists=%v", message, ok)
	}
	checks := []struct {
		id, role, kind, status, content string
	}{
		{"msg_user_turn_backfill", "user", "input", "recorded", "original input"},
		{"msg_response_response_backfill", "assistant", "response", "recorded", "AB"},
		{"msg_question_inv_backfill", "assistant", "question", "recorded", "Select region"},
		{"msg_answer_inv_backfill", "user", "answer", "recorded", "eu-west"},
		{"msg_approval_approval_backfill_requested", "system", "approval_requested", "recorded", "Approval requested for write_file"},
		{"msg_approval_approval_backfill_decision", "system", "approval_decision", "recorded", "deny"},
		{"msg_status_run_backfill", "system", "run_status", "failed", "provider unavailable"},
	}
	for _, check := range checks {
		message, ok := byID[check.id]
		if !ok || message.Role != check.role || message.Kind != check.kind || message.Status != check.status || message.Content != check.content {
			t.Fatalf("message %q=%+v exists=%v, want %+v", check.id, message, ok, check)
		}
	}
}

func TestCancelledRunStatusProjection(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	db := openTest(t)
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_cancel", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	run := storage.Run{ID: "run_cancel", SessionID: "ses_cancel", Status: domain.RunRunning, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	next := run
	next.Status = domain.RunCancelled
	next.Version = 2
	next.UpdatedAt = now.Add(time.Second)
	if err := db.CommitRunTransition(ctx, next, 1, storage.Event{Type: "run.CANCELLED", CreatedAt: next.UpdatedAt, Persistent: true}); err != nil {
		t.Fatal(err)
	}
	page, err := db.ListMessages(ctx, "ses_cancel", nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Kind != "run_status" || page.Messages[0].Status != "cancelled" || page.Messages[0].Content != string(domain.RunCancelled) || page.Messages[0].Event != "run.CANCELLED" {
		t.Fatalf("cancel message=%+v", page.Messages)
	}
}

func TestPruneRunsRemovesRunMessagesAndRetainsUnlinkedTurnInput(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 8, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	db := openTest(t)
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_prune_messages", CreatedAt: old, UpdatedAt: old}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateTurn(ctx, storage.Turn{ID: "turn_prune_messages", SessionID: "ses_prune_messages", UserInput: "retained turn input", Status: "completed", CreatedAt: old}); err != nil {
		t.Fatal(err)
	}
	run := storage.Run{ID: "run_prune_messages", SessionID: "ses_prune_messages", TurnID: "turn_prune_messages", Status: domain.RunRunning, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: old, UpdatedAt: old}
	if err := db.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	next := run
	next.Status = domain.RunCompleted
	next.Version = 2
	next.FinalAnswer = "expired run answer"
	next.UpdatedAt = old.Add(time.Second)
	if err := db.CommitRunTransition(ctx, next, 1, storage.Event{Type: "run.completed", CreatedAt: next.UpdatedAt, Persistent: true}); err != nil {
		t.Fatal(err)
	}
	result, err := db.PruneRuns(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.RunsDeleted != 1 || result.Tables["messages"] != 2 {
		t.Fatalf("prune result=%+v", result)
	}
	page, err := db.ListMessages(ctx, "ses_prune_messages", nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].ID != "msg_user_turn_prune_messages" || page.Messages[0].RunID != "" || page.Messages[0].Content != "retained turn input" {
		t.Fatalf("retained input=%+v", page.Messages)
	}
}
