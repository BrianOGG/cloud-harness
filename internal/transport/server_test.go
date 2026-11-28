package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
	"github.com/BrianOGG/cloud-harness/internal/tools"
	"github.com/BrianOGG/cloud-harness/internal/trace"
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

// Simulate a preferences edit after the HTTP handler reads the session
// settings but before startup commits the run.
type editPreferencesDuringStartup struct{ storage.Store }

func (s editPreferencesDuringStartup) CreateTurn(ctx context.Context, turn storage.Turn) error {
	err := s.SaveSessionPreferences(ctx, storage.SessionPreferences{SessionID: turn.SessionID, ModelID: "default", ApprovalMode: "safe"})
	if err != nil {
		return err
	}
	return s.Store.CreateTurn(ctx, turn)
}

func TestRunStartupPreservesLaterPreferenceEdit(t *testing.T) {
	m, h := newServer(t)
	m.Store = editPreferencesDuringStartup{m.Store}
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/sessions/" + session.ID + "/runs?wait=1"
	rec := modelConfigRequest(t, h, "POST", path, map[string]string{"message": "hello", "approval_mode": "full-auto"}, 201, "")
	var run view
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil || run.ApprovalMode != "full-auto" {
		t.Fatalf("run lost the submitted approval mode: %s", rec.Body)
	}
	prefs, err := m.Store.GetSessionPreferences(t.Context(), session.ID)
	if err != nil || prefs.ApprovalMode != "safe" {
		t.Fatalf("startup overwrote next-turn preferences: %+v, %v", prefs, err)
	}
}

func TestRunInheritingPreferencesKeepsExistingRevision(t *testing.T) {
	m, h := newServer(t)
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/sessions/" + session.ID + "/runs?wait=1"
	modelConfigRequest(t, h, "POST", path, map[string]string{"message": "hello", "approval_mode": "safe"}, 201, "")
	before, err := m.Store.GetSessionPreferences(t.Context(), session.ID)
	if err != nil || before.ApprovalMode != "safe" {
		t.Fatalf("explicit preferences: %+v, %v", before, err)
	}
	modelConfigRequest(t, h, "POST", path, map[string]string{"message": "hello again"}, 201, "")
	after, err := m.Store.GetSessionPreferences(t.Context(), session.ID)
	if err != nil || before != after {
		t.Fatalf("inheriting preferences invalidated an overlapping update: before=%+v after=%+v err=%v", before, after, err)
	}
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
	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/"+created.ID+"/runs?wait=1", strings.NewReader(`{"message":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
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

func TestBudgetRouteRaisesTheTokenLimit(t *testing.T) {
	m, h := newServer(t)
	m.Provider = &provider.Mock{Responses: []domain.ModelResponse{
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{
			{Ordinal: 0, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "c1", Name: "retrieve_history", Arguments: json.RawMessage(`{"query":"anything"}`)}},
			{Ordinal: 1, Kind: domain.ItemUsage, Usage: &domain.Usage{InputTokens: 90, OutputTokens: 20, CachedInputTokens: 12, CacheCreationInputTokens: 5, ReasoningTokens: 7, CacheReported: true, ReasoningReported: true}},
		}},
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "done"}}},
	}}
	m.SkipPlanner = true
	m.MaxRunTokens = 100
	ctx := context.Background()
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "use tokens")
	if err != nil || run.WaitReason != domain.WaitBudget {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/runs/"+run.ID+"/budget?wait=1", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(`{"extra_model_calls":10}`); rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "token budget exhausted") {
		t.Fatalf("model calls alone resumed a run that is out of tokens: %d %s", rec.Code, rec.Body)
	}
	rec := post(`{"extra_tokens":500}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"COMPLETED"`) || !strings.Contains(rec.Body.String(), `"max_tokens":600`) || !strings.Contains(rec.Body.String(), `"used_input_tokens":90`) {
		t.Fatalf("budget %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/v1/runs/" + run.ID, "/v1/sessions/" + ses.ID + "/runs"} {
		result := httptest.NewRecorder()
		h.ServeHTTP(result, httptest.NewRequest(http.MethodGet, path, nil))
		if result.Code != http.StatusOK {
			t.Fatalf("get %s: %d %s", path, result.Code, result.Body)
		}
		for _, field := range []string{`"used_cached_input_tokens":12`, `"used_cache_creation_input_tokens":5`, `"used_reasoning_tokens":7`, `"cache_reported":true`, `"reasoning_reported":true`} {
			if !strings.Contains(result.Body.String(), field) {
				t.Fatalf("get %s omitted %s: %s", path, field, result.Body)
			}
		}
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
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/"+run.ID+"/responses?wait=1", strings.NewReader(`{"answer":"yes"}`))
	req.Header.Set("Content-Type", "application/json")
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
	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/"+ses.ID+"/runs?wait=1", strings.NewReader(`{"message":"deploy app"}`))
	req.Header.Set("Content-Type", "application/json")
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
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run answer %d %s", rec.Code, rec.Body)
	}

	// POST the answer to resume and complete the run.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/runs/"+started.ID+"/answer?wait=1", strings.NewReader(`{"answer":"eu-central-1"}`))
	req.Header.Set("Content-Type", "application/json")
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
	req = httptest.NewRequest(http.MethodPost, "/v1/runs/"+started.ID+"/answer?wait=1", strings.NewReader(`{"answer":"eu-central-1"}`))
	req.Header.Set("Content-Type", "application/json")
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
	if len(apprState.PendingInvocation.Warnings) != 0 {
		t.Fatalf("ordinary write carries a warning: %q", apprState.PendingInvocation.Warnings)
	}
	if !strings.Contains(apprState.PendingInvocation.ArgsSummary, "db.conf") {
		t.Fatalf("expected args_summary to mention db.conf, got %q", apprState.PendingInvocation.ArgsSummary)
	}
	wantRes := filepath.Join(ws, "db.conf")
	if len(apprState.PendingInvocation.Resources) == 0 || apprState.PendingInvocation.Resources[0] != wantRes {
		t.Fatalf("expected resources [%s], got %+v", wantRes, apprState.PendingInvocation.Resources)
	}
}

func TestHTTPTokenAuthRequired(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	token := "super-secret-token-12345"
	m := &agent.Manager{Store: db, Provider: &provider.Mock{}, OwnerID: "api"}
	ses, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, ses.ID, "hello")
	if err != nil {
		t.Fatal(err)
	}

	h := (&Server{Manager: m, Token: token}).Handler()

	for _, path := range []string{"/v1/approvals", "/v1/runs/" + run.ID, "/v1/runs/" + run.ID + "/events"} {
		for _, tc := range []struct {
			name   string
			header string
			status int
		}{
			{"missing", "", http.StatusUnauthorized},
			{"wrong", "Bearer wrong-token-value-00000", http.StatusUnauthorized},
			{"correct", "Bearer " + token, http.StatusOK},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, path, nil)
				if tc.header != "" {
					req.Header.Set("Authorization", tc.header)
				}
				h.ServeHTTP(rec, req)
				if rec.Code != tc.status {
					t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body)
				}
				if tc.status == http.StatusUnauthorized {
					if rec.Header().Get("WWW-Authenticate") != `Bearer realm="cloud-harness"` {
						t.Fatalf("missing Bearer challenge: %v", rec.Header())
					}
					var body struct {
						Error *domain.Error `json:"error"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == nil || body.Error.Code != domain.CodeAuthDenied || body.Error.Message != "missing or invalid bearer token" {
						t.Fatalf("auth error envelope: %s (%v)", rec.Body, err)
					}
					if strings.Contains(rec.Body.String(), token) || strings.Contains(rec.Body.String(), "wrong-token-value-00000") {
						t.Fatal("auth error echoed a token")
					}
				}
			})
		}
	}

	// 4. /healthz stays open without token.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /healthz without token, got %d %s", rec.Code, rec.Body)
	}
}

func TestServeRefusesNonLoopbackWithoutToken(t *testing.T) {
	token := "0123456789abcdef"

	// Non-loopback + no token + no allow flag -> error.
	if err := config.ValidateBind("0.0.0.0:19421", "", false); err == nil {
		t.Fatal("expected error for non-loopback without token or allow flag")
	}

	// The legacy allow flag cannot bypass the token requirement.
	if err := config.ValidateBind("0.0.0.0:19421", "", true); err == nil {
		t.Fatal("legacy allow flag permitted non-loopback bind without token")
	}

	// Non-loopback + valid token + no allow flag -> ok.
	if err := config.ValidateBind("0.0.0.0:19421", token, false); err != nil {
		t.Fatalf("expected valid token to permit non-loopback bind, got %v", err)
	}

	// Loopback + no token + no allow flag -> ok.
	if err := config.ValidateBind("127.0.0.1:19421", "", false); err != nil {
		t.Fatalf("expected loopback without token to succeed, got %v", err)
	}
}

func TestSSELastEventIDReplay(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UTC()
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_sse", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, storage.Run{
		ID: "run_sse", SessionID: "ses_sse", Status: domain.RunRunning,
		Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	for _, step := range []string{"step.one", "step.two", "step.three"} {
		ev, err := trace.Persistent("run_sse", step, map[string]string{"name": step}, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.AppendEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	m := &agent.Manager{Store: db, Provider: &provider.Mock{}, OwnerID: "api"}
	srv := &Server{
		Manager:           m,
		HeartbeatInterval: 20 * time.Millisecond,
		PollInterval:      10 * time.Millisecond,
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, ts.URL+"/v1/runs/run_sse/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", "1")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	var lines []string
	readUntil := func(match func(joined string) bool) string {
		for {
			line, rerr := reader.ReadString('\n')
			if line != "" {
				lines = append(lines, line)
				joined := strings.Join(lines, "")
				if match(joined) {
					return joined
				}
			}
			if rerr != nil {
				t.Fatalf("stream read error: %v (collected:\n%s)", rerr, strings.Join(lines, ""))
			}
		}
	}

	// Wait until replayed events 2 and 3 plus at least one ": ping" heartbeat arrive.
	got := readUntil(func(joined string) bool {
		return strings.Contains(joined, "id: 2\n") &&
			strings.Contains(joined, "event: step.two\n") &&
			strings.Contains(joined, "id: 3\n") &&
			strings.Contains(joined, "event: step.three\n") &&
			strings.Contains(joined, ": ping\n")
	})
	if strings.Contains(got, "id: 1\n") || strings.Contains(got, "event: step.one\n") {
		t.Fatalf("event 1 should not be replayed after Last-Event-ID=1:\n%s", got)
	}

	// Publish a live event (id: 4) and verify it streams after the replayed ones.
	liveEv, err := trace.Persistent("run_sse", "step.four", map[string]string{"name": "step.four"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendEvent(ctx, liveEv); err != nil {
		t.Fatal(err)
	}
	srv.Notify("run_sse")

	got = readUntil(func(joined string) bool {
		return strings.Contains(joined, "id: 4\n") && strings.Contains(joined, "event: step.four\n")
	})
	idx2 := strings.Index(got, "id: 2\n")
	idx3 := strings.Index(got, "id: 3\n")
	idx4 := strings.Index(got, "id: 4\n")
	if !(idx2 >= 0 && idx2 < idx3 && idx3 < idx4) {
		t.Fatalf("expected events 2, 3, 4 in order:\n%s", got)
	}

	// Disconnect client and verify subscription cleanup.
	cancel()
	_ = resp.Body.Close()
	deadline := time.Now().Add(time.Second)
	for srv.SubscriberCount("run_sse") != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := srv.SubscriberCount("run_sse"); n != 0 {
		t.Fatalf("expected 0 active subscribers after disconnect, got %d", n)
	}
}

func TestHTTPBodyTooLarge(t *testing.T) {
	_, h := newServer(t)

	// Create a valid session with an ordinary small request first.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create session %d %s", rec.Code, rec.Body)
	}
	var ses struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ses); err != nil {
		t.Fatal(err)
	}

	// Send a POST body larger than 1 MiB -> 413 Request Entity Too Large.
	largeMsg := strings.Repeat("a", (1<<20)+1024)
	body := `{"message":"` + largeMsg + `"}`
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/sessions/"+ses.ID+"/runs?wait=1", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 Request Entity Too Large, got %d %s", rec.Code, rec.Body)
	}
}
