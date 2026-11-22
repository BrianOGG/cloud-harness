package agent

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestDelegateAgentToolIntersection(t *testing.T) {
	readonly := []string{"git_diff", "git_log", "git_status", "grep", "list_dir", "list_files", "read_file"}
	for _, tc := range []struct {
		name, agent, definition, allowed string
		wantTools                        []string
	}{
		{name: "explorer", agent: "explorer", allowed: `["git_diff","git_log","git_status","grep","list_dir","list_files","read_file","write_file","edit_file","run_command","ask_user","delegate_task"]`, wantTools: readonly},
		{name: "reviewer", agent: "reviewer", wantTools: readonly},
		{name: "caller narrower", agent: "custom", definition: "tools: [read_file, grep, write_file]\n", allowed: `["read_file"]`, wantTools: []string{"read_file"}},
		{name: "definition narrower", agent: "custom", definition: "tools: [grep]\n", allowed: `["grep","list_files","write_file"]`, wantTools: []string{"grep"}},
		{name: "workspace override", agent: "explorer", definition: "tools: [grep]\n", wantTools: []string{"grep"}},
		{name: "empty definition", agent: "custom", definition: "tools: []\n"},
		{name: "disjoint", agent: "custom", definition: "tools: [grep]\n", allowed: `["read_file"]`},
		{name: "empty caller list", agent: "custom", definition: "tools: [grep]\n", allowed: `[]`},
		{name: "definition without tools", agent: "custom", allowed: `["read_file","write_file"]`, wantTools: []string{"read_file", "write_file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			args := `{"goal":"inspect this workspace","agent":` + string(must(t, tc.agent))
			if tc.allowed != "" {
				args += `,"allowed_tools":` + tc.allowed
			}
			mock := mockAgentDelegate(args+"}", toolResp(domain.ToolCall{
				ProviderCallID: "finish", Name: domain.ToolFinishTurn, Arguments: json.RawMessage(`{"final_answer":"child done"}`),
			}))
			m, db, workspace := testManager(t, mock)
			if tc.agent == "custom" || tc.definition != "" {
				writeDelegateDefinition(t, workspace, tc.agent, tc.definition, "")
			}
			// These are parent permissions that a definition can only narrow.
			m.Grants = []domain.Grant{
				{Tool: "write_file", ResourceScope: workspace},
				{Tool: "edit_file", ResourceScope: workspace},
				{Tool: "run_command", ResourceScope: workspace},
			}
			parent := runAgentDelegate(t, m)
			child := delegateChildRun(t, db, parent)
			if child.Status != domain.RunCompleted || child.FinalAnswer != "child done" {
				t.Fatalf("child could not finish: %+v", child)
			}
			if len(mock.Requests) != 3 {
				t.Fatalf("got %d model requests, want parent/child/parent", len(mock.Requests))
			}
			var offered []string
			for _, def := range mock.Requests[1].Tools {
				offered = append(offered, def.Name)
			}
			wantOffered := append([]string{domain.ToolFinishTurn}, tc.wantTools...)
			slices.Sort(offered)
			slices.Sort(wantOffered)
			if !slices.Equal(offered, wantOffered) {
				t.Fatalf("child tools = %v, want %v", offered, wantOffered)
			}
			grants, err := db.ListGrants(ctx, child.ID, child.SessionID, m.now())
			if err != nil {
				t.Fatal(err)
			}
			var granted []string
			for _, g := range grants {
				if g.RunID != child.ID || g.ResourceScope != workspace {
					t.Fatalf("unexpected child grant: %+v", g)
				}
				granted = append(granted, g.Tool)
			}
			slices.Sort(granted)
			if !slices.Equal(granted, tc.wantTools) {
				t.Fatalf("child grants = %v, want %v", granted, tc.wantTools)
			}
		})
	}
}

func TestDelegateAgentToolLimitAppliesToDirectAndNestedCalls(t *testing.T) {
	ctx := context.Background()
	mock := mockAgentDelegate(`{"goal":"inspect this workspace","agent":"custom"}`,
		toolResp(
			domain.ToolCall{ProviderCallID: "poll", Name: domain.ToolPoll, Arguments: json.RawMessage(`{"target_tool":"read_file","target_args":{"path":"notes.txt"},"until":{},"max_attempts":1}`)},
			domain.ToolCall{ProviderCallID: "script", Name: domain.ToolExecuteProgram, Arguments: json.RawMessage(`{"script":"def main():\n  return 1\n","allow":["read_file"]}`)},
			domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"notes.txt"}`)},
		), textOnly("child done"),
	)
	m, db, workspace := testManager(t, mock)
	writeDelegateDefinition(t, workspace, "custom", "tools: [poll_tool, execute_program]\n", "")
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A session grant is visible to the shared broker for both runs.
	if err := db.SaveGrant(ctx, domain.Grant{
		ID: domain.NewID("grd_"), SessionID: session.ID, Tool: "read_file", Op: "call", ResourceScope: workspace, State: "active",
	}, m.now()); err != nil {
		t.Fatal(err)
	}
	parent, err := m.RunMessage(ctx, session.ID, "delegate an inspection")
	if err != nil || parent.FinalAnswer != "parent done" {
		t.Fatalf("parent run = %+v, err = %v", parent, err)
	}
	child := delegateChildRun(t, db, parent)
	batches, err := db.LoadBatches(ctx, child.ID)
	if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 3 {
		t.Fatalf("child batches = %+v, err = %v", batches, err)
	}
	poll, script, read := batches[0].Invocations[0], batches[0].Invocations[1], batches[0].Invocations[2]
	if poll.Status != domain.InvFailed || poll.Result == nil || !domain.IsCode(poll.Result.Error, domain.CodeAuthDenied) {
		t.Fatalf("expected poll to reject the target: %+v", poll)
	}
	if script.Status != domain.InvFailed || script.Result == nil || !domain.IsCode(script.Result.Error, domain.CodeScriptPolicy) || !strings.Contains(script.Result.Error.Message, "outside the child tool allowlist") {
		t.Fatalf("expected script to reject the target before execution: %+v", script)
	}
	if read.Status != domain.InvDenied || read.Dispatched || child.Budget.UsedToolCalls != 2 {
		t.Fatalf("direct read was not denied before dispatch: %+v, budget = %+v", read, child.Budget)
	}
}

func TestDelegateAgentBudgetCaps(t *testing.T) {
	for _, tc := range []struct {
		name, definition          string
		wantModelCap, wantToolCap int
		wantExhausted             bool
	}{
		{name: "model limit", definition: "max_model_calls: 1\n", wantModelCap: 1, wantToolCap: 8, wantExhausted: true},
		{name: "tool limit", definition: "max_tool_calls: 1\n", wantModelCap: 3, wantToolCap: 1, wantExhausted: true},
		{name: "no declared limits", wantModelCap: 3, wantToolCap: 8},
		{name: "larger limits", definition: "max_model_calls: 20\nmax_tool_calls: 20\n", wantModelCap: 3, wantToolCap: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"notes.txt"}`)}
			childReplies := []domain.ModelResponse{toolResp(read)}
			if tc.wantToolCap == 1 {
				secondRead := read
				secondRead.ProviderCallID = "read-again"
				childReplies[0] = toolResp(read, secondRead)
			}
			if !tc.wantExhausted {
				childReplies = append(childReplies, textOnly("child done"))
			}
			mock := mockAgentDelegate(`{"goal":"read notes.txt","agent":"custom","allowed_tools":["read_file"],"max_model_calls":3,"max_tool_calls":20}`, childReplies...)
			m, db, workspace := testManager(t, mock)
			writeDelegateDefinition(t, workspace, "custom", tc.definition, "")
			if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("notes"), 0o644); err != nil {
				t.Fatal(err)
			}
			m.UseBudget = true
			m.InitialBudget = domain.DefaultBudget()
			m.InitialBudget.MaxModelCalls, m.InitialBudget.MaxToolCalls = 5, 9
			parent := runAgentDelegate(t, m)
			child := delegateChildRun(t, db, parent)
			if child.Budget.MaxModelCalls != tc.wantModelCap || child.Budget.MaxToolCalls != tc.wantToolCap || child.Budget.UsedToolCalls != 1 {
				t.Fatalf("unexpected child budget: %+v", child.Budget)
			}
			if child.Budget.UsedModelCalls != len(childReplies) || len(mock.Requests) != len(childReplies)+2 {
				t.Fatalf("unexpected model calls: child budget = %+v, recorded requests = %d", child.Budget, len(mock.Requests))
			}
			finished := checkDelegateEvents(t, db, parent, child, "custom")
			if tc.wantExhausted {
				if child.Status != domain.RunFailed || finished["wait_reason"] != string(domain.WaitBudget) || finished["success"] != false {
					t.Fatalf("expected budget exhaustion to fail the child: %+v, event = %v", child, finished)
				}
				if tc.wantToolCap == 1 {
					batches, err := db.LoadBatches(context.Background(), child.ID)
					if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 2 {
						t.Fatalf("child batches = %+v, err = %v", batches, err)
					}
					attempt := batches[0].Invocations[1]
					if attempt.Status != domain.InvFailed || attempt.Result == nil || !domain.IsCode(attempt.Result.Error, domain.CodeBudget) {
						t.Fatalf("second tool did not exhaust the tool cap: %+v", attempt)
					}
				}
			} else if child.Status != domain.RunCompleted || child.FinalAnswer != "child done" {
				t.Fatalf("child did not complete with the derived budget: %+v", child)
			}
		})
	}
}

func TestDelegateAgentModelAndSystemPrompt(t *testing.T) {
	const criteria = "Include notes.txt in the summary."
	const contract = "a summary under 25 words"
	for _, tc := range []struct {
		name, parentModel, definition, system, wantModel string
	}{
		{name: "override", parentModel: "parent-model", definition: "model: child-model\n", system: "Review the workspace carefully.\n\nCite file paths.", wantModel: "child-model"},
		{name: "inherit", parentModel: "parent-model", wantModel: "parent-model"},
		{name: "provider default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := mockAgentDelegate(string(must(t, map[string]string{
				"goal": "review the notes", "agent": "custom", "success_criteria": criteria, "output_contract": contract,
			})), textOnly("Reviewed notes.txt."))
			m, db, workspace := testManager(t, mock)
			m.Model = tc.parentModel
			writeDelegateDefinition(t, workspace, "custom", tc.definition, tc.system)
			parent := runAgentDelegate(t, m)
			child := delegateChildRun(t, db, parent)
			if len(mock.Requests) != 3 {
				t.Fatalf("got %d model requests, want parent/child/parent", len(mock.Requests))
			}
			if a, b, c := mock.Requests[0].Model, mock.Requests[1].Model, mock.Requests[2].Model; a != tc.parentModel || b != tc.wantModel || c != tc.parentModel {
				t.Fatalf("request models = %q, %q, %q, want %q, %q, %q", a, b, c, tc.parentModel, tc.wantModel, tc.parentModel)
			}
			system := mock.Requests[1].Messages[0]
			prefix := childBaseSystem
			if tc.system != "" {
				prefix += "\n\n" + tc.system
			}
			prefix += "\nSuccess criteria (what counts as done): " + criteria
			if system.Role != "system" || !strings.HasPrefix(system.Content, prefix) || !strings.HasSuffix(system.Content, "\nOutput contract (expected output shape): "+contract) {
				t.Fatalf("unexpected child system prompt: %q", system.Content)
			}
			finished := checkDelegateEvents(t, db, parent, child, "custom")
			if child.Status != domain.RunCompleted || finished["success"] != true {
				t.Fatalf("expected accepted child result, got %+v, event = %v", child, finished)
			}
		})
	}
}

func TestDelegateUnknownAgentListsAvailableNames(t *testing.T) {
	ctx := context.Background()
	mock := mockAgentDelegate(`{"goal":"inspect","agent":"missing"}`)
	m, db, workspace := testManager(t, mock)
	writeDelegateDefinition(t, workspace, "custom", "", "")
	parent := runAgentDelegate(t, m)
	runs, err := db.ListRuns(ctx, parent.SessionID)
	if err != nil || len(runs) != 1 || parent.Budget.UsedChildRuns != 0 || len(mock.Requests) != 2 {
		t.Fatalf("unknown agent started a child: runs = %+v, budget = %+v, err = %v", runs, parent.Budget, err)
	}
	batches, err := db.LoadBatches(ctx, parent.ID)
	if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 1 {
		t.Fatalf("parent batches = %+v, err = %v", batches, err)
	}
	inv := batches[0].Invocations[0]
	want := `agents: unknown agent "missing"; available agents: custom, explorer, reviewer`
	if inv.Status != domain.InvFailed || inv.Result == nil || inv.Result.Error == nil || inv.Result.Error.Code != domain.CodeToolError || inv.Result.Error.Message != want {
		t.Fatalf("delegate_task result = %+v, want tool_error with %q", inv.Result, want)
	}
	i := slices.IndexFunc(mock.Requests[0].Tools, func(tool provider.ToolDefinition) bool { return tool.Name == domain.ToolDelegateTask })
	if i < 0 {
		t.Fatal("parent request did not offer delegate_task")
	}
	tool := mock.Requests[0].Tools[i]
	var schema struct {
		Properties map[string]struct{ Type string } `json:"properties"`
		Required   []string                         `json:"required"`
	}
	if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties["agent"].Type != "string" || slices.Contains(schema.Required, "agent") {
		t.Fatalf("delegate_task agent should be optional and a string: %s", tool.Parameters)
	}
	for _, text := range []string{"explorer", "reviewer", ".agents/agents/*.md"} {
		if !strings.Contains(tool.Description, text) {
			t.Fatalf("delegate_task description lacks %q: %q", text, tool.Description)
		}
	}
	if strings.Contains(tool.Description+string(tool.Parameters), "custom") {
		t.Fatalf("workspace agent name leaked into tool definition: %+v", tool)
	}
}

func TestDelegateEmptyAgentUsesExistingBehavior(t *testing.T) {
	for _, args := range []string{`{"goal":"inspect"}`, `{"goal":"inspect","agent":""}`} {
		t.Run(args, func(t *testing.T) {
			mock := mockAgentDelegate(args, textOnly("child done"))
			m, db, workspace := testManager(t, mock)
			// Any registry load would reject this file.
			writeDelegateDefinition(t, workspace, "invalid", "unknown_field: yes\n", "")
			parent := runAgentDelegate(t, m)
			child := delegateChildRun(t, db, parent)
			if child.Status != domain.RunCompleted || len(mock.Requests) != 3 {
				t.Fatalf("anonymous child = %+v, model requests = %d", child, len(mock.Requests))
			}
			req := mock.Requests[1]
			if req.Messages[0].Content != childBaseSystem || req.Model != "" || !slices.ContainsFunc(req.Tools, func(tool provider.ToolDefinition) bool { return tool.Name == "read_file" }) {
				t.Fatalf("anonymous child prompt, model or tools changed: %+v", req)
			}
			finished := checkDelegateEvents(t, db, parent, child, "")
			want := map[string]any{"child_run_id": child.ID, "status": string(domain.RunCompleted), "success": true, "unmet": ""}
			if !maps.Equal(finished, want) {
				t.Fatalf("anonymous child.finished payload = %v, want %v", finished, want)
			}
		})
	}
}

func mockAgentDelegate(args string, childReplies ...domain.ModelResponse) *provider.Mock {
	delegate := toolResp(domain.ToolCall{ProviderCallID: "delegate", Name: domain.ToolDelegateTask, Arguments: json.RawMessage(args)})
	responses := append([]domain.ModelResponse{delegate}, childReplies...)
	return &provider.Mock{Responses: append(responses, textOnly("parent done"))}
}

func writeDelegateDefinition(t *testing.T, workspace, name, fields, system string) {
	t.Helper()
	dir := filepath.Join(workspace, ".agents", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	text := "---\ndescription: Delegation fixture\n" + fields + "---\n" + system
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runAgentDelegate(t *testing.T, m *Manager) storage.Run {
	t.Helper()
	ctx := context.Background()
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := m.RunMessage(ctx, session.ID, "delegate an inspection")
	if err != nil || parent.Status != domain.RunCompleted || parent.FinalAnswer != "parent done" {
		t.Fatalf("parent run = %+v, err = %v", parent, err)
	}
	return parent
}

func delegateChildRun(t *testing.T, db storage.Store, parent storage.Run) storage.Run {
	t.Helper()
	runs, err := db.ListRuns(context.Background(), parent.SessionID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %+v, want parent and child, err = %v", runs, err)
	}
	for _, run := range runs {
		if run.ParentRunID == parent.ID {
			if run.WorkspaceID != parent.WorkspaceID {
				t.Fatalf("child workspace = %q, want parent workspace %q", run.WorkspaceID, parent.WorkspaceID)
			}
			return run
		}
	}
	t.Fatalf("no child of parent %s in runs %+v", parent.ID, runs)
	return storage.Run{}
}

func checkDelegateEvents(t *testing.T, db storage.Store, parent, child storage.Run, name string) map[string]any {
	t.Helper()
	events, err := db.ListEvents(context.Background(), parent.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	var finished map[string]any
	for _, e := range events {
		if e.Type != "child.started" && e.Type != "child.finished" {
			continue
		}
		types = append(types, e.Type)
		var payload map[string]any
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		got, present := payload["agent"]
		if present != (name != "") || present && got != name || payload["child_run_id"] != child.ID {
			t.Fatalf("%s payload = %v, want child %s with agent %q", e.Type, payload, child.ID, name)
		}
		if e.Type == "child.finished" {
			finished = payload
		} else if name == "" && len(payload) != 1 {
			t.Fatalf("anonymous child.started payload changed: %v", payload)
		}
	}
	if !slices.Equal(types, []string{"child.started", "child.finished"}) || finished["status"] != string(child.Status) {
		t.Fatalf("unexpected child event types %v or final payload %v", types, finished)
	}
	return finished
}
