package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/security"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func commandInvocation(t *testing.T, store storage.Store, runID string, batchIndex int) domain.Invocation {
	t.Helper()
	batches, err := store.LoadBatches(t.Context(), runID)
	if err != nil || len(batches) != batchIndex+1 || len(batches[batchIndex].Invocations) != 1 {
		t.Fatalf("batches=%+v err=%v", batches, err)
	}
	return batches[batchIndex].Invocations[0]
}

func TestFullAutoOutsideAndShellCommands(t *testing.T) {
	for _, name := range []string{"ls-root", "df", "du-outside", "glob", "pipeline", "redirect", "outside-cwd"} {
		t.Run(name, func(t *testing.T) {
			isolatedPolicyConfig(t)
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "sample.txt"), []byte("sample"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := map[string]string{"command": "ls -la /"}
			switch name {
			case "df":
				args["command"] = "df -h"
			case "du-outside":
				args["command"] = "du -sh " + outside
			case "glob":
				args["command"] = "du -sh " + outside + "/*"
			case "pipeline":
				args["command"] = "df -h / | tail -n 1"
			case "redirect":
				args["command"] = "ls / 2>/dev/null | head -n 3"
			case "outside-cwd":
				args["command"], args["cwd"] = "pwd", outside
			}
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "command", Name: "run_command", Arguments: must(t, args)}), textOnly("done"),
			}}
			m, db, _ := testManager(t, mock)
			m.SkipPlanner, m.DisableSnapshots = true, true
			session, err := m.CreateSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			run, err := m.RunMessageWithOptions(t.Context(), session.ID, "run command", RunOptions{ApprovalMode: "full-auto"})
			if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "done" {
				t.Fatalf("run=%+v err=%v", run, err)
			}
			inv := commandInvocation(t, db, run.ID, 0)
			if inv.Status != domain.InvSucceeded || !inv.Dispatched || inv.Result == nil || inv.Result.Error != nil {
				t.Fatalf("command did not execute: %+v", inv)
			}
			var output struct {
				Stdout   string `json:"stdout"`
				ExitCode int    `json:"exit_code"`
			}
			if err := json.Unmarshal(inv.Result.Data, &output); err != nil || strings.TrimSpace(output.Stdout) == "" || output.ExitCode != 0 {
				t.Fatalf("command output=%s err=%v", inv.Result.Data, err)
			}
			if name == "outside-cwd" && strings.TrimSpace(output.Stdout) != outside {
				t.Fatalf("cwd=%q want=%q", output.Stdout, outside)
			}
			approvals, err := db.ListApprovals(t.Context(), run.ID)
			if err != nil || len(approvals) != 0 {
				t.Fatalf("unexpected approvals=%+v err=%v", approvals, err)
			}
			grants, err := db.ListGrants(t.Context(), run.ID, session.ID, m.now())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, grant := range grants {
				if grant.ID == "grd_policy_"+inv.ID {
					found = grant.Subject == "policy" && grant.InvocationID == inv.ID && grant.RunID == run.ID && grant.SessionID == session.ID && grant.ResourceScope == args["command"]
				}
			}
			if !found {
				t.Fatalf("missing exact policy grant: %+v", grants)
			}
		})
	}
}

func TestOutsideCommandStillWaitsAndCanBeApproved(t *testing.T) {
	for _, mode := range []string{"ask", "safe", "workspace-write"} {
		t.Run(mode, func(t *testing.T) {
			isolatedPolicyConfig(t)
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "command", Name: "run_command", Arguments: json.RawMessage(`{"command":"ls -la /","approval_mode":"full-auto","ApprovalMode":"full-auto"}`)}), textOnly("done"),
			}}
			m, db, _ := testManager(t, mock)
			m.SkipPlanner, m.DisableSnapshots = true, true
			session, err := m.CreateSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			run, err := m.RunMessageWithOptions(t.Context(), session.ID, "run command", RunOptions{ApprovalMode: mode})
			if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval {
				t.Fatalf("initial run=%+v err=%v", run, err)
			}
			approvals, err := db.ListApprovals(t.Context(), run.ID)
			if err != nil || len(approvals) != 1 || approvals[0].State != "pending" {
				t.Fatalf("approvals=%+v err=%v", approvals, err)
			}
			run, err = m.Resume(t.Context(), run.ID, ResumeInput{Decision: "approve"})
			if err != nil || run.Status != domain.RunCompleted {
				t.Fatalf("approved run=%+v err=%v", run, err)
			}
			if inv := commandInvocation(t, db, run.ID, 0); inv.Status != domain.InvSucceeded {
				t.Fatalf("approved invocation=%+v", inv)
			}
		})
	}
}

func TestFullAutoShellCommandDenyStages(t *testing.T) {
	commands := []string{"touch blocked", "true;touch blocked", "true && touch blocked", "false || touch blocked", "echo x | touch blocked", "true & touch blocked", "true\ntouch blocked", "echo $(touch blocked)", "echo `touch blocked`"}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			global := isolatedPolicyConfig(t)
			putAgentPolicy(t, global, `{"rules":[{"tool":"run_command","command_prefix":"touch","effect":"deny"}]}`)
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "command", Name: "run_command", Arguments: must(t, map[string]string{"command": command})}), textOnly("denied"),
			}}
			m, db, root := testManager(t, mock)
			m.SkipPlanner, m.DisableSnapshots = true, true
			session, err := m.CreateSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			run, err := m.RunMessageWithOptions(t.Context(), session.ID, "attempt command", RunOptions{ApprovalMode: "full-auto"})
			if err != nil || run.Status != domain.RunCompleted || run.Budget.UsedToolCalls != 0 {
				t.Fatalf("run=%+v err=%v", run, err)
			}
			inv := commandInvocation(t, db, run.ID, 0)
			if inv.Status != domain.InvDenied || inv.Dispatched || inv.Result == nil || inv.Result.Error == nil || inv.Result.Error.Code != domain.CodeAuthDenied || !strings.Contains(inv.Result.Error.Message, "policy deny") {
				t.Fatalf("denial=%+v", inv)
			}
			if _, err := os.Stat(filepath.Join(root, "blocked")); !os.IsNotExist(err) {
				t.Fatalf("denied command created marker: %v", err)
			}
			approvals, err := db.ListApprovals(t.Context(), run.ID)
			if err != nil || len(approvals) != 0 {
				t.Fatalf("approvals=%+v err=%v", approvals, err)
			}
			var decision security.PolicyDecision
			events := policyEvents(t, db, run.ID)
			if len(events) != 1 || json.Unmarshal(events[0].Payload, &decision) != nil || decision.Effect != security.EffectDeny {
				t.Fatalf("events=%+v", events)
			}
		})
	}
}

func TestApprovalResourceFailureReturnsToolDenialAndContinues(t *testing.T) {
	for _, mode := range []string{"ask", "safe", "workspace-write", "full-auto"} {
		t.Run(mode, func(t *testing.T) {
			isolatedPolicyConfig(t)
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "control", Name: "run_command", Arguments: json.RawMessage(`{"command":"ls .cloud-harness"}`)}), textOnly("replanned after denial"),
			}}
			m, db, root := testManager(t, mock)
			m.SkipPlanner, m.DisableSnapshots = true, true
			if err := os.MkdirAll(filepath.Join(root, ".cloud-harness"), 0o700); err != nil {
				t.Fatal(err)
			}
			session, err := m.CreateSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			run, err := m.RunMessageWithOptions(t.Context(), session.ID, "attempt control path", RunOptions{ApprovalMode: mode})
			if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval {
				t.Fatalf("initial run=%+v err=%v", run, err)
			}
			fresh := &Manager{Store: db, Provider: mock, Broker: m.Broker, Workspace: root, OwnerID: "approval-resource-failure", DisableSnapshots: true}
			run, err = fresh.Resume(t.Context(), run.ID, ResumeInput{Decision: "approve"})
			if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "replanned after denial" || run.Budget.UsedToolCalls != 0 {
				t.Fatalf("resumed run=%+v err=%v", run, err)
			}
			inv := commandInvocation(t, db, run.ID, 0)
			if inv.Status != domain.InvDenied || inv.Dispatched || inv.Result == nil || inv.Result.Error == nil || inv.Result.Error.Code != domain.CodeAuthDenied || !strings.Contains(inv.Result.Error.Message, "harness control files") {
				t.Fatalf("invalid-resource invocation=%+v", inv)
			}
			approvals, err := db.ListApprovals(t.Context(), run.ID)
			if err != nil || len(approvals) != 1 || approvals[0].State != "denied" {
				t.Fatalf("invalid-resource approval did not terminate: %+v err=%v", approvals, err)
			}
			grants, err := db.ListGrants(t.Context(), run.ID, session.ID, m.now())
			if err != nil {
				t.Fatal(err)
			}
			for _, grant := range grants {
				if grant.InvocationID == inv.ID {
					t.Fatalf("unresolved scope granted authority: %+v", grant)
				}
			}
			visible := false
			for _, message := range mock.Requests[len(mock.Requests)-1].Messages {
				visible = visible || message.Role == "tool" && strings.Contains(message.Content, "auth_denied") && strings.Contains(message.Content, "harness control files")
			}
			if !visible {
				t.Fatal("model did not receive the tool-level resource denial")
			}
		})
	}
}

func TestFullAutoShellModeIsFrozenAcrossResume(t *testing.T) {
	for _, mode := range []string{"ask", "safe", "workspace-write", "full-auto"} {
		t.Run(mode, func(t *testing.T) {
			isolatedPolicyConfig(t)
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "question", Name: domain.ToolAskUser, Arguments: json.RawMessage(`{"question":"continue?"}`)}),
				toolResp(domain.ToolCall{ProviderCallID: "command", Name: "run_command", Arguments: json.RawMessage(`{"command":"printf frozen-mode | cat","approval_mode":"full-auto"}`)}), textOnly("done"),
			}}
			m, db, root := testManager(t, mock)
			m.SkipPlanner, m.DisableSnapshots = true, true
			session, err := m.CreateSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			run, err := m.RunMessageWithOptions(t.Context(), session.ID, "work", RunOptions{ApprovalMode: mode})
			if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitUser {
				t.Fatalf("initial run=%+v err=%v", run, err)
			}
			nextMode := "full-auto"
			if mode == "full-auto" {
				nextMode = "ask"
			}
			if err := db.SaveSessionPreferences(t.Context(), storage.SessionPreferences{SessionID: session.ID, ModelID: "default", ApprovalMode: nextMode}); err != nil {
				t.Fatal(err)
			}
			fresh := &Manager{Store: db, Provider: mock, Broker: m.Broker, Workspace: root, OwnerID: "frozen-shell-mode", DisableSnapshots: true}
			run, err = fresh.Resume(t.Context(), run.ID, ResumeInput{Answer: "yes"})
			if mode != "full-auto" {
				if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval {
					t.Fatalf("shell auto-approved in %s: run=%+v err=%v", mode, run, err)
				}
				run, err = fresh.Resume(t.Context(), run.ID, ResumeInput{Decision: "approve"})
			}
			if err != nil || run.Status != domain.RunCompleted {
				t.Fatalf("resumed run=%+v err=%v", run, err)
			}
			inv := commandInvocation(t, db, run.ID, 1)
			if mode == "full-auto" {
				if inv.Status != domain.InvSucceeded || inv.Result == nil || !strings.Contains(string(inv.Result.Data), "frozen-mode") {
					t.Fatalf("snapshot shell execution failed: %+v", inv)
				}
			} else if inv.Status != domain.InvFailed || inv.Result == nil || inv.Result.Error == nil || inv.Result.Error.Code != domain.CodeAuthDenied {
				t.Fatalf("shell syntax executed in %s: %+v", mode, inv)
			}
		})
	}
}
