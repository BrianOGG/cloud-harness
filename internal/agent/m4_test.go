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

func TestChildWriteFileWithAllowedTools(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{
			ProviderCallID: "d1",
			Name:           "delegate_task",
			Arguments:      json.RawMessage(`{"goal":"write child.txt","allowed_tools":["write_file","read_file"],"max_model_calls":4,"max_tool_calls":4}`),
		}),
		toolResp(domain.ToolCall{
			ProviderCallID: "cw1",
			Name:           "write_file",
			Arguments:      json.RawMessage(`{"path":"child.txt","content":"written-by-child"}`),
		}),
		textOnly("child-wrote-file"),
		textOnly("parent-done"),
	}}
	m, db, root := testManager(t, mock)
	m.Grants = []domain.Grant{{Tool: "write_file", Op: "call", ResourceScope: root, State: "active", Subject: "test"}}
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "delegate file creation")
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "parent-done" {
		t.Fatalf("run %+v %v", run, err)
	}
	body, err := os.ReadFile(filepath.Join(root, "child.txt"))
	if err != nil || string(body) != "written-by-child" {
		t.Fatalf("child.txt %q %v", body, err)
	}
	runs, err := db.ListRuns(ctx, ses.ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs %+v %v", runs, err)
	}
	var child storage.Run
	for _, r := range runs {
		if r.ParentRunID == run.ID {
			child = r
		}
	}
	if child.ID == "" || child.Status != domain.RunCompleted {
		t.Fatalf("child %+v", child)
	}
	childBatches, err := db.LoadBatches(ctx, child.ID)
	if err != nil || len(childBatches) != 1 || len(childBatches[0].Invocations) != 1 {
		t.Fatalf("child batches %+v %v", childBatches, err)
	}
	if childBatches[0].Invocations[0].Status != domain.InvSucceeded {
		t.Fatalf("child write_file invocation %+v", childBatches[0].Invocations[0])
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

func TestDelegateTaskSuccessCriteriaMet(t *testing.T) {
	ctx := context.Background()
	criteria := "output includes runnable test file paths and go test passes"
	contract := "return only JSON: {files:[], tests_passed:bool}"
	childAnswer := `{"files":["internal/agent/m4_test.go"],"tests_passed":true}`
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{
			ProviderCallID: "d1",
			Name:           "delegate_task",
			Arguments: json.RawMessage(`{
				"goal":"run package tests",
				"success_criteria":"` + criteria + `",
				"output_contract":"` + contract + `"
			}`),
		}),
		textOnly(childAnswer),
		textOnly("parent-done"),
	}}
	m, db, _ := testManager(t, mock)
	ses, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, ses.ID, "verify tests via child")
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "parent-done" {
		t.Fatalf("run %+v %v", run, err)
	}
	if len(mock.Requests) != 3 {
		t.Fatalf("expected 3 model calls, got %d", len(mock.Requests))
	}
	childReq := mock.Requests[1]
	sysText := childReq.Messages[0].Content
	userText := childReq.Messages[len(childReq.Messages)-1].Content
	if !strings.Contains(sysText, criteria) || !strings.Contains(sysText, contract) {
		t.Fatalf("child system prompt missing criteria/contract: %q", sysText)
	}
	if !strings.Contains(userText, criteria) || !strings.Contains(userText, contract) {
		t.Fatalf("child user message missing criteria/contract: %q", userText)
	}
	batches, err := db.LoadBatches(ctx, run.ID)
	if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 1 {
		t.Fatalf("batches %+v %v", batches, err)
	}
	var got struct {
		ChildRunID string `json:"child_run_id"`
		Status     string `json:"status"`
		Summary    string `json:"summary"`
		Success    bool   `json:"success"`
		Unmet      string `json:"unmet"`
	}
	if err := json.Unmarshal(batches[0].Invocations[0].Result.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ChildRunID == "" || got.Status != string(domain.RunCompleted) || got.Summary != childAnswer || !got.Success || got.Unmet != "" {
		t.Fatalf("unexpected delegate result: %+v", got)
	}
	parentFollowUp := messageText(mock.Requests[2].Messages)
	if !strings.Contains(parentFollowUp, `"success":true`) || !strings.Contains(parentFollowUp, `"unmet":""`) {
		t.Fatalf("parent did not receive judged result: %s", parentFollowUp)
	}
}

func TestDelegateTaskSuccessCriteriaUnmet(t *testing.T) {
	ctx := context.Background()
	criteria := "output includes runnable test file paths and go test passes"
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{
			ProviderCallID: "d1",
			Name:           "delegate_task",
			Arguments: json.RawMessage(`{
				"goal":"run package tests",
				"success_criteria":"` + criteria + `",
				"output_contract":"a summary under 200 words"
			}`),
		}),
		textOnly("UNMET: internal/agent/m4_test.go failed to compile"),
		textOnly("parent-saw-self-assessed-unmet"),
		toolResp(domain.ToolCall{
			ProviderCallID: "d2",
			Name:           "delegate_task",
			Arguments: json.RawMessage(`{
				"goal":"run package tests",
				"success_criteria":"` + criteria + `",
				"output_contract":"return only JSON: {files:[], tests_passed:bool}"
			}`),
		}),
		textOnly(`{"files":["internal/agent/m4_test.go"],"tests_passed":false}`),
		textOnly("parent-saw-summary-review-unmet"),
	}}
	m, db, _ := testManager(t, mock)
	ses, _ := m.CreateSession(ctx)

	run1, err := m.RunMessage(ctx, ses.ID, "first attempt")
	if err != nil || run1.Status != domain.RunCompleted || run1.FinalAnswer != "parent-saw-self-assessed-unmet" {
		t.Fatalf("run1 %+v %v", run1, err)
	}
	batches1, err := db.LoadBatches(ctx, run1.ID)
	if err != nil || len(batches1) != 1 {
		t.Fatalf("batches1 %+v %v", batches1, err)
	}
	var got1 struct {
		ChildRunID string `json:"child_run_id"`
		Status     string `json:"status"`
		Success    bool   `json:"success"`
		Unmet      string `json:"unmet"`
	}
	if err := json.Unmarshal(batches1[0].Invocations[0].Result.Data, &got1); err != nil {
		t.Fatal(err)
	}
	if got1.Status != string(domain.RunCompleted) || got1.Success || !strings.Contains(got1.Unmet, "failed to compile") {
		t.Fatalf("expected unmet self-assessment without terminal status failure, got %+v", got1)
	}
	child1, err := db.GetRun(ctx, got1.ChildRunID)
	if err != nil || child1.Status != domain.RunCompleted {
		t.Fatalf("failed judgement changed child1 terminal state: %+v %v", child1, err)
	}

	run2, err := m.RunMessage(ctx, ses.ID, "second attempt")
	if err != nil || run2.Status != domain.RunCompleted || run2.FinalAnswer != "parent-saw-summary-review-unmet" {
		t.Fatalf("run2 %+v %v", run2, err)
	}
	batches2, err := db.LoadBatches(ctx, run2.ID)
	if err != nil || len(batches2) != 1 {
		t.Fatalf("batches2 %+v %v", batches2, err)
	}
	var got2 struct {
		ChildRunID string `json:"child_run_id"`
		Status     string `json:"status"`
		Success    bool   `json:"success"`
		Unmet      string `json:"unmet"`
	}
	if err := json.Unmarshal(batches2[0].Invocations[0].Result.Data, &got2); err != nil {
		t.Fatal(err)
	}
	if got2.Status != string(domain.RunCompleted) || got2.Success || !strings.Contains(got2.Unmet, "tests_passed is false") {
		t.Fatalf("expected unmet summary review without terminal status failure, got %+v", got2)
	}
	child2, err := db.GetRun(ctx, got2.ChildRunID)
	if err != nil || child2.Status != domain.RunCompleted {
		t.Fatalf("failed judgement changed child2 terminal state: %+v %v", child2, err)
	}
}

func TestChildWaitingReturnsStructuredErrorWithoutOrphaning(t *testing.T) {
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{
			ProviderCallID: "d1",
			Name:           "delegate_task",
			Arguments:      json.RawMessage(`{"goal":"ask a question","max_model_calls":4,"max_tool_calls":4}`),
		}),
		toolResp(domain.ToolCall{
			ProviderCallID: "q1",
			Name:           domain.ToolAskUser,
			Arguments:      json.RawMessage(`{"question":"what should I do?"}`),
		}),
		textOnly("parent-handled-child-wait"),
	}}
	m, db, _ := testManager(t, mock)
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "delegate interactive task")
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "parent-handled-child-wait" {
		t.Fatalf("parent run %+v %v", run, err)
	}
	batches, err := db.LoadBatches(ctx, run.ID)
	if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 1 {
		t.Fatalf("batches %+v %v", batches, err)
	}
	var got struct {
		ChildRunID string `json:"child_run_id"`
		Status     string `json:"status"`
		WaitReason string `json:"wait_reason"`
		Success    bool   `json:"success"`
		Unmet      string `json:"unmet"`
		Error      string `json:"error"`
	}
	if err := json.Unmarshal(batches[0].Invocations[0].Result.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ChildRunID == "" || got.Success || got.Unmet == "" || got.Error == "" {
		t.Fatalf("expected structured failure on child WAITING, got %+v", got)
	}
	child, err := db.GetRun(ctx, got.ChildRunID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Status == domain.RunWaiting || child.Status == domain.RunCompleted {
		t.Fatalf("child run was orphaned or marked completed: %+v", child)
	}
}
