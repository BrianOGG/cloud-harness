package sqlite

import (
	"context"
	"encoding/json"
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
	approval := domain.Approval{ID: "approval_1", RunID: run.ID, InvocationID: "inv_write", Tool: "write_file", State: "pending", CreatedAt: now}
	if err := db.SaveApproval(ctx, approval); err != nil {
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
	cursor := ""
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
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(all) != 7 {
		t.Fatalf("message count %d: %+v", len(all), all)
	}
	wantKinds := map[string]bool{"input": true, "response": true, "question": true, "answer": true, "approval_requested": true, "approval_decision": true, "status": true}
	for _, message := range all {
		if !wantKinds[message.Kind] || message.ID == "" || message.Role == "" || message.CreatedAt.IsZero() {
			t.Fatalf("invalid message %+v", message)
		}
	}
	if page, err := db.ListMessages(ctx, "ses_other", "", 50); err != nil || len(page.Messages) != 0 {
		t.Fatalf("session isolation page=%+v err=%v", page, err)
	}
	if _, err := db.ListMessages(ctx, "ses_history", "not-a-cursor", 2); err == nil || !domain.IsCode(err, domain.CodeInvalidArgument) {
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
	page, err := db.ListMessages(ctx, "ses_history", "", 50)
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
