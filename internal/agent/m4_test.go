package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

func TestScriptRefusesSideEffectsAndNewPermissions(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "p1", Name: "execute_program", Arguments: json.RawMessage(`{"script":"def main(:\n","allow":["write_file"]}`)}),
		textOnly("stopped"),
	}}
	m, db, root := testManager(t, mock)
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "run a script")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("%+v %v", run, err)
	}
	batches, _ := db.LoadBatches(ctx, run.ID)
	inv := batches[0].Invocations[0]
	if inv.Status != domain.InvFailed || inv.Result == nil || inv.Result.Error == nil || inv.Result.Error.Code != domain.CodeScriptPolicy {
		t.Fatalf("policy %+v", inv)
	}
	if strings.Contains(inv.Result.Error.Message, "syntax") {
		t.Fatal("worker interpreted a script that was not authorized")
	}
	if _, err := os.Stat(filepath.Join(root, "pwn.txt")); !os.IsNotExist(err) {
		t.Fatal("unconfirmed write created a file")
	}

	mock2 := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "p2", Name: "execute_program", Arguments: json.RawMessage(`{"script":"def main():\n  b = broker.invoke(\"write_file\", {\"path\": \"pwn.txt\", \"content\": \"x\"})\n  code = \"\"\n  if b[\"error\"] != None:\n    code = b[\"error\"][\"code\"]\n  return {\"code\": code}\n","allow":["read_file"]}`)}),
		textOnly("done"),
	}}
	m.Provider = mock2
	run, err = m.RunMessage(ctx, ses.ID, "try a write from the script")
	if err != nil || run.Status != domain.RunWaiting {
		if err != nil || run.Status != domain.RunCompleted {
			t.Fatalf("inner %+v %v", run, err)
		}
	}
	if run.Status == domain.RunWaiting {
		t.Fatal("script paused for a new permission")
	}
	batches, _ = db.LoadBatches(ctx, run.ID)
	body := string(batches[0].Invocations[0].Result.Data)
	if !strings.Contains(body, "auth_denied") {
		t.Fatalf("inner result %s", body)
	}
	if _, err := os.Stat(filepath.Join(root, "pwn.txt")); !os.IsNotExist(err) {
		t.Fatal("script wrote a file")
	}
}

func TestChildHasItsOwnTraceBudgetAndContext(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "d1", Name: "delegate_task", Arguments: json.RawMessage(`{"goal":"CHILD-GOAL","allowed_tools":["read_file"],"max_model_calls":4,"max_tool_calls":4}`)}),
		toolResp(domain.ToolCall{ProviderCallID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"note.txt"}`)}),
		textOnly("child-done"),
		textOnly("parent-done"),
	}}
	m, db, root := testManager(t, mock)
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "PARENT-SECRET do the child task")
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "parent-done" {
		t.Fatalf("%+v %v", run, err)
	}
	runs, _ := db.ListRuns(ctx, ses.ID)
	if len(runs) != 2 {
		t.Fatalf("runs %+v", runs)
	}
	var child storage.Run
	for _, r := range runs {
		if r.ParentRunID == run.ID {
			child = r
		}
	}
	if child.ID == "" || child.FinalAnswer != "child-done" {
		t.Fatalf("child %+v", child)
	}
	if child.Budget.UsedModelCalls != 2 || child.Budget.UsedToolCalls != 1 {
		t.Fatalf("child budget %+v", child.Budget)
	}
	if run.Budget.UsedModelCalls != child.Budget.UsedModelCalls+2 || run.Budget.UsedToolCalls != child.Budget.UsedToolCalls+1 {
		t.Fatalf("parent budget %+v child %+v", run.Budget, child.Budget)
	}
	events, _ := db.ListEvents(ctx, child.ID, 0)
	if len(events) == 0 {
		t.Fatal("child has no trace")
	}
	parentEvents, _ := db.ListEvents(ctx, run.ID, 0)
	joined := ""
	for _, ev := range parentEvents {
		joined += string(ev.Payload)
	}
	if !strings.Contains(joined, child.ID) {
		t.Fatal("parent trace does not name the child")
	}
	ck, err := db.LatestCheckpoint(ctx, child.ID)
	if err != nil || len(ck) == 0 || strings.Contains(string(ck), "PARENT-SECRET") {
		t.Fatalf("checkpoint %s %v", ck, err)
	}
	isolated := false
	for _, req := range mock.Requests {
		blob := messageText(req.Messages)
		if strings.Contains(blob, "CHILD-GOAL") && !strings.Contains(blob, "PARENT-SECRET") {
			isolated = true
		}
	}
	if !isolated {
		t.Fatal("child context included the parent secret")
	}
}

func TestParentCancelReachesChild(t *testing.T) {
	ctx := context.Background()
	started := make(chan struct{})
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "d1", Name: "delegate_task", Arguments: json.RawMessage(`{"goal":"wait","allowed_tools":["block"]}`)}),
		toolResp(domain.ToolCall{ProviderCallID: "b1", Name: "block", Arguments: json.RawMessage(`{}`)}),
		textOnly("should-not-finish"),
	}}
	m, db, _ := testManager(t, mock)
	m.Broker.Register(tools.Spec{
		Name: "block", Description: "block until cancelled", Readonly: true, Idempotent: true, SideEffectClass: "none",
		Execute: func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	ses, _ := m.CreateSession(ctx)
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = m.RunMessage(ctx, ses.ID, "cancel me")
	}()
	select {
	case <-started:
	case <-done:
		t.Fatalf("finished before block: %v", runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("child did not start")
	}
	var parentID string
	deadline := time.Now().Add(2 * time.Second)
	for parentID == "" && time.Now().Before(deadline) {
		runs, _ := db.ListRuns(ctx, ses.ID)
		for _, r := range runs {
			if r.ParentRunID == "" {
				parentID = r.ID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if parentID == "" {
		t.Fatal("parent run missing")
	}
	if _, err := m.Cancel(context.Background(), parentID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not unblock the child")
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	runs, _ := db.ListRuns(ctx, ses.ID)
	if len(runs) != 2 {
		t.Fatalf("runs %+v", runs)
	}
	for _, r := range runs {
		if r.Status != domain.RunCancelled {
			t.Fatalf("not cancelled %+v", r)
		}
	}
}
