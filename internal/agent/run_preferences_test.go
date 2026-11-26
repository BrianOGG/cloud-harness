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
	run, err := m.RunMessageWithOptions(t.Context(), session.ID, "work", RunOptions{ModelID: "custom:example", Model: "selected-model", ProviderID: "example", ApprovalMode: "workspace-write"})
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitUser {
		t.Fatalf("initial: %+v %v", run, err)
	}
	snap, err := m.loadCheckpoint(t.Context(), run.ID)
	if err != nil || snap.Model != "selected-model" || snap.ProviderID != "example" || snap.ApprovalMode != "workspace-write" {
		t.Fatalf("snapshot selection missing: %v", err)
	}
	if err := db.SaveSessionPreferences(t.Context(), storage.SessionPreferences{SessionID: session.ID, ModelID: "default", ApprovalMode: "ask"}); err != nil {
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
	} {
		t.Run(tc.mode+"/"+tc.tool, func(t *testing.T) {
			isolatedPolicyConfig(t)
			m, _, root := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{toolResp(domain.ToolCall{ProviderCallID: "tool", Name: tc.tool, Arguments: json.RawMessage(tc.args)}), textOnly("done")}})
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
			if tc.mode == "workspace-write" && tc.tool != "run_command" {
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
