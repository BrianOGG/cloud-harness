package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/tools"
	"github.com/BrianOGG/cloud-harness/internal/trace"
)

// Exercise real invocation persistence and HTTP assembly using only a scripted
// provider and a temporary workspace/database. No command is approved or run.
func TestHTTPTracePreservesArgumentsResultsAndDenial(t *testing.T) {
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	base, _ := newServer(t)
	m := &agent.Manager{Store: base.Store, OwnerID: "trace-contract", SkipPlanner: true, DisableSnapshots: true, Workspace: t.TempDir()}
	m.Broker = tools.NewBroker(m.Store, m.Store)
	if err := os.WriteFile(filepath.Join(m.Workspace, "fixture.txt"), []byte("fixture read output"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.Provider = &provider.Mock{Responses: []domain.ModelResponse{
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{
			{Kind: domain.ItemToolCall, Ordinal: 0, ToolCall: &domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"fixture.txt"}`)}},
			{Kind: domain.ItemOpaque, Ordinal: 1, Opaque: &domain.OpaqueItem{Provider: "mock", Payload: []byte(`{"private":"trace-private-provider-marker"}`)}},
		}},
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{
			{Kind: domain.ItemToolCall, Ordinal: 0, ToolCall: &domain.ToolCall{ProviderCallID: "diagnostic", Name: "run_command", Arguments: json.RawMessage(`{"command":"uname -a","cwd":"."}`)}},
		}},
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemText, Text: "The command was declined."}}},
	}}
	h := (&Server{Manager: m}).Handler()
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessageWithOptions(t.Context(), session.ID, "read and diagnose", agent.RunOptions{ApprovalMode: "workspace-write"})
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval {
		t.Fatalf("expected pending command: status=%s reason=%s err=%v", run.Status, run.WaitReason, err)
	}
	wait, err := m.WaitInfo(t.Context(), run)
	if err != nil || wait.PendingInvocation == nil || wait.PendingInvocation.Tool != "run_command" || string(wait.PendingInvocation.Arguments) != `{"command":"uname -a","cwd":"."}` {
		t.Fatalf("expected the exact diagnostic command pending, not the file read: %+v err=%v", wait.PendingInvocation, err)
	}
	readTrace := func() map[string]trace.Call {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/"+run.ID+"/trace", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("trace status=%d", rec.Code)
		}
		for _, forbidden := range []string{"trace-private-provider-marker", `"opaque"`, `"continuation"`, `"snapshot"`} {
			if strings.Contains(rec.Body.String(), forbidden) {
				t.Fatalf("trace exposed private provider state %q", forbidden)
			}
		}
		var got trace.View
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		calls := make(map[string]trace.Call)
		for _, step := range got.Run.Steps {
			for _, batch := range step.Batches {
				for _, call := range batch.Calls {
					calls[call.ProviderCallID] = call
				}
			}
		}
		return calls
	}
	calls := readTrace()
	read := calls["read"]
	if string(read.Arguments) != `{"path":"fixture.txt"}` || read.Result == nil || !read.Result.OK || !strings.Contains(string(read.Result.Data), "fixture read output") {
		raw, _ := json.Marshal(read)
		t.Fatalf("successful tool arguments/result missing: %s", raw)
	}
	pending := calls["diagnostic"]
	if pending.Status != string(domain.InvWaitingApproval) || string(pending.Arguments) != `{"command":"uname -a","cwd":"."}` || pending.Result != nil {
		t.Fatalf("pending tool misrepresented: %+v", pending)
	}
	if _, err := m.Resume(t.Context(), run.ID, agent.ResumeInput{Decision: "deny"}); err != nil {
		t.Fatal(err)
	}
	denied := readTrace()["diagnostic"]
	if denied.ID != pending.ID || denied.Status != string(domain.InvDenied) || denied.Result == nil || denied.Result.OK || denied.Result.Error == nil || denied.Result.Error.Code != domain.CodeAuthDenied || denied.Result.Error.Message != "approval denied" {
		t.Fatalf("denied tool did not preserve invocation/error: %+v", denied)
	}
}
