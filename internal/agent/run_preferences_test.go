package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/security"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestRunSelectionAndModeSurviveResume(t *testing.T) {
	isolatedPolicyConfig(t)
	m, db, root := testManager(t, &provider.Mock{})
	m.SkipPlanner = true
	m.DisableSnapshots = true
	m.ModelRouting = &config.ModelRouting{}
	calls := 0
	selected := providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		calls++
		if req.ReasoningEffort != "high" {
			t.Error("reasoning effort changed across resume")
		}
		if req.Model != "selected-model" {
			t.Errorf("model=%q, want snapshot selection", req.Model)
		}
		if calls == 1 {
			return toolResp(domain.ToolCall{ProviderCallID: "ask", Name: domain.ToolAskUser, Arguments: json.RawMessage("{\"question\":\"continue?\"}")}), nil
		}
		if calls == 2 {
			return toolResp(domain.ToolCall{ProviderCallID: "write", Name: "write_file", Arguments: json.RawMessage("{\"path\":\"result.txt\",\"content\":\"ok\"}")}), nil
		}
		return textOnly("selected answer"), nil
	})
	m.Provider = providerFunc(func(context.Context, provider.Request) (domain.ModelResponse, error) {
		t.Error("default provider used")
		return textOnly("wrong"), nil
	})
	resolved := 0
	m.ProviderResolver = func(id string) (provider.Provider, error) {
		resolved++
		if id != "example" {
			t.Errorf("provider id %q", id)
		}
		return selected, nil
	}
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessageWithOptions(t.Context(), session.ID, "work", RunOptions{ModelID: "custom:example", Model: "selected-model", ProviderID: "example", ApprovalMode: "workspace-write", ReasoningEffort: "high"})
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitUser {
		t.Fatalf("initial: %+v %v", run, err)
	}
	snap, err := m.loadCheckpoint(t.Context(), run.ID)
	if err != nil || snap.Model != "selected-model" || snap.ProviderID != "example" || snap.ApprovalMode != "workspace-write" || snap.ReasoningEffort != "high" {
		t.Fatalf("snapshot selection missing: %v", err)
	}
	if err := db.SaveSessionPreferences(t.Context(), storage.SessionPreferences{SessionID: session.ID, ModelID: "default", ApprovalMode: "ask", ReasoningEffort: "off"}); err != nil {
		t.Fatal(err)
	}
	// A fresh Manager simulates restart. Neither the new session preferences nor
	// a changed default/routing table may replace this run's frozen selection.
	resumed := &Manager{Store: db, Provider: m.Provider, ProviderResolver: m.ProviderResolver, Broker: m.Broker, Workspace: root, OwnerID: m.OwnerID, DisableSnapshots: true, Model: "changed-default", ModelRouting: &config.ModelRouting{}}
	run, err = resumed.Resume(t.Context(), run.ID, ResumeInput{Answer: "yes"})
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "selected answer" || resolved != 2 {
		t.Fatalf("resume status=%s calls=%d resolutions=%d err=%v", run.Status, calls, resolved, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "result.txt"))
	if err != nil || string(data) != "ok" {
		t.Fatal("snapshot workspace-write mode was not restored")
	}
}

func TestApprovalModeIntegrationWriteAndCommandBoundaries(t *testing.T) {
	for _, tc := range []struct {
		mode, tool, args string
		want             domain.RunStatus
	}{
		{"ask", "write_file", "{\"path\":\"out.txt\",\"content\":\"ok\"}", domain.RunWaiting},
		{"safe", "write_file", "{\"path\":\"out.txt\",\"content\":\"ok\"}", domain.RunWaiting},
		{"workspace-write", "write_file", "{\"path\":\"out.txt\",\"content\":\"ok\"}", domain.RunCompleted},
		{"workspace-write", "edit_file", "{\"path\":\"out.txt\",\"old_string\":\"before\",\"new_string\":\"after\"}", domain.RunCompleted},
		{"workspace-write", "run_command", "{\"command\":\"echo never-auto-approve\"}", domain.RunWaiting},
		{"ask", "run_command", `{"command":"echo never-auto-approve"}`, domain.RunWaiting},
		{"safe", "run_command", `{"command":"echo never-auto-approve"}`, domain.RunWaiting},
		{"full-auto", "run_command", `{"command":"uname -a"}`, domain.RunCompleted},
		{"full-auto", "run_command", `{"command":"cat /proc/cpuinfo"}`, domain.RunCompleted},
		{"full-auto", "run_command", `{"command":"cat /proc/meminfo"}`, domain.RunCompleted},
		{"full-auto", "run_command", `{"command":"echo never-auto-approve"}`, domain.RunCompleted},
		{"full-auto", "write_file", `{"path":"out.txt","content":"ok"}`, domain.RunCompleted},
		{"full-auto", "edit_file", `{"path":"out.txt","old_string":"before","new_string":"after"}`, domain.RunCompleted},
	} {
		t.Run(tc.mode+"/"+tc.tool, func(t *testing.T) {
			isolatedPolicyConfig(t)
			m, db, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{toolResp(domain.ToolCall{ProviderCallID: "tool", Name: tc.tool, Arguments: json.RawMessage(tc.args)}), textOnly("done")}})
			m.SkipPlanner = true
			m.DisableSnapshots = true
			if err := os.WriteFile(filepath.Join(root, "out.txt"), []byte("before"), 0o600); err != nil {
				t.Fatal(err)
			}
			session, err := m.CreateSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			run, err := m.RunMessageWithOptions(t.Context(), session.ID, "perform tool", RunOptions{ApprovalMode: tc.mode})
			if err != nil || run.Status != tc.want {
				t.Fatalf("status=%s want=%s err=%v", run.Status, tc.want, err)
			}
			if tc.mode == "full-auto" {
				batches, err := db.LoadBatches(t.Context(), run.ID)
				if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 1 {
					t.Fatalf("batches=%+v err=%v", batches, err)
				}
				inv := batches[0].Invocations[0]
				if inv.Status != domain.InvSucceeded || !inv.Dispatched || inv.Result == nil || inv.Result.Error != nil {
					t.Fatalf("full-auto did not execute successfully: %+v", inv)
				}
				approvals, err := db.ListApprovals(t.Context(), run.ID)
				if err != nil || len(approvals) != 0 {
					t.Fatalf("full-auto requested approval: %+v err=%v", approvals, err)
				}
				events := policyEvents(t, db, run.ID)
				var decision security.PolicyDecision
				if len(events) != 1 || json.Unmarshal(events[0].Payload, &decision) != nil || decision.Effect != security.EffectAllow || decision.Source != "session_mode" || decision.Rule != "full-auto session mode" {
					t.Fatalf("full-auto audit decision missing: %+v", events)
				}
				if tc.tool == "run_command" && decision.CommandSHA256 == "" {
					t.Fatal("command audit selector missing")
				}
			}
			if (tc.mode == "workspace-write" || tc.mode == "full-auto") && tc.tool != "run_command" {
				content, err := os.ReadFile(filepath.Join(root, "out.txt"))
				want := "ok"
				if tc.tool == "edit_file" {
					want = "after"
				}
				if err != nil || string(content) != want {
					t.Fatal("workspace write/edit did not execute successfully")
				}
			}
		})
	}
}

func TestFullAutoModeIsFrozenAcrossResume(t *testing.T) {
	for _, mode := range []string{"ask", "full-auto"} {
		t.Run(mode, func(t *testing.T) {
			isolatedPolicyConfig(t)
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "question", Name: domain.ToolAskUser, Arguments: json.RawMessage(`{"question":"continue?"}`)}),
				toolResp(domain.ToolCall{ProviderCallID: "command", Name: "run_command", Arguments: json.RawMessage(`{"command":"echo snapshot-mode"}`)}),
				textOnly("done"),
			}}
			m, db, workspace := testManager(t, mock)
			m.SkipPlanner, m.DisableSnapshots = true, true
			session, err := m.CreateSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			run, err := m.RunMessageWithOptions(t.Context(), session.ID, "work", RunOptions{ApprovalMode: mode})
			if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitUser {
				t.Fatalf("initial status=%s reason=%s err=%v", run.Status, run.WaitReason, err)
			}
			nextMode := "full-auto"
			if mode == "full-auto" {
				nextMode = "ask"
			}
			if err := db.SaveSessionPreferences(t.Context(), storage.SessionPreferences{SessionID: session.ID, ModelID: "default", ApprovalMode: nextMode}); err != nil {
				t.Fatal(err)
			}
			fresh := &Manager{Store: db, Provider: mock, Broker: m.Broker, Workspace: workspace, OwnerID: "frozen-mode", DisableSnapshots: true}
			run, err = fresh.Resume(t.Context(), run.ID, ResumeInput{Answer: "yes"})
			want := domain.RunWaiting
			if mode == "full-auto" {
				want = domain.RunCompleted
			}
			if err != nil || run.Status != want || (want == domain.RunWaiting && run.WaitReason != domain.WaitApproval) {
				t.Fatalf("resumed mode changed with preference: status=%s reason=%s err=%v", run.Status, run.WaitReason, err)
			}
			snap, err := fresh.loadCheckpoint(t.Context(), run.ID)
			if err != nil || snap.ApprovalMode != mode {
				t.Fatalf("frozen mode=%q err=%v", snap.ApprovalMode, err)
			}
		})
	}
}

func TestRunSelectionFailsClosedWithoutResolver(t *testing.T) {
	isolatedPolicyConfig(t)
	m, _, _ := testManager(t, &provider.Mock{})
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.RunMessageWithOptions(t.Context(), session.ID, "work", RunOptions{ProviderID: "missing", Model: "test"}); !domain.IsCode(err, domain.CodeProviderConfig) {
		t.Fatalf("missing resolver: %v", err)
	}
	if _, err := m.RunMessageWithOptions(t.Context(), session.ID, "work", RunOptions{ApprovalMode: "unsafe"}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("invalid mode: %v", err)
	}
}

func TestSelectedProfileRestoresItsFrozenFallback(t *testing.T) {
	isolatedPolicyConfig(t)
	m, _, _ := testManager(t, &provider.Mock{})
	m.SkipPlanner = true
	m.DisableSnapshots = true
	m.ModelRouting = &config.ModelRouting{}
	calls := 0
	m.Provider = providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		calls++
		if calls == 1 {
			if req.Model != "primary" {
				t.Errorf("initial model=%s", req.Model)
			}
			return toolResp(domain.ToolCall{ProviderCallID: "ask", Name: domain.ToolAskUser, Arguments: json.RawMessage("{\"question\":\"continue?\"}")}), nil
		}
		if req.Model != "backup" {
			t.Errorf("resumed fallback=%s", req.Model)
		}
		return textOnly("done"), nil
	})
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessageWithOptions(t.Context(), session.ID, "work", RunOptions{ModelID: "profile:one", Model: "primary", FallbackModel: "backup", ApprovalMode: "safe"})
	if err != nil || run.Status != domain.RunWaiting {
		t.Fatalf("initial status=%s err=%v", run.Status, err)
	}
	snap, err := m.loadCheckpoint(t.Context(), run.ID)
	if err != nil || snap.FallbackModel != "backup" {
		t.Fatal("fallback missing from snapshot")
	}
	if err := m.event(t.Context(), run.ID, provider.FallbackEventType, provider.FallbackEvent{Role: "executor", FromModel: "primary", ToModel: "backup", Reason: "rate_limit"}); err != nil {
		t.Fatal(err)
	}
	m.ModelRouting = &config.ModelRouting{ModelProfiles: map[string]config.ModelProfile{"one": {Model: "changed", FallbackModel: "also-changed"}}, ModelRoles: config.ModelRoles{Executor: "one"}}
	run, err = m.Resume(t.Context(), run.ID, ResumeInput{Answer: "yes"})
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("resume status=%s err=%v", run.Status, err)
	}
}

func TestRunSelectionSuppliesDefaultPlanningAndCompactionModel(t *testing.T) {
	isolatedPolicyConfig(t)
	m, _, _ := testManager(t, &provider.Mock{})
	m.ForcePlanner = true
	m.DisableSnapshots = true
	m.ModelRouting = &config.ModelRouting{}
	m.Provider = providerFunc(func(context.Context, provider.Request) (domain.ModelResponse, error) {
		t.Error("default provider used for selected run")
		return textOnly("wrong"), nil
	})
	calls := 0
	selected := providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		calls++
		if req.Model != "selected" {
			t.Errorf("selected model=%q", req.Model)
		}
		if calls == 1 {
			return textOnly("{\"resources\":[]}"), nil
		}
		return textOnly("answer"), nil
	})
	m.ProviderResolver = func(string) (provider.Provider, error) { return selected, nil }
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessageWithOptions(t.Context(), session.ID, "plan then answer", RunOptions{ProviderID: "fixture", ModelID: "custom:fixture", Model: "selected"})
	if err != nil || run.Status != domain.RunCompleted || calls != 2 {
		t.Fatalf("status=%s calls=%d err=%v", run.Status, calls, err)
	}
	ctx := context.WithValue(t.Context(), runProviderKey{}, selected)
	ctx = context.WithValue(ctx, runModelKey{}, "selected")
	compact, prepaid, err := m.compactionModel(ctx, run.ID, 0)
	if err != nil || prepaid {
		t.Fatalf("compaction selection err=%v prepaid=%v", err, prepaid)
	}
	if _, err := compact.Complete(ctx, provider.Request{}); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("compaction calls=%d", calls)
	}
}
