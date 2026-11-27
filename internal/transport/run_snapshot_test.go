package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestRunViewReportsFrozenModeAndTurnAcrossRestore(t *testing.T) {
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	m, _ := newServer(t)
	m.SkipPlanner, m.DisableSnapshots = true, true
	m.Workspace = t.TempDir()
	m.Provider = &provider.Mock{Responses: []domain.ModelResponse{{
		Provider: "mock", Complete: true,
		Items: []domain.ResponseItem{{Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{
			ProviderCallID: "diagnostic", Name: "run_command",
			Arguments: json.RawMessage(`{"command":"uname -a"}`),
		}}},
	}}}
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessageWithOptions(t.Context(), session.ID, "diagnose", agent.RunOptions{ApprovalMode: "workspace-write"})
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval {
		t.Fatalf("expected command approval wait: status=%s reason=%s err=%v", run.Status, run.WaitReason, err)
	}
	if err := m.Store.SaveSessionPreferences(t.Context(), storage.SessionPreferences{SessionID: session.ID, ModelID: "default", ApprovalMode: "full-auto"}); err != nil {
		t.Fatal(err)
	}
	// A fresh manager and a changed default workspace simulate another serve
	// process. The API still reports the run checkpoint, never these defaults.
	restored := &agent.Manager{Store: m.Store, Workspace: t.TempDir(), OwnerID: "restored"}
	h := (&Server{Manager: restored}).Handler()
	for _, path := range []string{"/v1/runs/" + run.ID, "/v1/sessions/" + session.ID + "/runs"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d", path, rec.Code)
		}
		var got view
		if path == "/v1/runs/"+run.ID {
			err = json.Unmarshal(rec.Body.Bytes(), &got)
		} else {
			var rows []view
			err = json.Unmarshal(rec.Body.Bytes(), &rows)
			if len(rows) != 1 {
				t.Fatalf("run count=%d", len(rows))
			}
			got = rows[0]
		}
		if err != nil || got.ApprovalMode != "workspace-write" || got.TurnID != run.TurnID || got.TurnID == "" || got.Workspace != m.Workspace {
			t.Fatalf("frozen public choices changed: mode=%q turn=%q workspace=%q err=%v", got.ApprovalMode, got.TurnID, got.Workspace, err)
		}
		if got.Status != domain.RunWaiting || got.PendingInvocation == nil || got.PendingInvocation.Tool != "run_command" {
			t.Fatal("preference update changed or hid the pending command")
		}
		var public map[string]json.RawMessage
		if path == "/v1/runs/"+run.ID {
			if err := json.Unmarshal(rec.Body.Bytes(), &public); err != nil {
				t.Fatal(err)
			}
			if _, ok := public["policy"]; ok {
				t.Fatal("run view exposed the complete policy snapshot")
			}
		}
	}
}

func TestRunViewDoesNotInventUnavailableSnapshotMode(t *testing.T) {
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	m, _ := newServer(t)
	m.SkipPlanner, m.DisableSnapshots = true, true
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessageWithOptions(t.Context(), session.ID, "hello", agent.RunOptions{ApprovalMode: "safe"})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Manager: m}
	for index, raw := range []string{`{"messages":[]}`, `{"approval_mode":"unknown"}`, `{"approval_mode":"ask"`, `null`} {
		if err := m.Store.SaveCheckpoint(t.Context(), run.ID, run.Version+int64(index)+1, []byte(raw), time.Now()); err != nil {
			t.Fatal(err)
		}
		got := server.runView(t.Context(), run)
		if got.ApprovalMode != "" || got.Workspace != "" {
			t.Fatalf("unknown checkpoint invented choices: mode=%q workspace=%q", got.ApprovalMode, got.Workspace)
		}
	}
	missing := server.runView(t.Context(), storage.Run{ID: "run-no-checkpoint", Status: domain.RunCompleted})
	if missing.ApprovalMode != "" || missing.Workspace != "" {
		t.Fatal("missing checkpoint inherited session/default choices")
	}
}
