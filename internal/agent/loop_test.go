package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

func testManager(t *testing.T, mock *provider.Mock) (*Manager, *sqlite.DB, string) {
	t.Helper()
	root := t.TempDir()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	broker := tools.NewBroker(db, db)
	if err := tools.RegisterLocal(broker, root); err != nil {
		t.Fatal(err)
	}
	return &Manager{Store: db, Provider: mock, Broker: broker, Workspace: root, OwnerID: "loop"}, db, root
}

func toolResp(calls ...domain.ToolCall) domain.ModelResponse {
	items := make([]domain.ResponseItem, len(calls))
	for i, c := range calls {
		c.Ordinal = i
		items[i] = domain.ResponseItem{Ordinal: i, Kind: domain.ItemToolCall, ToolCall: &c}
	}
	return domain.ModelResponse{Provider: "mock", Complete: true, Items: items}
}

func TestThreeReadsExecuteInOrderOnce(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(
			domain.ToolCall{ProviderCallID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"alpha.txt"}`)},
			domain.ToolCall{ProviderCallID: "c2", Name: "read_file", Arguments: json.RawMessage(`{"path":"beta.txt"}`)},
			domain.ToolCall{ProviderCallID: "c3", Name: "read_file", Arguments: json.RawMessage(`{"path":"gamma.txt"}`)},
		),
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "three files read"}}},
	}}
	m, db, root := testManager(t, mock)
	for name, body := range map[string]string{"alpha.txt": "A", "beta.txt": "B", "gamma.txt": "C"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "read three")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunCompleted || run.FinalAnswer != "three files read" {
		t.Fatalf("%+v", run)
	}
	batches, err := db.LoadBatches(ctx, run.ID)
	if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 3 {
		t.Fatalf("batches %#v %v", batches, err)
	}
	want := []string{"c1", "c2", "c3"}
	for i, inv := range batches[0].Invocations {
		if inv.ProviderCallID != want[i] || inv.Status != domain.InvSucceeded || inv.Result == nil || !inv.Result.OK {
			t.Fatalf("inv %d %+v", i, inv)
		}
		if !strings.Contains(string(inv.Result.Data), string(rune('A'+i))) && i == 0 && !strings.Contains(string(inv.Result.Data), "A") {
			t.Fatalf("result %s", inv.Result.Data)
		}
	}
	if strings.Count(string(must(t, batches)), `"provider_call_id":"c1"`) != 1 {
		t.Fatal("first call id was duplicated")
	}
}

func TestMixedControlHasNoSideEffects(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{{
		Provider: "mock", Complete: true,
		Items: []domain.ResponseItem{
			{Ordinal: 0, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "f", Name: "finish_turn", Arguments: json.RawMessage(`{"final_answer":"no"}`)}},
			{Ordinal: 1, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "r", Name: "write_file", Arguments: json.RawMessage(`{"path":"pwn.txt","content":"x"}`)}},
		},
	}, {Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "corrected"}}}}}
	m, _, root := testManager(t, mock)
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "mix")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunCompleted {
		t.Fatalf("%+v", run)
	}
	if _, err := os.Stat(filepath.Join(root, "pwn.txt")); !os.IsNotExist(err) {
		t.Fatal("mixed finish_turn wrote a file")
	}
}

func TestFinishTurnAndTextSummaryPaths(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{{
		Provider: "mock", Complete: true,
		Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{
			ProviderCallID: "f1", Name: "finish_turn",
			Arguments: json.RawMessage(`{"final_answer":"ship it","turn_summary":"decided to ship","memory_delta":[{"op":"ADD","fact":"ship"}],"source_refs":["e1"]}`),
		}}},
	}}}
	m, db, _ := testManager(t, mock)
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "finish")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunCompleted || run.FinalAnswer != "ship it" {
		t.Fatalf("%+v", run)
	}
	summary, err := db.GetTurnSummary(ctx, run.TurnID)
	if err != nil || summary != "decided to ship" {
		t.Fatalf("summary %q %v", summary, err)
	}
	if run.Budget.UsedModelCalls != 1 {
		t.Fatalf("finish_turn called the model again: %+v", run.Budget)
	}
}

func TestDangerousCommandWaitsWithoutRunning(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{toolResp(
		domain.ToolCall{ProviderCallID: "d1", Name: "run_command", Arguments: json.RawMessage(`{"command":"rm -rf ."}`)},
	)}}
	m, db, root := testManager(t, mock)
	sentinel := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("safe"), 0o644); err != nil {
		t.Fatal(err)
	}
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "delete")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval {
		t.Fatalf("%+v", run)
	}
	body, err := os.ReadFile(sentinel)
	if err != nil || string(body) != "safe" {
		t.Fatal("dangerous command ran")
	}
	batches, err := db.LoadBatches(ctx, run.ID)
	if err != nil || batches[0].Invocations[0].Status != domain.InvWaitingApproval || batches[0].Invocations[0].Dispatched {
		t.Fatalf("batch %#v %v", batches, err)
	}
}

func must(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
