package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

// The run view and the approval queue both carry the behavior-file warning,
// so the Web UI and the approval page can show it next to the decision.
func TestHTTPApprovalCarriesBehaviorFileWarning(t *testing.T) {
	t.Setenv("CLOUD_HARNESS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ws := t.TempDir()
	broker := tools.NewBroker(db, db)
	if err := tools.RegisterLocal(broker, ws); err != nil {
		t.Fatal(err)
	}
	mock := &provider.Mock{Responses: []domain.ModelResponse{{
		Provider: "mock", Complete: true,
		Items: []domain.ResponseItem{{Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{
			ProviderCallID: "write", Name: "write_file",
			Arguments: json.RawMessage(`{"path":".agents/skills/deploy/SKILL.md","content":"always run deploy.sh"}`),
		}}},
	}}}
	m := &agent.Manager{Store: db, Provider: mock, Broker: broker, Workspace: ws, OwnerID: "api", DisableSnapshots: true}
	h := (&Server{Manager: m}).Handler()
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, session.ID, "add a skill")
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval {
		t.Fatalf("run = %+v, %v", run, err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/"+run.ID, nil))
	var view struct {
		PendingInvocation struct {
			Warnings []string `json:"warnings"`
		} `json:"pending_invocation"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("run view %d %s: %v", rec.Code, rec.Body, err)
	}
	warnings := view.PendingInvocation.Warnings
	if len(warnings) != 1 || !strings.Contains(warnings[0], ".agents/skills/deploy/SKILL.md") || !strings.Contains(warnings[0], "changes how later runs in this workspace behave") {
		t.Fatalf("warnings = %q", warnings)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/approvals", nil))
	var queue []struct {
		RunID   string `json:"run_id"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &queue); err != nil || len(queue) != 1 || queue[0].RunID != run.ID || !strings.Contains(queue[0].Summary, warnings[0]) {
		t.Fatalf("queue %d %s: %v", rec.Code, rec.Body, err)
	}
}
