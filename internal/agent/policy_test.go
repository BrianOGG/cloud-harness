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
	"github.com/cloud-harness/cloud-harness/internal/security"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

// putAgentPolicy writes a configuration file as the user would. A workspace
// hooks.json or policy.json is also trusted, as `cloud-harness trust approve`
// does; putUntrusted writes one without that decision.
func putAgentPolicy(t *testing.T, file, body string) {
	t.Helper()
	putUntrusted(t, file, body)
	name := filepath.Base(file)
	if filepath.Base(filepath.Dir(file)) != security.ControlDir || name != security.TrustHooksFile && name != security.TrustPolicyFile {
		return
	}
	if os.Getenv("CLOUD_HARNESS_CONFIG") == "" {
		t.Fatal("isolatedPolicyConfig must precede a trusted workspace control file")
	}
	dir, err := security.ConfigDirectory()
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Dir(filepath.Dir(file))
	if err := security.TrustContent(workspace, dir, map[string][]byte{name: []byte(body)}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func putUntrusted(t *testing.T, file, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func isolatedPolicyConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(dir, "config.json"))
	return filepath.Join(dir, "policy.json")
}

func policyEvents(t *testing.T, store storage.Store, runID string) []storage.Event {
	t.Helper()
	events, err := store.ListEvents(context.Background(), runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []storage.Event
	for _, event := range events {
		if event.Type == "policy.decided" {
			out = append(out, event)
		}
	}
	return out
}

func TestPolicyToolEffects(t *testing.T) {
	for _, tc := range []struct {
		name, workspacePolicy, globalPolicy, tool, path string
		effect                                          security.Effect
		source                                          string
		status                                          domain.InvStatus
	}{
		{"workspace allow overrides global deny", `{"rules":[{"tool":"write_file","path_glob":"out.txt","effect":"allow"}]}`, `{"rules":[{"tool":"*","effect":"deny"}]}`, "write_file", "out.txt", security.EffectAllow, "workspace", domain.InvSucceeded},
		{"global allow", "", `{"rules":[{"tool":"write_file","effect":"allow"}]}`, "write_file", "out.txt", security.EffectAllow, "global", domain.InvSucceeded},
		{"global deny beats grant", "", `{"rules":[{"tool":"read_file","effect":"deny"}]}`, "read_file", "notes.txt", security.EffectDeny, "global", domain.InvDenied},
		{"workspace allows env baseline", `{"rules":[{"tool":"read_file","path_glob":".env","effect":"allow"}]}`, "", "read_file", ".env", security.EffectAllow, "workspace", domain.InvSucceeded},
		{"env baseline", "", "", "read_file", ".env", security.EffectDeny, "builtin", domain.InvDenied},
		{"env variant baseline", "", "", "read_file", ".env.local", security.EffectDeny, "builtin", domain.InvDenied},
		{"env example readable", "", "", "read_file", ".env.example", security.EffectAsk, "default", domain.InvSucceeded},
		{"outside write baseline", "", "", "write_file", "../outside.txt", security.EffectDeny, "builtin", domain.InvDenied},
		{"inside write asks without policy", "", "", "write_file", "out.txt", security.EffectAsk, "default", domain.InvWaitingApproval},
		{"explicit ask", `{"rules":[{"tool":"write_file","effect":"ask"}]}`, `{"rules":[{"tool":"*","effect":"allow"}]}`, "write_file", "out.txt", security.EffectAsk, "workspace", domain.InvWaitingApproval},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			global := isolatedPolicyConfig(t)
			args := map[string]string{"path": tc.path}
			if tc.tool == "write_file" {
				args["content"] = "private-write-content"
			}
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "call", Name: tc.tool, Arguments: must(t, args)}),
				textOnly("handled"),
			}}
			m, db, workspace := testManager(t, mock)
			m.DisableSnapshots = true
			if tc.tool == "read_file" {
				putAgentPolicy(t, filepath.Join(workspace, tc.path), "private-read-content")
			}
			if tc.workspacePolicy != "" {
				putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), tc.workspacePolicy)
			}
			if tc.globalPolicy != "" {
				putAgentPolicy(t, global, tc.globalPolicy)
			}
			session, err := m.CreateSession(ctx)
			if err != nil {
				t.Fatal(err)
			}
			run, err := m.RunMessage(ctx, session.ID, "perform one operation")
			if err != nil {
				t.Fatal(err)
			}
			batches, err := db.LoadBatches(ctx, run.ID)
			if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 1 {
				t.Fatalf("batches = %+v, %v", batches, err)
			}
			inv := batches[0].Invocations[0]
			if inv.Status != tc.status || inv.Dispatched != (tc.status == domain.InvSucceeded) {
				t.Fatalf("invocation = %+v, want %s", inv, tc.status)
			}
			approvals, err := db.ListApprovals(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.status == domain.InvWaitingApproval {
				if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval || len(approvals) != 1 || approvals[0].State != "pending" || len(mock.Requests) != 1 {
					t.Fatalf("existing approval path not used: run=%+v approvals=%+v", run, approvals)
				}
			} else if run.Status != domain.RunCompleted || len(approvals) != 0 || len(mock.Requests) != 2 {
				t.Fatalf("unexpected wait or incomplete run: run=%+v approvals=%+v", run, approvals)
			}
			if tc.status == domain.InvDenied {
				if inv.Result == nil || inv.Result.Error == nil || !strings.Contains(inv.Result.Error.Message, tc.source) || !strings.Contains(inv.Result.Error.Message, "policy deny") {
					t.Fatalf("denial lacks policy reason: %+v", inv.Result)
				}
				visible := false
				for _, msg := range mock.Requests[1].Messages {
					if msg.Role == "tool" && strings.Contains(msg.Content, tc.source) && strings.Contains(msg.Content, "policy deny") {
						visible = true
					}
				}
				if !visible || strings.Contains(messageText(mock.Requests[1].Messages), "private-read-content") {
					t.Fatal("model did not see a safe, actionable policy error")
				}
			}
			if tc.tool == "write_file" {
				body, err := os.ReadFile(filepath.Join(workspace, tc.path))
				if tc.status == domain.InvSucceeded {
					if err != nil || string(body) != "private-write-content" {
						t.Fatalf("allowed write did not execute: %q, %v", body, err)
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("unapproved write executed: %q, %v", body, err)
				}
			}
			events := policyEvents(t, db, run.ID)
			if len(events) != 1 || events[0].InvocationID != inv.ID {
				t.Fatalf("policy decision must be recorded exactly once: %+v", events)
			}
			var decision security.PolicyDecision
			if err := json.Unmarshal(events[0].Payload, &decision); err != nil {
				t.Fatal(err)
			}
			if decision.Effect != tc.effect || decision.Source != tc.source || decision.Tool != tc.tool || decision.Rule == "" {
				t.Fatalf("policy event = %+v", decision)
			}
			if strings.Contains(string(events[0].Payload), "private-") {
				t.Fatalf("file content leaked into event: %s", events[0].Payload)
			}
		})
	}
}

func TestPolicyCommandAllowAndRedaction(t *testing.T) {
	isolatedPolicyConfig(t)
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "command", Name: "run_command", Arguments: json.RawMessage(`{"command":"echo private-command-argument"}`)}),
		textOnly("done"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), `{"rules":[{"tool":"run_command","command_prefix":"echo","effect":"allow"}]}`)
	session, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, session.ID, "execute command")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run = %+v, %v", run, err)
	}
	batches, err := db.LoadBatches(ctx, run.ID)
	if err != nil || len(batches) != 1 {
		t.Fatalf("batches = %+v, %v", batches, err)
	}
	inv := batches[0].Invocations[0]
	if inv.Status != domain.InvSucceeded || !strings.Contains(string(inv.Result.Data), "private-command-argument") {
		t.Fatalf("allowed command did not execute: %+v", inv)
	}
	approvals, _ := db.ListApprovals(ctx, run.ID)
	events := policyEvents(t, db, run.ID)
	if len(approvals) != 0 || len(events) != 1 || strings.Contains(string(events[0].Payload), "private-command-argument") || strings.Contains(string(events[0].Payload), "echo") || !strings.Contains(string(events[0].Payload), "command_sha256") {
		t.Fatalf("approval or redaction failure: approvals=%+v events=%+v", approvals, events)
	}
	grants, err := db.ListGrants(ctx, run.ID, session.ID, m.now())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, grant := range grants {
		if grant.Subject == "policy" {
			found = true
			if grant.InvocationID != inv.ID || grant.ResourceScope != "echo private-command-argument" || grant.RunID != run.ID || grant.SessionID != session.ID {
				t.Fatalf("policy authority is not invocation-scoped: %+v", grant)
			}
		}
	}
	if !found {
		t.Fatal("command did not use scoped broker authority")
	}
}

func TestPolicyCommandDenyReasonIsVisibleAndRedacted(t *testing.T) {
	isolatedPolicyConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "command", Name: "run_command", Arguments: json.RawMessage(`{"command":"echo private-command-argument"}`)}), textOnly("denied"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), `{"rules":[{"tool":"run_command","command_prefix":"echo","effect":"deny"}]}`)
	session, _ := m.CreateSession(context.Background())
	run, err := m.RunMessage(context.Background(), session.ID, "attempt command")
	if err != nil || run.Status != domain.RunCompleted || run.Budget.UsedToolCalls != 0 {
		t.Fatalf("command denial = %+v, %v", run, err)
	}
	batches, _ := db.LoadBatches(context.Background(), run.ID)
	inv := batches[0].Invocations[0]
	if inv.Status != domain.InvDenied || inv.Dispatched || inv.Result == nil || inv.Result.Error == nil {
		t.Fatalf("denied command executed or asked for approval: %+v", inv)
	}
	message := inv.Result.Error.Message
	if !strings.Contains(message, "workspace") || !strings.Contains(message, "command_prefix_sha256=") || strings.Contains(message, "echo") || strings.Contains(message, "private-command-argument") {
		t.Fatalf("unsafe command denial: %s", message)
	}
	visible := false
	for _, msg := range mock.Requests[1].Messages {
		visible = visible || msg.Role == "tool" && strings.Contains(msg.Content, message)
	}
	if !visible {
		t.Fatal("command denial was not sent to model")
	}
	if events := policyEvents(t, db, run.ID); len(events) != 1 || strings.Contains(string(events[0].Payload), "private-command-argument") {
		t.Fatalf("command policy events = %+v", events)
	}
}

func TestPolicySnapshotSurvivesApprovalResume(t *testing.T) {
	global := isolatedPolicyConfig(t)
	ctx := context.Background()
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(
			domain.ToolCall{ProviderCallID: "ask", Name: "write_file", Arguments: json.RawMessage(`{"path":"ask.txt","content":"approved"}`)},
			domain.ToolCall{ProviderCallID: "allow", Name: "write_file", Arguments: json.RawMessage(`{"path":"allow.txt","content":"automatic"}`)},
		), textOnly("done"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	file := filepath.Join(workspace, ".cloud-harness", "policy.json")
	putAgentPolicy(t, file, `{"rules":[{"tool":"write_file","path_glob":"allow.txt","effect":"allow"}]}`)
	session, _ := m.CreateSession(ctx)
	run, err := m.RunMessage(ctx, session.ID, "write two files")
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval {
		t.Fatalf("expected initial approval: %+v, %v", run, err)
	}
	// Both edited layers must be irrelevant to this run, even in a fresh manager.
	putAgentPolicy(t, file, `{"rules":[{"tool":"*","effect":"deny"}]}`)
	putAgentPolicy(t, global, `invalid JSON`)
	fresh := &Manager{Store: db, Provider: mock, Broker: tools.NewBroker(db, db), Workspace: workspace, DisableSnapshots: true, OwnerID: "resume-policy"}
	resumed, err := fresh.Resume(ctx, run.ID, ResumeInput{Decision: "approve"})
	if err != nil || resumed.Status != domain.RunCompleted {
		t.Fatalf("policy changed during resume: %+v, %v", resumed, err)
	}
	for name, want := range map[string]string{"ask.txt": "approved", "allow.txt": "automatic"} {
		if body, err := os.ReadFile(filepath.Join(workspace, name)); err != nil || string(body) != want {
			t.Fatalf("%s = %q, %v", name, body, err)
		}
	}
	approvals, _ := db.ListApprovals(ctx, run.ID)
	if len(approvals) != 1 || approvals[0].State != "approved" {
		t.Fatalf("allow requested another approval: %+v", approvals)
	}
	events := policyEvents(t, db, run.ID)
	if len(events) != 2 || events[0].InvocationID == events[1].InvocationID {
		t.Fatalf("decisions duplicated across resume: %+v", events)
	}
}

func TestPolicyInvalidAtRunStartup(t *testing.T) {
	for _, tc := range []struct{ name, body, layer string }{
		{"invalid JSON", `{"rules":[`, "workspace"},
		{"invalid effect", `{"rules":[{"tool":"*","effect":"permit"}]}`, "global"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global := isolatedPolicyConfig(t)
			mock := &provider.Mock{}
			m, _, workspace := testManager(t, mock)
			file := filepath.Join(workspace, ".cloud-harness", "policy.json")
			if tc.layer == "global" {
				file = global
			}
			putAgentPolicy(t, file, tc.body)
			session, _ := m.CreateSession(context.Background())
			run, err := m.RunMessage(context.Background(), session.ID, "must not call model")
			if err == nil || !strings.Contains(err.Error(), file) || run.Status != domain.RunFailed || len(mock.Requests) != 0 {
				t.Fatalf("invalid policy was not rejected at startup: run=%+v err=%v model calls=%d", run, err, len(mock.Requests))
			}
		})
	}
}

func TestPolicyAllowRespectsBudgetAndWorkspaceConstraint(t *testing.T) {
	for _, budget := range []bool{true, false} {
		t.Run(map[bool]string{true: "budget", false: "outside workspace"}[budget], func(t *testing.T) {
			isolatedPolicyConfig(t)
			args := `{"path":"out.txt","content":"blocked"}`
			if !budget {
				args = `{"path":"../outside.txt","content":"blocked"}`
			}
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "write", Name: "write_file", Arguments: json.RawMessage(args)}), textOnly("blocked"),
			}}
			m, db, workspace := testManager(t, mock)
			m.DisableSnapshots = true
			putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), `{"rules":[{"tool":"*","effect":"allow"}]}`)
			if budget {
				m.UseBudget, m.InitialBudget = true, domain.DefaultBudget()
				m.InitialBudget.MaxToolCalls = 0
			}
			session, _ := m.CreateSession(context.Background())
			run, err := m.RunMessage(context.Background(), session.ID, "attempt write")
			if err != nil || run.Status != domain.RunCompleted || run.Budget.UsedToolCalls != 0 {
				t.Fatalf("run = %+v, %v", run, err)
			}
			batches, _ := db.LoadBatches(context.Background(), run.ID)
			inv := batches[0].Invocations[0]
			want := domain.CodeAuthDenied
			if budget {
				want = domain.CodeBudget
			}
			if inv.Result == nil || inv.Result.Error == nil || inv.Result.Error.Code != want {
				t.Fatalf("hard constraint bypassed: %+v", inv)
			}
			if _, err := os.Stat(filepath.Join(workspace, jsonString([]byte(args), "path"))); !os.IsNotExist(err) {
				t.Fatalf("write escaped constraint: %v", err)
			}
		})
	}
}

func TestPolicyDenyAppliesToNestedPollAndProgram(t *testing.T) {
	for _, tc := range []struct{ name, tool, args string }{
		{"poll", domain.ToolPoll, `{"target_tool":"read_file","target_args":{"path":".env"},"until":{},"max_attempts":1}`},
		{"program", domain.ToolExecuteProgram, `{"script":"def main():\n  return broker.invoke(\"read_file\", {\"path\": \".env\"})\n","allow":["read_file"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedPolicyConfig(t)
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "outer", Name: tc.tool, Arguments: json.RawMessage(tc.args)}), textOnly("denied"),
			}}
			m, db, workspace := testManager(t, mock)
			m.DisableSnapshots = true
			putAgentPolicy(t, filepath.Join(workspace, ".env"), "nested-private-content")
			session, _ := m.CreateSession(context.Background())
			run, err := m.RunMessage(context.Background(), session.ID, "try nested read")
			if err != nil || run.Status != domain.RunCompleted || len(mock.Requests) != 2 {
				t.Fatalf("nested run = %+v, %v", run, err)
			}
			visible := messageText(mock.Requests[1].Messages)
			if !strings.Contains(visible, "builtin") || !strings.Contains(visible, "policy deny") || strings.Contains(visible, "nested-private-content") {
				t.Fatalf("nested policy was bypassed: %s", visible)
			}
			if events := policyEvents(t, db, run.ID); len(events) != 2 {
				t.Fatalf("outer and nested decisions = %+v", events)
			}
		})
	}
}

func TestPolicyAllowCannotWidenDelegateEmptyTools(t *testing.T) {
	isolatedPolicyConfig(t)
	mock := mockAgentDelegate(`{"goal":"inspect","agent":"empty"}`,
		toolResp(domain.ToolCall{ProviderCallID: "forged-read", Name: "read_file", Arguments: json.RawMessage(`{"path":"notes.txt"}`)}), textOnly("child done"),
	)
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	writeDelegateDefinition(t, workspace, "empty", "tools: []\n", "")
	putAgentPolicy(t, filepath.Join(workspace, "notes.txt"), "limited-private-content")
	putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), `{"rules":[{"tool":"*","effect":"allow"}]}`)
	parent := runAgentDelegate(t, m)
	child := delegateChildRun(t, db, parent)
	grants, err := db.ListGrants(context.Background(), child.ID, child.SessionID, m.now())
	if err != nil || len(grants) != 0 {
		t.Fatalf("policy widened empty child grants: %+v, %v", grants, err)
	}
	batches, err := db.LoadBatches(context.Background(), child.ID)
	if err != nil || len(batches) != 1 || batches[0].Invocations[0].Status != domain.InvDenied || batches[0].Invocations[0].Dispatched || child.Budget.UsedToolCalls != 0 {
		t.Fatalf("empty child executed a forged tool: %+v, %v", batches, err)
	}
}

func TestPolicyDefaultAskKeepsSensitiveCommandApprovalOrder(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "default ask", true: "allow retains hard constraint"}[allow], func(t *testing.T) {
			isolatedPolicyConfig(t)
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(
					domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"secrets.txt"}`)},
					domain.ToolCall{ProviderCallID: "command", Name: "run_command", Arguments: json.RawMessage(`{"command":"echo blocked-egress"}`)},
				), textOnly("denied"),
			}}
			m, db, workspace := testManager(t, mock)
			m.DisableSnapshots = true
			putAgentPolicy(t, filepath.Join(workspace, "secrets.txt"), "sensitive-input")
			if allow {
				putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), `{"rules":[{"tool":"run_command","effect":"allow"}]}`)
			}
			session, _ := m.CreateSession(context.Background())
			run, err := m.RunMessage(context.Background(), session.ID, "read and attempt command")
			if err != nil {
				t.Fatal(err)
			}
			batches, _ := db.LoadBatches(context.Background(), run.ID)
			command := batches[0].Invocations[1]
			if allow {
				if run.Status != domain.RunCompleted || command.Status != domain.InvDenied || command.Dispatched {
					t.Fatalf("policy bypassed sensitive egress constraint: run=%+v command=%+v", run, command)
				}
			} else if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval || command.Status != domain.InvWaitingApproval || command.Dispatched {
				t.Fatalf("default policy changed legacy approval ordering: run=%+v command=%+v", run, command)
			}
		})
	}
}

func TestPolicyAllowPreservesUnknownToolRejection(t *testing.T) {
	isolatedPolicyConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "unknown", Name: "missing_tool", Arguments: json.RawMessage(`{}`)}), textOnly("replanned"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	putAgentPolicy(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), `{"rules":[{"tool":"*","effect":"allow"}]}`)
	session, _ := m.CreateSession(context.Background())
	run, err := m.RunMessage(context.Background(), session.ID, "try a tool")
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "replanned" || run.Budget.UsedToolCalls != 0 || len(mock.Requests) != 2 {
		t.Fatalf("wildcard allow changed unknown-tool rejection into a fatal run: %+v, %v", run, err)
	}
	batches, err := db.LoadBatches(context.Background(), run.ID)
	if err != nil || len(batches) != 1 || batches[0].Invocations[0].Status != domain.InvDenied || batches[0].Invocations[0].Dispatched {
		t.Fatalf("unknown tool was not rejected before dispatch: %+v, %v", batches, err)
	}
}
