package agent

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestRunRoutesPlannerExecutorAndDelegatesFromConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routing.json")
	if err := os.WriteFile(path, []byte(`{
		"model_profiles": {
			"plan":{"model":"planner-model"},
			"work":{"model":"executor-from-file"},
			"brief":{"model":"fast-model"}
		},
		"model_roles":{"planner":"plan","executor":"work","fast":"brief"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUD_HARNESS_CONFIG", path)
	t.Setenv("CLOUD_HARNESS_MODEL_PROFILES", "")
	t.Setenv("CLOUD_HARNESS_MODEL_ROLES", "")
	m, db, workspace := testManager(t, &provider.Mock{})
	m.ForcePlanner = true
	m.Model = "legacy-executor"
	writeDelegateDefinition(t, workspace, "custom", "model: child-override\n", "")
	steps := []struct {
		model string
		reply domain.ModelResponse
	}{
		{"planner-model", textOnly(`{"resources":[]}`)},
		{"executor-from-file", toolResp(domain.ToolCall{ProviderCallID: "d1", Name: domain.ToolDelegateTask, Arguments: json.RawMessage(`{"goal":"inherit the executor"}`)})},
		{"executor-from-file", textOnly("child done")},
		{"executor-from-file", toolResp(domain.ToolCall{ProviderCallID: "d2", Name: domain.ToolDelegateTask, Arguments: json.RawMessage(`{"goal":"use a model override","agent":"custom"}`)})},
		{"child-override", textOnly("custom done")},
		{"executor-from-file", textOnly("parent done")},
	}
	calls := 0
	m.Provider = providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		if calls >= len(steps) || req.Model != steps[calls].model {
			t.Fatalf("unexpected call %d on model %q", calls+1, req.Model)
		}
		step := steps[calls]
		calls++
		return step.reply, nil
	})
	run := runAgentDelegate(t, m)
	if calls != len(steps) || run.Budget.UsedModelCalls != len(steps) || run.Budget.UsedChildRuns != 2 {
		t.Fatalf("calls = %d, ledger = %+v", calls, run.Budget)
	}
	events, err := db.ListEvents(t.Context(), run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"planner.selected", "planner.completed"} {
		if !slices.ContainsFunc(events, func(ev storage.Event) bool { return ev.Type == want }) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestRunWithoutModelProfilesPreservesCallsAndEvents(t *testing.T) {
	t.Setenv("CLOUD_HARNESS_CONFIG", "")
	t.Setenv("CLOUD_HARNESS_MODEL_PROFILES", "")
	t.Setenv("CLOUD_HARNESS_MODEL_ROLES", "")
	mock := &provider.Mock{Responses: []domain.ModelResponse{textOnly(`{"resources":[]}`), textOnly("done")}}
	m, db, _ := testManager(t, mock)
	m.ForcePlanner = true
	m.Model = "legacy-override"
	run, err := m.RunMessage(t.Context(), routingSession(t, m), "plan and answer")
	if err != nil || run.Status != domain.RunCompleted || run.Budget.UsedModelCalls != 2 {
		t.Fatalf("run = %+v, error = %v", run, err)
	}
	var models []string
	for _, req := range mock.Requests {
		models = append(models, req.Model)
	}
	if want := []string{"", "legacy-override"}; !slices.Equal(models, want) {
		t.Errorf("legacy request models = %q; want %q", models, want)
	}
	events, err := db.ListEvents(t.Context(), run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, ev.Type)
	}
	want := []string{"run.created", "planner.completed", "planner.selected", "model.completed", "run.completed", "turn.summary"}
	if !slices.Equal(kinds, want) {
		t.Errorf("legacy event order = %v; want %v", kinds, want)
	}
}

func TestExecutorProfileFallbackCompletesAndSurvivesResume(t *testing.T) {
	t.Parallel()
	m, db, workspace := testManager(t, &provider.Mock{})
	m.ModelRouting = executorRouting()
	m.SkipPlanner = true
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var models []string
	m.Provider = modelHTTPProvider(t, func(model string) (int, string) {
		models = append(models, model)
		if model == "primary" {
			return http.StatusTooManyRequests, `upstream echoed fixture-key and private task text`
		}
		if len(models) == 5 {
			return http.StatusOK, `{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"plan","function":{"name":"update_plan","arguments":"{\"plan\":\"continue\"}"}}]}}]}`
		}
		return http.StatusOK, `{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"ask","function":{"name":"ask_user","arguments":"{\"question\":\"Continue?\"}"}}]}}]}`
	})
	waiting, err := m.RunMessage(ctx, routingSession(t, m), "private task text")
	if err != nil || waiting.Status != domain.RunWaiting || waiting.WaitReason != domain.WaitUser || waiting.Budget.UsedModelCalls != 6 {
		t.Fatalf("waiting run = %+v, error = %v, requests = %v", waiting, err, models)
	}
	// A fresh manager has no in-memory route state from the run that paused.
	resumer := &Manager{Store: db, Broker: m.Broker, Workspace: workspace, ModelRouting: executorRouting(),
		Provider: modelHTTPProvider(t, func(model string) (int, string) {
			models = append(models, model)
			return http.StatusOK, `{"choices":[{"message":{"content":"completed on backup"}}]}`
		}),
	}
	finished, err := resumer.Resume(ctx, waiting.ID, ResumeInput{Answer: "yes"})
	if err != nil || finished.Status != domain.RunCompleted || finished.FinalAnswer != "completed on backup" || finished.Budget.UsedModelCalls != len(models) {
		t.Fatalf("resumed run = %+v, error = %v, requests = %v", finished, err, models)
	}
	if want := []string{"primary", "primary", "primary", "primary", "backup", "backup", "backup"}; !slices.Equal(models, want) {
		t.Fatalf("request models = %v; want %v", models, want)
	}
	if evs := fallbackPayloads(t, db, finished.ID); len(evs) != 1 || !maps.Equal(evs[0], map[string]string{
		"role": "executor", "from_model": "primary", "to_model": "backup", "reason": "rate_limited",
	}) {
		t.Fatalf("fallback events = %+v", evs)
	}
	resumer.SkipPlanner = true
	another, err := resumer.RunMessage(ctx, finished.SessionID, "new run")
	if err != nil || another.Status != domain.RunCompleted || len(models) != 8 || models[7] != "primary" || len(fallbackPayloads(t, db, another.ID)) != 0 {
		t.Fatalf("new run inherited the switch: run = %+v, error = %v, requests = %v", another, err, models)
	}
}

func TestPlannerFallbackAlsoSwitchesTheExecutorOnASharedProfile(t *testing.T) {
	t.Parallel()
	m, db, _ := testManager(t, &provider.Mock{})
	m.ModelRouting = executorRouting()
	m.ModelRouting.ModelRoles.Planner = "work"
	m.ForcePlanner = true
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var models []string
	m.Provider = providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		models = append(models, req.Model)
		if req.Model == "primary" {
			return domain.ModelResponse{}, provider.NewHTTPStatusError(http.StatusTooManyRequests, "limited")
		}
		if len(req.Tools) == 0 {
			return textOnly(`{"resources":[]}`), nil
		}
		return textOnly("done"), nil
	})
	run, err := m.RunMessage(ctx, routingSession(t, m), "planned answer")
	if err != nil || run.Status != domain.RunCompleted || run.FinalAnswer != "done" || run.Budget.UsedModelCalls != len(models) ||
		!slices.Equal(models, []string{"primary", "primary", "primary", "primary", "backup", "backup"}) {
		t.Fatalf("run = %+v, error = %v, requests = %v", run, err, models)
	}
	if evs := fallbackPayloads(t, db, run.ID); len(evs) != 1 || evs[0]["role"] != "planner" {
		t.Fatalf("fallback events = %+v", evs)
	}
}

func TestFallbackCannotSpendPastModelBudget(t *testing.T) {
	t.Parallel()
	m, db, _ := testManager(t, &provider.Mock{})
	m.ModelRouting = executorRouting()
	m.UseBudget = true
	m.InitialBudget = domain.Budget{MaxModelCalls: 4}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var models []string
	m.Provider = providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		models = append(models, req.Model)
		if req.Model == "primary" {
			return domain.ModelResponse{}, provider.NewHTTPStatusError(http.StatusServiceUnavailable, "unavailable")
		}
		return textOnly("budget extended"), nil
	})
	run, err := m.RunMessage(ctx, routingSession(t, m), "check budget")
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitBudget || run.Budget.UsedModelCalls != 4 ||
		!slices.Equal(models, []string{"primary", "primary", "primary", "primary"}) {
		t.Fatalf("run = %+v, error = %v, requests = %v", run, err, models)
	}
	run, err = m.Resume(ctx, run.ID, ResumeInput{ExtraModelCalls: 1})
	if err != nil || run.Status != domain.RunCompleted || run.Budget.UsedModelCalls != 5 || len(models) != 5 || models[4] != "backup" {
		t.Fatalf("resumed run = %+v, error = %v, requests = %v", run, err, models)
	}
	if evs := fallbackPayloads(t, db, run.ID); len(evs) != 1 || evs[0]["reason"] != "upstream_5xx" {
		t.Fatalf("fallback events = %+v", evs)
	}
}

func TestExecutorProfileDoesNotRetryHTTP400(t *testing.T) {
	m, db, _ := testManager(t, &provider.Mock{})
	m.ModelRouting = executorRouting()
	var models []string
	m.Provider = modelHTTPProvider(t, func(model string) (int, string) {
		models = append(models, model)
		return http.StatusBadRequest, `{"message":"invalid request (example: HTTP 429)"}`
	})
	run, err := m.RunMessage(t.Context(), routingSession(t, m), "model request")
	if err != nil || run.Status != domain.RunFailed || !domain.IsCode(run.Error, domain.CodeProviderHTTP) || run.Budget.UsedModelCalls != 1 ||
		!slices.Equal(models, []string{"primary"}) || len(fallbackPayloads(t, db, run.ID)) != 0 {
		t.Fatalf("run = %+v, error = %v, requests = %v", run, err, models)
	}
}

func TestCancelRunDuringProfileBackoff(t *testing.T) {
	m, db, _ := testManager(t, &provider.Mock{})
	m.ModelRouting = executorRouting()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first := make(chan string, 1)
	m.Provider = providerFunc(func(ctx context.Context, req provider.Request) (domain.ModelResponse, error) {
		id, _ := ctx.Value(runKey).(string)
		select {
		case first <- id:
		default:
		}
		return domain.ModelResponse{}, provider.NewHTTPStatusError(http.StatusTooManyRequests, "limited")
	})
	session := routingSession(t, m)
	done := make(chan error, 1)
	go func() {
		_, err := m.RunMessage(ctx, session, "retry until canceled")
		done <- err
	}()
	var id string
	select {
	case id = <-first:
	case <-ctx.Done():
		t.Fatal("model was not called")
	}
	time.Sleep(30 * time.Millisecond) // Let Complete enter the one second backoff.
	start := time.Now()
	canceled, err := m.Cancel(ctx, id)
	if err != nil || canceled.Status != domain.RunCancelled {
		t.Errorf("Cancel = %+v, %v", canceled, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v during cancellation", err)
		}
	case <-time.After(700 * time.Millisecond):
		t.Fatal("run did not stop during the backoff")
	}
	run, err := db.GetRun(t.Context(), id)
	if err != nil || run.Status != domain.RunCancelled || run.Budget.UsedModelCalls != 1 || time.Since(start) >= 800*time.Millisecond || len(fallbackPayloads(t, db, id)) != 0 {
		t.Fatalf("canceled run = %+v, error = %v, elapsed = %v", run, err, time.Since(start))
	}
}

func TestFastProfileRoutesExistingSummaryWithinBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		budget   int
		wantCall bool
	}{{"available", 2, true}, {"exhausted", 1, false}} {
		t.Run(tc.name, func(t *testing.T) {
			m, db, _ := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{textOnly("answer")}})
			m.ModelRouting = &config.ModelRouting{
				ModelProfiles: map[string]config.ModelProfile{"summary": {Model: "fast-model"}},
				ModelRoles:    config.ModelRoles{Fast: "summary"},
			}
			m.UseBudget = true
			m.InitialBudget = domain.Budget{MaxModelCalls: tc.budget}
			gotCall := false
			m.Summarizer = providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
				gotCall = true
				if req.Model != "fast-model" {
					t.Errorf("summary model = %q", req.Model)
				}
				return textOnly(`{"turn_summary":"profiled summary"}`), nil
			})
			run, err := m.RunMessage(t.Context(), routingSession(t, m), "question")
			if err != nil || run.Status != domain.RunCompleted || run.Budget.UsedModelCalls != tc.budget || gotCall != tc.wantCall {
				t.Fatalf("run = %+v, error = %v, summary called = %t", run, err, gotCall)
			}
			if summary, err := db.GetTurnSummary(t.Context(), run.TurnID); tc.wantCall && (err != nil || summary != "profiled summary") {
				t.Fatalf("summary = %q, error = %v", summary, err)
			}
		})
	}
}

func executorRouting() *config.ModelRouting {
	return &config.ModelRouting{
		ModelProfiles: map[string]config.ModelProfile{"work": {Model: "primary", FallbackModel: "backup"}},
		ModelRoles:    config.ModelRoles{Executor: "work"},
	}
}

func routingSession(t *testing.T, m *Manager) string {
	t.Helper()
	session, err := m.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return session.ID
}

func fallbackPayloads(t *testing.T, db storage.Store, runID string) []map[string]string {
	t.Helper()
	events, err := db.ListEvents(t.Context(), runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var payloads []map[string]string
	for _, ev := range events {
		if ev.Type == provider.FallbackEventType {
			var payload map[string]string
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			payloads = append(payloads, payload)
		}
	}
	return payloads
}

type modelRoundTrip func(*http.Request) (*http.Response, error)

func (f modelRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func modelHTTPProvider(t *testing.T, reply func(model string) (int, string)) *provider.OpenAICompatible {
	t.Helper()
	return &provider.OpenAICompatible{
		BaseURL: "http://fixture.invalid/v1", APIKey: "fixture-key", Model: "legacy-default",
		Client: &http.Client{Transport: modelRoundTrip(func(req *http.Request) (*http.Response, error) {
			defer req.Body.Close()
			var body struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			status, content := reply(body.Model)
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(content))}, nil
		})},
	}
}
