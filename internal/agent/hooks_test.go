package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/hooks"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

func putAgentHooks(t *testing.T, workspace string, actions map[hooks.Point][]hooks.Action, goCheck bool) {
	t.Helper()
	putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "hooks.json"), string(must(t, map[string]any{"hooks": actions, "go_check": goCheck})))
}

func runHookMessage(t *testing.T, m *Manager) (storage.Run, error) {
	t.Helper()
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return m.RunMessage(t.Context(), session.ID, "perform requested task")
}

func hookEvents(t *testing.T, store storage.Store, runID string) []hooks.Event {
	t.Helper()
	events, err := store.ListEvents(t.Context(), runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []hooks.Event
	for _, event := range events {
		if event.Type == "hook.completed" || event.Type == "hook.warning" {
			var hook hooks.Event
			if err := json.Unmarshal(event.Payload, &hook); err != nil {
				t.Fatal(err)
			}
			if hook.Warning != (event.Type == "hook.warning") || hook.InvocationID != event.InvocationID {
				t.Fatalf("invalid warning/linkage: %+v %+v", event, hook)
			}
			out = append(out, hook)
		}
	}
	return out
}

func modelSawTool(mock *provider.Mock, text string) bool {
	for _, req := range mock.Requests {
		for _, msg := range req.Messages {
			if msg.Role == "tool" && strings.Contains(msg.Content, text) {
				return true
			}
		}
	}
	return false
}

func TestHooksFiveLifecyclePointsInRuntimeOrder(t *testing.T) {
	isolatedPolicyConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"notes.txt"}`)}), textOnly("done"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	putAgentPolicy(t, filepath.Join(workspace, "notes.txt"), "notes")
	actions := make(map[hooks.Point][]hooks.Action)
	want := []hooks.Point{hooks.SessionStart, hooks.UserPromptSubmit, hooks.PreToolUse, hooks.PostToolUse, hooks.Stop}
	for _, point := range want {
		actions[point] = []hooks.Action{{Type: "command", Command: "echo " + string(point)}}
	}
	putAgentHooks(t, workspace, actions, false)
	run, err := runHookMessage(t, m)
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	events := hookEvents(t, db, run.ID)
	if len(events) != len(want) {
		t.Fatalf("hook events: %+v", events)
	}
	for i, event := range events {
		if event.Point != want[i] || strings.TrimSpace(event.Stdout) != string(want[i]) {
			t.Fatalf("event %d: %+v", i, event)
		}
	}
	persisted, _ := db.ListEvents(t.Context(), run.ID, 0)
	policyAt, preAt, dispatchAt, postAt, completeAt := -1, -1, -1, -1, -1
	for i, event := range persisted {
		switch event.Type {
		case "policy.decided":
			policyAt = i
		case "invocation." + string(domain.InvRunning):
			dispatchAt = i
		case "run.completed":
			completeAt = i
		case "hook.completed":
			var h hooks.Event
			_ = json.Unmarshal(event.Payload, &h)
			if h.Point == hooks.PreToolUse {
				preAt = i
			}
			if h.Point == hooks.PostToolUse {
				postAt = i
			}
		}
	}
	if !(policyAt >= 0 && policyAt < preAt && preAt < dispatchAt && dispatchAt < postAt && postAt < completeAt) {
		t.Fatalf("gate/hook order: policy=%d pre=%d dispatch=%d post=%d complete=%d", policyAt, preAt, dispatchAt, postAt, completeAt)
	}
}

func TestPreToolHookBlocksWarnsAndTimesOut(t *testing.T) {
	for _, tc := range []struct {
		name, command, visible string
		timeout                int
		block, warning         bool
	}{
		{"block", "echo repair-before-writing >&2; exit 2", "repair-before-writing", 0, true, false},
		{"warning", "echo nonfatal-warning; exit 9", "", 0, false, true},
		{"timeout", "sleep 1", "timed out", 20, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedPolicyConfig(t)
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"out.txt","content":"written"}`)}), textOnly("handled"),
			}}
			m, db, workspace := testManager(t, mock)
			m.DisableSnapshots = true
			putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), `{"rules":[{"tool":"write_file","effect":"allow"}]}`)
			putAgentHooks(t, workspace, map[hooks.Point][]hooks.Action{hooks.PreToolUse: {{Type: "command", Command: tc.command, TimeoutMS: tc.timeout}}}, false)
			run, err := runHookMessage(t, m)
			if err != nil || run.Status != domain.RunCompleted {
				t.Fatalf("run=%+v err=%v", run, err)
			}
			batches, _ := db.LoadBatches(t.Context(), run.ID)
			inv := batches[0].Invocations[0]
			events := hookEvents(t, db, run.ID)
			if len(events) != 1 || events[0].Blocked != tc.block || events[0].Warning != tc.warning {
				t.Fatalf("events=%+v", events)
			}
			if tc.block {
				if inv.Dispatched || inv.Status != domain.InvDenied || inv.Result == nil || inv.Result.Error == nil || !strings.Contains(inv.Result.Error.Message, tc.visible) || !modelSawTool(mock, tc.visible) || run.Budget.UsedToolCalls != 0 {
					t.Fatalf("blocked hook result not visible: %+v, budget=%+v", inv, run.Budget)
				}
				if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
					t.Fatalf("blocked tool executed: %v", err)
				}
			} else if inv.Status != domain.InvSucceeded || !inv.Dispatched || run.Budget.UsedToolCalls != 1 {
				t.Fatalf("warning blocked dispatch: %+v, budget=%+v", inv, run.Budget)
			}
		})
	}
}

func TestLifecycleHookCanBlock(t *testing.T) {
	for _, point := range []hooks.Point{hooks.SessionStart, hooks.UserPromptSubmit, hooks.PostToolUse, hooks.Stop} {
		t.Run(string(point), func(t *testing.T) {
			isolatedPolicyConfig(t)
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"notes.txt"}`)}), textOnly("done"),
			}}
			m, db, workspace := testManager(t, mock)
			m.DisableSnapshots = true
			putAgentPolicy(t, filepath.Join(workspace, "notes.txt"), "notes")
			putAgentHooks(t, workspace, map[hooks.Point][]hooks.Action{point: {{Type: "command", Command: "echo lifecycle-rejected >&2; exit 2"}}}, false)
			run, err := runHookMessage(t, m)
			switch point {
			case hooks.SessionStart, hooks.UserPromptSubmit:
				if err == nil || !strings.Contains(err.Error(), "lifecycle-rejected") || run.Status != domain.RunFailed || len(mock.Requests) != 0 {
					t.Fatalf("submission entered context: run=%+v err=%v calls=%d", run, err, len(mock.Requests))
				}
				if hits, err := db.SearchHistory(t.Context(), run.SessionID, "requested", 10); err != nil || len(hits) != 0 {
					t.Fatalf("rejected prompt indexed: %+v %v", hits, err)
				}
			case hooks.PostToolUse:
				if err != nil || run.Status != domain.RunCompleted || !modelSawTool(mock, "lifecycle-rejected") {
					t.Fatalf("post hook result invisible: run=%+v err=%v", run, err)
				}
			case hooks.Stop:
				if err != nil || run.Status != domain.RunFailed || run.Error == nil || !strings.Contains(run.Error.Message, "lifecycle-rejected") || run.FinalAnswer != "" {
					t.Fatalf("Stop did not prevent completion: run=%+v err=%v", run, err)
				}
			}
		})
	}
}

func TestPolicyDenyDoesNotRunPreHook(t *testing.T) {
	isolatedPolicyConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":".env"}`)}), textOnly("denied"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	putAgentHooks(t, workspace, map[hooks.Point][]hooks.Action{hooks.PreToolUse: {{Type: "command", Command: "touch hook-ran; exit 2"}}}, false)
	run, err := runHookMessage(t, m)
	if err != nil || run.Status != domain.RunCompleted || len(hookEvents(t, db, run.ID)) != 0 || !modelSawTool(mock, "policy deny") {
		t.Fatalf("policy ordering: run=%+v err=%v", run, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "hook-ran")); !os.IsNotExist(err) {
		t.Fatalf("policy denied call ran hook: %v", err)
	}
}

func TestHooksSnapshotAndApprovalResume(t *testing.T) {
	global := isolatedPolicyConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"out.txt","content":"written"}`)}), textOnly("handled"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	putAgentHooks(t, workspace, map[hooks.Point][]hooks.Action{
		hooks.SessionStart: {{Type: "command", Command: "echo start"}},
		hooks.PreToolUse:   {{Type: "command", Command: "echo frozen-hook-rejected; exit 2"}},
	}, false)
	run, err := runHookMessage(t, m)
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval || len(hookEvents(t, db, run.ID)) != 1 {
		t.Fatalf("pre hook ran before approval: run=%+v err=%v", run, err)
	}
	putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "hooks.json"), "invalid JSON")
	putAgentPolicy(t, filepath.Join(filepath.Dir(global), "hooks.json"), "invalid JSON")
	fresh := &Manager{Store: db, Provider: mock, Broker: tools.NewBroker(db, db), Workspace: workspace, DisableSnapshots: true}
	run, err = fresh.Resume(t.Context(), run.ID, ResumeInput{Decision: "approve"})
	if err != nil || run.Status != domain.RunCompleted || !modelSawTool(mock, "frozen-hook-rejected") {
		t.Fatalf("approval bypassed hook snapshot: run=%+v err=%v", run, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("blocked approved tool ran: %v", err)
	}
	events := hookEvents(t, db, run.ID)
	if len(events) != 2 || events[1].Point != hooks.PreToolUse {
		t.Fatalf("lifecycle repeated on resume: %+v", events)
	}
	batches, _ := db.LoadBatches(t.Context(), run.ID)
	if batches[0].Invocations[0].Dispatched || batches[0].Invocations[0].Status != domain.InvDenied {
		t.Fatalf("hook denial recorded as dispatch: %+v", batches[0].Invocations[0])
	}
}

func TestUserReplyHookRejectsBeforeResuming(t *testing.T) {
	isolatedPolicyConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "ask", Name: domain.ToolAskUser, Arguments: json.RawMessage(`{"question":"answer?"}`)}), textOnly("done"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	putAgentHooks(t, workspace, map[hooks.Point][]hooks.Action{hooks.UserPromptSubmit: {{Type: "starlark", Script: `def main():
    text = broker.invoke("hook.input", {})["data"]["prompt"]
    if text == "reject this reply":
        return {"exit_code": 2, "stderr": "reply-rejected"}
    return {"exit_code": 0}
`}}}, false)
	run, err := runHookMessage(t, m)
	if err != nil || run.WaitReason != domain.WaitUser {
		t.Fatalf("ask user: %+v %v", run, err)
	}
	rejected, err := m.Resume(t.Context(), run.ID, ResumeInput{Answer: "reject this reply"})
	if err == nil || !strings.Contains(err.Error(), "reply-rejected") || rejected.Status != domain.RunWaiting || len(mock.Requests) != 1 {
		t.Fatalf("rejected reply entered model context: %+v %v", rejected, err)
	}
	current, _ := db.GetRun(t.Context(), run.ID)
	if current.Status != domain.RunWaiting || current.WaitReason != domain.WaitUser {
		t.Fatalf("rejected reply consumed wait: %+v", current)
	}
	resumed, err := m.Resume(t.Context(), run.ID, ResumeInput{Answer: "accepted reply"})
	if err != nil || resumed.Status != domain.RunCompleted {
		t.Fatalf("accepted reply: %+v %v", resumed, err)
	}
}

func TestInvalidHooksFailRunBeforeModel(t *testing.T) {
	isolatedPolicyConfig(t)
	mock := &provider.Mock{}
	m, _, workspace := testManager(t, mock)
	file := filepath.Join(workspace, ".cloud-harness", "hooks.json")
	putAgentPolicy(t, file, `{"hooks":{"PreToolUse":[{"type":"command"}]}}`)
	run, err := runHookMessage(t, m)
	if err == nil || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), "action 1") || run.Status != domain.RunFailed || len(mock.Requests) != 0 {
		t.Fatalf("invalid hooks accepted: %+v %v", run, err)
	}
}

func TestDefaultGoSelfCheckAppearsInToolResultAndCanBeDisabled(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		enabled       bool
	}{
		{"syntax", "package demo\nfunc Broken( {\n", true},
		{"vet", "package demo\n\nfunc Broken() { missing() }\n", true},
		{"format", "package demo\nfunc Good(){ }\n", true},
		{"disabled", "package demo\nfunc Broken( {\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedPolicyConfig(t)
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "write", Name: "write_file", Arguments: must(t, map[string]string{"path": "pkg/demo.go", "content": tc.content})}), textOnly("repair next"),
			}}
			m, db, workspace := testManager(t, mock)
			m.DisableSnapshots = true
			putAgentPolicy(t, filepath.Join(workspace, "go.mod"), "module demo\n\ngo 1.20\n")
			putAgentPolicy(t, filepath.Join(workspace, "pkg", "demo.go"), "package demo\n")
			// An unrelated invalid package must never be vetted.
			putAgentPolicy(t, filepath.Join(workspace, "unrelated", "bad.go"), "package unrelated\nfunc Wrong( {\n")
			putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), `{"rules":[{"tool":"write_file","effect":"allow"}]}`)
			if !tc.enabled {
				putAgentHooks(t, workspace, map[hooks.Point][]hooks.Action{}, false)
			}
			run, err := runHookMessage(t, m)
			if err != nil || run.Status != domain.RunCompleted {
				t.Fatalf("self-check run: %+v %v", run, err)
			}
			batches, _ := db.LoadBatches(t.Context(), run.ID)
			data := string(batches[0].Invocations[0].Result.Data)
			events := hookEvents(t, db, run.ID)
			if tc.enabled {
				if !strings.Contains(data, "hook_diagnostics") || !modelSawTool(mock, "self-check") || len(events) != 1 || events[0].Type != "go_check" || !events[0].Warning || strings.Contains(data, "unrelated") {
					t.Fatalf("missing/scoped diagnostics: data=%s events=%+v", data, events)
				}
				want := "gofmt -l"
				if tc.name == "vet" {
					want = "go vet"
				}
				if !strings.Contains(data, want) || tc.name == "vet" && !strings.Contains(data, "undefined: missing") {
					t.Fatalf("missing %s output: %s", want, data)
				}
			} else if strings.Contains(data, "hook_diagnostics") || len(events) != 0 {
				t.Fatalf("disabled check ran: data=%s events=%+v", data, events)
			}
		})
	}
}

func TestGoSelfCheckDetectsCommandAndFailedToolChanges(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "command", true: "failed tool"}[failed], func(t *testing.T) {
			isolatedPolicyConfig(t)
			name := "run_command"
			args := json.RawMessage(`{"command":"sh break.sh"}`)
			if failed {
				name, args = "failing_writer", json.RawMessage(`{}`)
			}
			mock := &provider.Mock{Responses: []domain.ModelResponse{toolResp(domain.ToolCall{ProviderCallID: "change", Name: name, Arguments: args}), textOnly("handled")}}
			m, _, workspace := testManager(t, mock)
			m.DisableSnapshots = true
			putAgentPolicy(t, filepath.Join(workspace, "go.mod"), "module demo\n\ngo 1.20\n")
			putAgentPolicy(t, filepath.Join(workspace, "demo.go"), "package demo\n")
			putAgentPolicy(t, filepath.Join(workspace, "break.sh"), "printf 'package demo\nfunc Bad( {\n' > demo.go\n")
			putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), `{"rules":[{"tool":"*","effect":"allow"}]}`)
			if failed {
				m.Broker.Register(tools.Spec{Name: name, SideEffectClass: "write", Execute: func(context.Context, json.RawMessage) (json.RawMessage, error) {
					if err := os.WriteFile(filepath.Join(workspace, "demo.go"), []byte("package demo\nfunc Bad( {\n"), 0o600); err != nil {
						return nil, err
					}
					return nil, domain.E(domain.CodeToolError, "executor failed after writing")
				}})
			}
			run, err := runHookMessage(t, m)
			if err != nil || run.Status != domain.RunCompleted || !modelSawTool(mock, "gofmt -l") || !modelSawTool(mock, "go vet") {
				t.Fatalf("non-file-tool changes not checked: run=%+v err=%v requests=%+v", run, err, mock.Requests)
			}
		})
	}
}

func TestHooksDoNotBypassBudget(t *testing.T) {
	isolatedPolicyConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"out.txt","content":"blocked"}`)}), textOnly("handled"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	m.UseBudget, m.InitialBudget = true, domain.DefaultBudget()
	m.InitialBudget.MaxToolCalls = 0
	putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), `{"rules":[{"tool":"write_file","effect":"allow"}]}`)
	putAgentHooks(t, workspace, map[hooks.Point][]hooks.Action{hooks.PreToolUse: {{Type: "command", Command: "true"}}}, false)
	run, err := runHookMessage(t, m)
	if err != nil || run.Status != domain.RunCompleted || run.Budget.UsedToolCalls != 0 || !modelSawTool(mock, "budget_exhausted") {
		t.Fatalf("budget bypass: %+v %v", run, err)
	}
	if len(hookEvents(t, db, run.ID)) != 1 {
		t.Fatal("pre hook did not run")
	}
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("tool escaped budget: %v", err)
	}
}

func TestNestedProgramAndPollCannotBypassHooksOrPolicy(t *testing.T) {
	for _, tc := range []struct{ name, tool, args string }{
		{"program", domain.ToolExecuteProgram, `{"script":"def main():\n  return broker.invoke(\"read_file\", {\"path\": \"notes.txt\"})\n","allow":["read_file"]}`},
		{"poll", domain.ToolPoll, `{"target_tool":"read_file","target_args":{"path":"notes.txt"},"until":{},"max_attempts":2}`},
	} {
		for _, policyDeny := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/hook deny", true: "/policy deny"}[policyDeny], func(t *testing.T) {
				isolatedPolicyConfig(t)
				mock := &provider.Mock{Responses: []domain.ModelResponse{toolResp(domain.ToolCall{ProviderCallID: "outer", Name: tc.tool, Arguments: json.RawMessage(tc.args)}), textOnly("handled")}}
				m, db, workspace := testManager(t, mock)
				m.DisableSnapshots = true
				putAgentPolicy(t, filepath.Join(workspace, "notes.txt"), "nested-private-bytes")
				if policyDeny {
					putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), `{"rules":[{"tool":"read_file","effect":"deny"}]}`)
				}
				putAgentHooks(t, workspace, map[hooks.Point][]hooks.Action{hooks.PreToolUse: {{Type: "starlark", Script: `def main():
    tool = broker.invoke("hook.input", {})["data"]["tool"]
    if tool == "read_file":
        return {"exit_code": 2, "stderr": "nested-hook-rejected"}
    return {"exit_code": 0}
`}}}, false)
				run, err := runHookMessage(t, m)
				want, count := "nested-hook-rejected", 2
				if policyDeny {
					want, count = "policy deny", 1
				}
				if err != nil || run.Status != domain.RunCompleted || !modelSawTool(mock, want) || modelSawTool(mock, "nested-private-bytes") || len(hookEvents(t, db, run.ID)) != count {
					t.Fatalf("nested protection: run=%+v err=%v hooks=%+v requests=%+v", run, err, hookEvents(t, db, run.ID), mock.Requests)
				}
			})
		}
	}
}

func TestPostHookRunsForEveryPollAttempt(t *testing.T) {
	isolatedPolicyConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "poll", Name: domain.ToolPoll, Arguments: json.RawMessage(`{"target_tool":"read_file","target_args":{"path":"notes.txt"},"until":{},"max_attempts":2}`)}), textOnly("done"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	putAgentPolicy(t, filepath.Join(workspace, "notes.txt"), "notes")
	putAgentHooks(t, workspace, map[hooks.Point][]hooks.Action{hooks.PostToolUse: {{Type: "command", Command: "echo returned"}}}, false)
	run, err := runHookMessage(t, m)
	if err != nil || run.Status != domain.RunCompleted || run.Budget.UsedToolCalls != 3 {
		t.Fatalf("poll: %+v %v", run, err)
	}
	events := hookEvents(t, db, run.ID)
	if len(events) != 3 || events[0].InvocationID == events[1].InvocationID || events[1].InvocationID == events[2].InvocationID {
		t.Fatalf("missing per-attempt post hook: %+v", events)
	}
}

func TestStopHookOnWaitingCancellationUsesFrozenConfig(t *testing.T) {
	isolatedPolicyConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "ask", Name: domain.ToolAskUser, Arguments: json.RawMessage(`{"question":"continue?"}`)}),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	putAgentHooks(t, workspace, map[hooks.Point][]hooks.Action{hooks.Stop: {{Type: "command", Command: "echo stopped > stop.txt"}}}, false)
	run, err := runHookMessage(t, m)
	if err != nil || run.WaitReason != domain.WaitUser || len(hookEvents(t, db, run.ID)) != 0 {
		t.Fatalf("Stop ran at pause: %+v %v", run, err)
	}
	putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "hooks.json"), "invalid JSON")
	fresh := &Manager{Store: db, Provider: mock, Workspace: workspace}
	cancelled, err := fresh.Cancel(t.Context(), run.ID)
	if err != nil || cancelled.Status != domain.RunCancelled {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	if output, err := os.ReadFile(filepath.Join(workspace, "stop.txt")); err != nil || string(output) != "stopped\n" {
		t.Fatalf("Stop did not run: %q %v", output, err)
	}
	if _, err := fresh.Cancel(t.Context(), run.ID); err != nil || len(hookEvents(t, db, run.ID)) != 1 {
		t.Fatalf("Stop repeated after cancellation: %v", err)
	}
}

func TestMissingHooksConfigPreservesExistingToolResult(t *testing.T) {
	isolatedPolicyConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"notes.txt"}`)}), textOnly("done"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	putAgentPolicy(t, filepath.Join(workspace, "notes.txt"), "existing-result")
	run, err := runHookMessage(t, m)
	if err != nil || run.Status != domain.RunCompleted || run.Budget.UsedToolCalls != 1 || len(hookEvents(t, db, run.ID)) != 0 || !modelSawTool(mock, "existing-result") || modelSawTool(mock, "hook_diagnostics") {
		t.Fatalf("missing config changed existing behavior: %+v %v", run, err)
	}
}
