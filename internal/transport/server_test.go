package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

func newServer(t *testing.T) (*agent.Manager, http.Handler) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	m := &agent.Manager{Store: db, Provider: &provider.Mock{}, OwnerID: "api"}
	return m, (&Server{Manager: m}).Handler()
}

func TestMockRunOverHTTP(t *testing.T) {
	_, h := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", bytes.NewReader(nil))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("session %d %s", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatal(rec.Body)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/"+created.ID+"/runs", strings.NewReader(`{"message":"hello"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("run %d %s", rec.Code, rec.Body)
	}
	var run struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		FinalAnswer string `json:"final_answer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.Status != "COMPLETED" || run.FinalAnswer != "Mock answer: hello" {
		t.Fatalf("%+v", run)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/runs/"+run.ID+"/events", nil)
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "id: 1\n") || !strings.Contains(body, "event: run.created") {
		t.Fatalf("sse:\n%s", body)
	}
	first := body
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/runs/"+run.ID+"/events?preview=1", nil)
	req.Header.Set("Last-Event-ID", "1")
	h.ServeHTTP(rec, req)
	replay := rec.Body.String()
	if strings.Contains(replay, "id: 1\n") {
		t.Fatalf("replayed the first event:\n%s", replay)
	}
	if !strings.Contains(replay, "event: stream.delta") || strings.Contains(replay, "id: ") && strings.Contains(replay, "stream.delta") {
		// The delta must appear, and the line immediately before it must not be an id.
	}
	if !strings.Contains(replay, "event: stream.delta\ndata: {\"text\":\"preview\"}") {
		t.Fatalf("preview missing:\n%s", replay)
	}
	deltaAt := strings.Index(replay, "event: stream.delta")
	if strings.Contains(replay[deltaAt-10:deltaAt], "id:") {
		t.Fatalf("delta has a cursor:\n%s", replay)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/runs/"+run.ID+"/events", nil)
	h.ServeHTTP(rec, req)
	if rec.Body.String() != first {
		t.Fatal("reconnecting replayed a different log or stored the preview")
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/runs/"+run.ID+"/trace", nil)
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "opaque") && strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("trace %s", rec.Body)
	}
}

func TestResumeDoesNotReplaceRun(t *testing.T) {
	m, h := newServer(t)
	m.UseBudget = true
	m.InitialBudget = domain.Budget{MaxModelCalls: 0}
	ctx := context.Background()
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "need budget")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/"+run.ID+"/responses", strings.NewReader(`{"answer":"yes"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("responses %d %s", rec.Code, rec.Body)
	}
	got, err := m.Store.GetRun(ctx, run.ID)
	if err != nil || got.ID != run.ID || got.Status != domain.RunWaiting {
		t.Fatalf("run changed %+v %v", got, err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/runs/"+run.ID+"/cancel", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"CANCELLED"`) {
		t.Fatalf("cancel %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/runs/"+run.ID+"/cancel", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("second cancel %d %s", rec.Code, rec.Body)
	}
}

func TestServeStartupAutoRecoversExpiredRunningRuns(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	past := time.Now().UTC().Add(-10 * time.Minute)
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_1", CreatedAt: past, UpdatedAt: past}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, storage.Run{
		ID: "run_crashed", SessionID: "ses_1", Status: domain.RunRunning,
		Budget: domain.DefaultBudget(), Version: 1, CreatedAt: past, UpdatedAt: past,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveBatch(ctx, storage.StoredBatch{
		ID: "bat_1", RunID: "run_crashed", Status: domain.BatchOpen, Action: domain.ActionExecute,
		Invocations: []domain.Invocation{{
			ID: "inv_1", Ordinal: 0, ProviderCallID: "c1", Name: "write_file",
			Arguments: json.RawMessage(`{"path":"out.txt","content":"x"}`), Class: domain.ActionExecute,
			Status: domain.InvRunning, Dispatched: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireLease(ctx, "run_crashed", "old-server", past, past.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	m := &agent.Manager{Store: db, Provider: &provider.Mock{}, OwnerID: "api"}
	h := (&Server{Manager: m}).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/run_crashed", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get run %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"WAITING"`) || !strings.Contains(rec.Body.String(), `"RECONCILIATION"`) {
		t.Fatalf("expected auto-recovered WAITING(RECONCILIATION), got %s", rec.Body.String())
	}
}

func TestHTTPAnswerEndpointResumesRun(t *testing.T) {
	m, h := newServer(t)
	m.Provider = &provider.Mock{Responses: []domain.ModelResponse{
		{
			Provider: "mock", Complete: true,
			Items: []domain.ResponseItem{{
				Ordinal: 0, Kind: domain.ItemToolCall,
				ToolCall: &domain.ToolCall{
					ProviderCallID: "ask_http_1",
					Name:           domain.ToolAskUser,
					Arguments:      json.RawMessage(`{"question":"Which region should I deploy to?"}`),
				},
			}},
		},
		{
			Provider: "mock", Complete: true,
			Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "Deployed to eu-central-1"}},
		},
	}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session %d %s", rec.Code, rec.Body)
	}
	var ses struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ses); err != nil || ses.ID == "" {
		t.Fatalf("session decode: %v %s", err, rec.Body)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/"+ses.ID+"/runs", strings.NewReader(`{"message":"deploy app"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create run %d %s", rec.Code, rec.Body)
	}
	var started view
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	if started.Status != domain.RunWaiting || started.WaitReason != domain.WaitUser {
		t.Fatalf("expected WAITING(USER), got %+v", started)
	}

	// 404 for unknown run.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/runs/run_unknown/answer", strings.NewReader(`{"answer":"eu-central-1"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run answer %d %s", rec.Code, rec.Body)
	}

	// POST the answer to resume and complete the run.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/runs/"+started.ID+"/answer", strings.NewReader(`{"answer":"eu-central-1"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d %s", rec.Code, rec.Body)
	}
	var completed view
	if err := json.Unmarshal(rec.Body.Bytes(), &completed); err != nil {
		t.Fatal(err)
	}
	if completed.ID != started.ID || completed.Status != domain.RunCompleted || completed.FinalAnswer != "Deployed to eu-central-1" {
		t.Fatalf("completed run %+v", completed)
	}

	// POSTing again when the run is no longer waiting yields 409 Conflict.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/runs/"+started.ID+"/answer", strings.NewReader(`{"answer":"eu-central-1"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 on completed run, got %d %s", rec.Code, rec.Body)
	}
}

func TestHTTPGetRunState(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	broker := tools.NewBroker(db, db)
	if err := tools.RegisterLocal(broker, ws); err != nil {
		t.Fatal(err)
	}
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		{
			Provider: "mock", Complete: true,
			Items: []domain.ResponseItem{{
				Ordinal: 0, Kind: domain.ItemToolCall,
				ToolCall: &domain.ToolCall{
					ProviderCallID: "ask_state_1",
					Name:           domain.ToolAskUser,
					Arguments:      json.RawMessage(`{"question":"What database port should we use?"}`),
				},
			}},
		},
		{
			Provider: "mock", Complete: true,
			Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "Using port 5432"}},
		},
		{
			Provider: "mock", Complete: true,
			Items: []domain.ResponseItem{{
				Ordinal: 0, Kind: domain.ItemToolCall,
				ToolCall: &domain.ToolCall{
					ProviderCallID: "write_state_1",
					Name:           "write_file",
					Arguments:      json.RawMessage(`{"path":"db.conf","content":"port=5432"}`),
				},
			}},
		},
	}}
	m := &agent.Manager{Store: db, Provider: mock, Broker: broker, Workspace: ws, OwnerID: "api"}
	h := (&Server{Manager: m}).Handler()

	// 404 for unknown run.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/run_missing", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing run %d %s", rec.Code, rec.Body)
	}

	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Run waiting on ask_user.
	userRun, err := m.RunMessage(ctx, ses.ID, "configure db")
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/runs/"+userRun.ID, nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get user waiting run %d %s", rec.Code, rec.Body)
	}
	var userState view
	if err := json.Unmarshal(rec.Body.Bytes(), &userState); err != nil {
		t.Fatal(err)
	}
	if userState.ID != userRun.ID || userState.SessionID != ses.ID || userState.Status != domain.RunWaiting {
		t.Fatalf("unexpected user wait state: %+v", userState)
	}
	if userState.WaitReason != domain.WaitUser || userState.WaitKind != domain.WaitUser {
		t.Fatalf("expected wait kind USER, got reason=%q kind=%q", userState.WaitReason, userState.WaitKind)
	}
	if userState.Question != "What database port should we use?" {
		t.Fatalf("expected pending question, got %q", userState.Question)
	}

	// Resume the user wait and verify GET returns COMPLETED.
	if _, err := m.Resume(ctx, userRun.ID, agent.ResumeInput{Answer: "5432"}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/runs/"+userRun.ID, nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get completed run %d %s", rec.Code, rec.Body)
	}
	var doneState view
	if err := json.Unmarshal(rec.Body.Bytes(), &doneState); err != nil {
		t.Fatal(err)
	}
	if doneState.Status != domain.RunCompleted || doneState.FinalAnswer != "Using port 5432" || doneState.Question != "" {
		t.Fatalf("unexpected completed state: %+v", doneState)
	}

	// 2. Run waiting on approval.
	apprRun, err := m.RunMessage(ctx, ses.ID, "write db.conf")
	if err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/runs/"+apprRun.ID, nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get approval waiting run %d %s", rec.Code, rec.Body)
	}
	var apprState view
	if err := json.Unmarshal(rec.Body.Bytes(), &apprState); err != nil {
		t.Fatal(err)
	}
	if apprState.ID != apprRun.ID || apprState.SessionID != ses.ID || apprState.Status != domain.RunWaiting {
		t.Fatalf("unexpected approval wait state: %+v", apprState)
	}
	if apprState.WaitReason != domain.WaitApproval || apprState.WaitKind != domain.WaitApproval {
		t.Fatalf("expected wait kind APPROVAL, got reason=%q kind=%q", apprState.WaitReason, apprState.WaitKind)
	}
	if apprState.PendingInvocation == nil {
		t.Fatalf("expected pending_invocation in approval wait response: %s", rec.Body.String())
	}
	if apprState.PendingInvocation.Tool != "write_file" {
		t.Fatalf("expected pending tool write_file, got %q", apprState.PendingInvocation.Tool)
	}
	if !strings.Contains(apprState.PendingInvocation.ArgsSummary, "db.conf") {
		t.Fatalf("expected args_summary to mention db.conf, got %q", apprState.PendingInvocation.ArgsSummary)
	}
	wantRes := filepath.Join(ws, "db.conf")
	if len(apprState.PendingInvocation.Resources) == 0 || apprState.PendingInvocation.Resources[0] != wantRes {
		t.Fatalf("expected resources [%s], got %+v", wantRes, apprState.PendingInvocation.Resources)
	}
}
