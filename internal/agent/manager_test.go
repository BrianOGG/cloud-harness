package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
)

func TestMockRunCompletesWithoutAKey(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock := &provider.Mock{}
	m := &Manager{Store: db, Provider: mock, OwnerID: "owner-a"}
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "ping")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunCompleted || run.FinalAnswer != "Mock answer: ping" {
		t.Fatalf("%+v", run)
	}
	if run.Budget.UsedModelCalls != 1 {
		t.Fatalf("budget %+v", run.Budget)
	}
	events, err := db.ListEvents(ctx, run.ID, 0)
	if err != nil || len(events) < 2 {
		t.Fatalf("events %#v %v", events, err)
	}
	second, err := m.Cancel(ctx, run.ID)
	if err == nil || second.Status == domain.RunCancelled {
		t.Fatalf("completed run was cancelled: %+v %v", second, err)
	}
}

func TestBudgetExhaustionDoesNotCallProvider(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock := &provider.Mock{}
	m := &Manager{Store: db, Provider: mock, UseBudget: true, InitialBudget: domain.Budget{MaxModelCalls: 0}}
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "ping")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitBudget {
		t.Fatalf("%+v", run)
	}
	if len(mock.Requests) != 0 {
		t.Fatal("provider was called without budget")
	}
	again, err := m.Resume(ctx, run.ID, ResumeInput{})
	if !domain.IsCode(err, domain.CodeBudget) || again.ID != run.ID || again.Status != domain.RunWaiting {
		t.Fatalf("resume changed the run: %+v %v", again, err)
	}
	cancelled, err := m.Cancel(ctx, run.ID)
	if err != nil || cancelled.Status != domain.RunCancelled || cancelled.ID != run.ID {
		t.Fatalf("cancel %+v %v", cancelled, err)
	}
	again, err = m.Cancel(ctx, run.ID)
	if err != nil || again.Status != domain.RunCancelled {
		t.Fatalf("second cancel %+v %v", again, err)
	}
}

func TestToolBatchIsStoredAndOpaqueStaysOutOfTrace(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mock := &provider.Mock{Responses: []domain.ModelResponse{{
		Provider: "mock", Complete: true,
		Items: []domain.ResponseItem{
			{Ordinal: 0, Kind: domain.ItemText, Text: "reading"},
			{Ordinal: 1, Kind: domain.ItemOpaque, Opaque: &domain.OpaqueItem{Provider: "mock", Kind: "reasoning", Payload: []byte("SECRET-PAYLOAD"), Signature: []byte("SIG-OPAQUE")}},
			{Ordinal: 2, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "c1", Name: "read_file", Arguments: []byte(`{"path":"a"}`)}},
		},
	}}}
	m := &Manager{Store: db, Provider: mock}
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "read a")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunCompleted {
		t.Fatalf("%+v", run)
	}
	summary, err := db.GetTurnSummary(ctx, run.TurnID)
	if err != nil || !strings.Contains(summary, "Turn summary:") {
		t.Fatalf("summary %q %v", summary, err)
	}
	batches, err := db.LoadBatches(ctx, run.ID)
	if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 1 {
		t.Fatalf("batches %#v %v", batches, err)
	}
	if batches[0].Invocations[0].Dispatched || batches[0].Invocations[0].Status != domain.InvDenied {
		t.Fatalf("tool ran without a broker: %+v", batches[0].Invocations[0])
	}
	if len(mock.Requests) < 2 || len(mock.Requests[1].Continuation) == 0 || mock.Requests[1].Continuation[1].Opaque == nil || string(mock.Requests[1].Continuation[1].Opaque.Signature) != "SIG-OPAQUE" {
		t.Fatalf("continuation was not replayed: %+v", mock.Requests)
	}
	visible, err := db.ListVisibleResponses(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := db.ListEvents(ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(visible)
	evRaw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "SECRET-PAYLOAD") || strings.Contains(string(raw), "SIG-OPAQUE") || strings.Contains(string(evRaw), "SECRET-PAYLOAD") {
		t.Fatalf("trace leaked opaque")
	}
	blob, err := db.LoadContinuation(ctx, visible[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := domain.MergeFromStorage("mock", visible[0].Items, blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(merged[1].Opaque.Payload) != "SECRET-PAYLOAD" || string(merged[1].Opaque.Signature) != "SIG-OPAQUE" {
		t.Fatalf("blob did not round-trip %#v", merged[1].Opaque)
	}
}
