package agent

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

func TestAskOracleReturnsOnlySanitizedConsultationAdvice(t *testing.T) {
	const (
		question = "这个缓存键为什么会冲突？"
		advice   = "Include the tenant id in the cache key."
		prefix   = `api_key := "named-fixture-secret"` + "\n" +
			`Authorization: Bearer fixtureBearer123` + "\n" +
			`client("sk-LongFakeToken0123456789ABCDEF")` + "\n"
	)
	code := prefix + strings.Repeat("界", (8180-len(prefix))/3) + `"sk-NearCutoffFixture0123456789-tail"` + "OMITTED_CODE"
	background := "Cache context\n" + strings.Repeat("x", 4<<10) + "OMITTED_CONTEXT"
	call := oracleCall(t, "consult", question)
	call.Arguments = must(t, map[string]string{"question": question, "code": code, "context": background})
	consultation := textOnly(advice)
	consultation.Items = append(consultation.Items,
		domain.ResponseItem{Ordinal: 1, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{
			ProviderCallID: "unwanted", Name: "write_file", Arguments: json.RawMessage(`{"path":"oracle.txt","content":"should not write"}`),
		}},
		domain.ResponseItem{Ordinal: 2, Kind: domain.ItemUsage, Usage: &domain.Usage{InputTokens: 123, OutputTokens: 27, CachedInputTokens: 11}},
	)
	mock := &provider.Mock{Responses: []domain.ModelResponse{toolResp(call), consultation, textOnly("main model's answer")}}
	m, db, root := testManager(t, mock)
	m.SkipPlanner, m.Model = true, "default-model"
	m.ModelRouting = oracleRouting()
	run := completedOracleRun(t, m, "private main thread")
	if run.FinalAnswer != "main model's answer" || run.Budget.UsedModelCalls != 3 || len(mock.Requests) != 3 {
		t.Fatalf("run = %+v, model requests = %d", run, len(mock.Requests))
	}
	if fact, ok := m.Broker.Lookup(tools.ToolAskOracle); !ok || !fact.Readonly || fact.SideEffect != "none" || fact.Pollable {
		t.Fatalf("oracle broker fact = %+v, found = %t", fact, ok)
	}
	if !hasOracleTool(mock.Requests[0]) {
		t.Fatal("main model was not offered ask_oracle")
	}
	req := mock.Requests[1]
	if req.Model != "planner-primary" || len(req.Messages) != 2 || len(req.Tools) != 0 || len(req.Continuation) != 0 || len(req.Prefix) != 0 {
		t.Fatalf("unexpected consultation request: model=%q, messages=%d, tools=%d, continuation=%d, prefix=%d",
			req.Model, len(req.Messages), len(req.Tools), len(req.Continuation), len(req.Prefix))
	}
	text := req.Messages[1].Content
	_, snippet, _ := strings.Cut(text, "\n\nCode snippet:\n")
	snippet, contextText, _ := strings.Cut(snippet, "\n\nContext:\n")
	if !strings.Contains(text, question) || len(snippet) > 8<<10 || !utf8.ValidString(snippet) ||
		!strings.HasSuffix(snippet, "[code truncated at 8 KiB]") || !strings.Contains(snippet, "[redacted]") ||
		len(contextText) > 4<<10 || !strings.HasPrefix(contextText, "Cache context") || !strings.HasSuffix(contextText, "[context truncated at 4 KiB]") {
		t.Fatalf("incorrect consultation prompt, code bytes=%d, context bytes=%d", len(snippet), len(contextText))
	}
	for _, secret := range []string{"named-fixture-secret", "fixtureBearer123", "sk-LongFakeToken", "sk-NearCutoff", "OMITTED_CODE", "OMITTED_CONTEXT", "private main thread"} {
		if strings.Contains(text, secret) {
			t.Errorf("consultation included %q", secret)
		}
	}
	var result string
	for _, msg := range mock.Requests[2].Messages {
		if msg.Role == "tool" && msg.ToolCallID == "consult" {
			result = msg.Content
		}
	}
	if !strings.Contains(result, `"answer":"`+advice+`"`) || !strings.Contains(result, `"untrusted":true`) || strings.Contains(result, "write_file") {
		t.Errorf("main model's tool result = %s", result)
	}
	if _, err := os.Stat(filepath.Join(root, "oracle.txt")); !os.IsNotExist(err) {
		t.Fatalf("consultation triggered a write: %v", err)
	}
	rows, err := db.ListVisibleResponses(t.Context(), run.ID)
	if err != nil || len(rows) != 3 || rows[1].Usage.InputTokens != 123 || rows[1].Usage.OutputTokens != 27 || len(rows[1].Items) != 0 {
		t.Fatalf("stored model usage: %d rows, error = %v", len(rows), err)
	}
	asked := oracleEventPayloads(t, db, run.ID, "oracle.asked")
	if len(asked) != 1 || len(asked[0]) != 9 || asked[0]["model"] != "planner-primary" ||
		asked[0]["question_length"] != float64(utf8.RuneCountInString(question)) || asked[0]["code_bytes"] != float64(len(snippet)) ||
		asked[0]["input_tokens"] != float64(123) || asked[0]["output_tokens"] != float64(27) || asked[0]["cached_input_tokens"] != float64(11) || asked[0]["latency_ms"] == nil {
		t.Fatalf("oracle.asked = %+v", asked)
	}
}

func TestAskOracleCostLimitStopsNextConsultationBeforeRequest(t *testing.T) {
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(oracleCall(t, "first", "first"), oracleCall(t, "second", "second")),
		usageText("advice", 100, 50), textOnly("done"),
	}}
	m, db, _ := testManager(t, mock)
	m.Model, m.ModelRouting = "main-model", oracleRouting()
	setOracleCostConfig(t, 0.00005, map[string]config.ModelPrice{"planner-primary": {Input: 0.25, Output: 0.5}})
	run, err := m.RunMessage(t.Context(), routingSession(t, m), "consult twice")
	if err != nil || run.Status != domain.RunCompleted || countOracleModelRequests(mock, "planner-primary") != 1 || len(mock.Requests) != 3 {
		t.Fatalf("run = %+v, error = %v, provider requests = %d", run, err, len(mock.Requests))
	}
	cause := oracleBudgetError(t, db, run.ID, "second")
	if !strings.Contains(cause.Message, "limit $0.00005") || !strings.Contains(cause.Message, "accumulated cost $0.00005") {
		t.Errorf("cost budget error = %v", cause)
	}
	if asked := oracleEventPayloads(t, db, run.ID, "oracle.asked"); len(asked) != 1 {
		t.Errorf("oracle.asked = %+v", asked)
	}
	assertOracleFailure(t, db, run.ID, "budget")
}

func TestAskOracleNoCostLimitRetainsThreeConsultationLimit(t *testing.T) {
	calls := []domain.ToolCall{oracleCall(t, "first", "one"), oracleCall(t, "second", "two"), oracleCall(t, "third", "three"), oracleCall(t, "fourth", "four")}
	mock := &provider.Mock{Responses: []domain.ModelResponse{toolResp(calls...), textOnly("one"), textOnly("two"), textOnly("three"), textOnly("done")}}
	m, db, _ := testManager(t, mock)
	m.Model, m.ModelRouting = "main-model", oracleRouting()
	setOracleCostConfig(t, 0, nil)
	run, err := m.RunMessage(t.Context(), routingSession(t, m), "consult several times")
	if err != nil || run.Status != domain.RunCompleted || countOracleModelRequests(mock, "planner-primary") != 3 {
		t.Fatalf("run = %+v, error = %v, provider requests = %d", run, err, len(mock.Requests))
	}
	if cause := oracleBudgetError(t, db, run.ID, "fourth"); !strings.Contains(cause.Message, "3 consultations per run") {
		t.Errorf("fourth consultation = %v", cause)
	}
	if asked := oracleEventPayloads(t, db, run.ID, "oracle.asked"); len(asked) != 3 {
		t.Errorf("oracle.asked = %+v", asked)
	}
	assertOracleFailure(t, db, run.ID, "budget_exhausted")
}

func TestAskOracleUnpricedModelDoesNotAccumulateCost(t *testing.T) {
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(oracleCall(t, "first", "first"), oracleCall(t, "second", "second")),
		usageText("advice", 10_000_000, 10_000_000), textOnly("done"),
	}}
	m, db, _ := testManager(t, mock)
	m.SkipPlanner = true
	m.ModelRouting = &config.ModelRouting{}
	m.Model = "unpriced-model"
	setOracleCostConfig(t, 0.000000001, map[string]config.ModelPrice{"priced-model": {Input: 0.5, Output: 1}})
	run, err := m.RunMessage(t.Context(), routingSession(t, m), "consult twice")
	if err != nil || run.Status != domain.RunCompleted || len(mock.Requests) != 4 {
		t.Fatalf("run = %+v, error = %v, provider requests = %d", run, err, len(mock.Requests))
	}
	if asked := oracleEventPayloads(t, db, run.ID, "oracle.asked"); len(asked) != 2 {
		t.Errorf("oracle.asked = %+v", asked)
	}
	if failed := oracleEventPayloads(t, db, run.ID, "oracle.failed"); len(failed) != 0 {
		t.Errorf("unexpected oracle.failed = %+v", failed)
	}
}

func TestAskOracleCostLimitRecomputesUsageAfterResume(t *testing.T) {
	ask := domain.ToolCall{ProviderCallID: "pause", Name: domain.ToolAskUser, Arguments: json.RawMessage(`{"question":"Continue?"}`)}
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(oracleCall(t, "first", "first")),
		usageText("advice", 100, 50),
		toolResp(ask),
	}}
	m, db, root := testManager(t, mock)
	m.Model, m.ModelRouting = "main-model", oracleRouting()
	setOracleCostConfig(t, 0.00005, map[string]config.ModelPrice{"planner-primary": {Input: 0.25, Output: 0.5}})
	run, err := m.RunMessage(t.Context(), routingSession(t, m), "resume after an oracle consultation")
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitUser || len(mock.Requests) != 3 {
		t.Fatalf("run = %+v, error = %v, provider requests = %d", run, err, len(mock.Requests))
	}
	if asked := oracleEventPayloads(t, db, run.ID, "oracle.asked"); len(asked) != 1 {
		t.Fatalf("pre-resume oracle.asked = %+v", asked)
	}
	resumedProvider := &provider.Mock{Responses: []domain.ModelResponse{toolResp(oracleCall(t, "resumed", "check after resume")), textOnly("done")}}
	resumer := &Manager{Store: db, Broker: tools.NewBroker(db, db), Workspace: root, Provider: resumedProvider,
		Model: "main-model", ModelRouting: oracleRouting()}
	run, err = resumer.Resume(t.Context(), run.ID, ResumeInput{Answer: "continue"})
	if err != nil || run.Status != domain.RunCompleted || len(resumedProvider.Requests) != 2 || countOracleModelRequests(resumedProvider, "planner-primary") != 0 {
		t.Fatalf("resumed run = %+v, error = %v, provider requests = %d", run, err, len(resumedProvider.Requests))
	}
	if cause := oracleBudgetError(t, db, run.ID, "resumed"); !strings.Contains(cause.Message, "accumulated cost $0.00005") {
		t.Errorf("resumed consultation = %v", cause)
	}
	assertOracleFailure(t, db, run.ID, "budget")
}

func usageText(text string, inputTokens, outputTokens int) domain.ModelResponse {
	resp := textOnly(text)
	resp.Items = append(resp.Items, domain.ResponseItem{Ordinal: 1, Kind: domain.ItemUsage, Usage: &domain.Usage{InputTokens: inputTokens, OutputTokens: outputTokens}})
	return resp
}

func setOracleCostConfig(t *testing.T, limit float64, prices map[string]config.ModelPrice) {
	t.Helper()
	fields := map[string]any{"model_prices": prices}
	if limit > 0 {
		fields["oracle_max_cost_usd"] = limit
	}
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "harness.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUD_HARNESS_CONFIG", path)
	t.Setenv("CLOUD_HARNESS_ORACLE_MAX_COST_USD", "")
}

func countOracleModelRequests(mock *provider.Mock, model string) int {
	count := 0
	for _, request := range mock.Requests {
		if request.Model == model {
			count++
		}
	}
	return count
}

func TestAskOracleRunLimitSurvivesRewindAndResume(t *testing.T) {
	ask := domain.ToolCall{ProviderCallID: "pause", Name: domain.ToolAskUser, Arguments: json.RawMessage(`{"question":"Continue?"}`)}
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(oracleCall(t, "o1", "first"), oracleCall(t, "o2", "second"), oracleCall(t, "o3", "third")),
		textOnly("one"), textOnly("two"), textOnly("three"), toolResp(ask),
	}}
	m, db, root := testManager(t, mock)
	m.SkipPlanner, m.Model = true, "default-model"
	m.ModelRouting = &config.ModelRouting{}
	run, err := m.RunMessage(t.Context(), routingSession(t, m), "consult and pause")
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitUser || len(mock.Requests) != 5 {
		t.Fatalf("waiting run = %+v, error = %v, requests = %d", run, err, len(mock.Requests))
	}
	if got := mock.Requests[1].Model; got != "default-model" {
		t.Errorf("consultation without a profile requested %q", got)
	}
	if asked := oracleEventPayloads(t, db, run.ID, "oracle.asked"); len(asked) != 3 {
		t.Fatalf("oracle.asked = %+v", asked)
	}
	events, err := db.ListEvents(t.Context(), run.ID, 0)
	if err != nil || len(events) == 0 || events[0].Type != "run.created" {
		t.Fatalf("run events = %+v, error = %v", events, err)
	}
	if rewound, err := db.RewindChat(t.Context(), run.ID, events[0].Seq); err != nil || rewound.DeletedEvents == 0 {
		t.Fatalf("rewind = %+v, error = %v", rewound, err)
	}
	resumed := &provider.Mock{Responses: []domain.ModelResponse{toolResp(oracleCall(t, "o4", "fourth")), textOnly("done")}}
	resumer := &Manager{Store: db, Broker: tools.NewBroker(db, db), Workspace: root, Provider: resumed, ModelRouting: &config.ModelRouting{}}
	run, err = resumer.Resume(t.Context(), run.ID, ResumeInput{Answer: "yes"})
	if err != nil || run.Status != domain.RunCompleted || len(resumed.Requests) != 2 || run.Budget.UsedModelCalls != 7 {
		t.Fatalf("resumed run = %+v, error = %v, requests = %d", run, err, len(resumed.Requests))
	}
	if cause := oracleBudgetError(t, db, run.ID, "o4"); !strings.Contains(cause.Message, "3 consultations per run") {
		t.Errorf("fourth consultation = %v", cause)
	}
	if asked := oracleEventPayloads(t, db, run.ID, "oracle.asked"); len(asked) != 0 {
		t.Errorf("oracle.asked = %+v", asked)
	}
	assertOracleFailure(t, db, run.ID, "budget_exhausted")
}

func TestAskOracleExhaustedModelBudgetSendsNoRequest(t *testing.T) {
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(oracleCall(t, "fits", "first"), oracleCall(t, "blocked", "second")), textOnly("advice"),
	}}
	m, db, _ := testManager(t, mock)
	m.SkipPlanner = true
	m.ModelRouting = oracleRouting()
	m.UseBudget = true
	m.InitialBudget = domain.DefaultBudget()
	m.InitialBudget.MaxModelCalls = 2 // One main call, one consultation.
	run, err := m.RunMessage(t.Context(), routingSession(t, m), "only one consultation fits")
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitBudget || run.Budget.UsedModelCalls != 2 ||
		len(mock.Requests) != 2 || mock.Requests[1].Model != "planner-primary" {
		t.Fatalf("run = %+v, error = %v, requests = %d", run, err, len(mock.Requests))
	}
	if cause := oracleBudgetError(t, db, run.ID, "blocked"); cause.Message != "model call budget exhausted" {
		t.Errorf("blocked consultation = %v", cause)
	}
	if asked := oracleEventPayloads(t, db, run.ID, "oracle.asked"); len(asked) != 1 {
		t.Errorf("oracle.asked = %+v", asked)
	}
	assertOracleFailure(t, db, run.ID, "budget_exhausted")
}

func TestAskOracleInBatchResumedAfterApproval(t *testing.T) {
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(snapshotWrite("write", "approved.txt", "ok"), oracleCall(t, "oracle", "ask after approval")),
	}}
	m, db, root := testManager(t, mock)
	m.SkipPlanner = true
	m.ModelRouting = oracleRouting()
	run, err := m.RunMessage(t.Context(), routingSession(t, m), "write and consult")
	if err != nil || run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval || len(mock.Requests) != 1 {
		t.Fatalf("waiting run = %+v, error = %v, requests = %d", run, err, len(mock.Requests))
	}
	resumed := &provider.Mock{Responses: []domain.ModelResponse{textOnly("approval advice"), textOnly("done")}}
	resumer := &Manager{Store: db, Broker: tools.NewBroker(db, db), Workspace: root, Provider: resumed, ModelRouting: oracleRouting()}
	run, err = resumer.Resume(t.Context(), run.ID, ResumeInput{Decision: "approve"})
	if err != nil || run.Status != domain.RunCompleted || run.Budget.UsedModelCalls != 3 || len(resumed.Requests) != 2 || resumed.Requests[0].Model != "planner-primary" {
		t.Fatalf("resumed run = %+v, error = %v, requests = %d", run, err, len(resumed.Requests))
	}
	if asked := oracleEventPayloads(t, db, run.ID, "oracle.asked"); len(asked) != 1 {
		t.Errorf("oracle.asked = %+v", asked)
	}
}

func TestAskOracleUsesPlannerFallbackOnce(t *testing.T) {
	t.Parallel()
	m, db, _ := testManager(t, &provider.Mock{})
	m.SkipPlanner = true
	m.ModelRouting = oracleRouting()
	m.ModelRouting.ModelRoles.Executor = m.ModelRouting.ModelRoles.Planner
	var models []string
	m.Provider = providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
		models = append(models, req.Model)
		if len(req.Tools) != 0 {
			if len(models) == 1 {
				return toolResp(oracleCall(t, "o1", "first"), oracleCall(t, "o2", "second")), nil
			}
			return textOnly("main answer"), nil
		}
		if req.Model == "planner-primary" {
			return domain.ModelResponse{}, provider.NewHTTPStatusError(http.StatusTooManyRequests, "private upstream body")
		}
		return textOnly("backup advice"), nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	run, err := m.RunMessage(ctx, routingSession(t, m), "use backup")
	want := []string{"planner-primary", "planner-primary", "planner-primary", "planner-primary", "planner-primary", "planner-backup", "planner-backup", "planner-backup"}
	if err != nil || run.Status != domain.RunCompleted || !slices.Equal(models, want) || run.Budget.UsedModelCalls != len(want) {
		t.Fatalf("run = %+v, error = %v, call models = %v", run, err, models)
	}
	events := fallbackPayloads(t, db, run.ID)
	if len(events) != 1 || !maps.Equal(events[0], map[string]string{
		"role": "planner", "from_model": "planner-primary", "to_model": "planner-backup", "reason": "rate_limited",
	}) {
		t.Errorf("fallback events = %+v", events)
	}
	asked := oracleEventPayloads(t, db, run.ID, "oracle.asked")
	if len(asked) != 2 || asked[0]["model"] != "planner-backup" || asked[1]["model"] != "planner-backup" {
		t.Errorf("oracle.asked = %+v", asked)
	}
}

func TestAskOracleFailuresOmitProviderBody(t *testing.T) {
	for _, tc := range []struct {
		reason string
		err    error
	}{
		{"upstream_error", provider.NewHTTPStatusError(http.StatusBadRequest, "upstream echoed private question")},
		{"timeout", context.DeadlineExceeded},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			m, db, _ := testManager(t, &provider.Mock{})
			m.SkipPlanner = true
			m.ModelRouting = oracleRouting()
			calls := 0
			m.Provider = providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
				calls++
				if calls == 1 {
					return toolResp(oracleCall(t, "private", "private question")), nil
				}
				if len(req.Tools) == 0 {
					return domain.ModelResponse{}, tc.err
				}
				for _, message := range req.Messages {
					if message.Role == "tool" && (strings.Contains(message.Content, "private question") || !strings.Contains(message.Content, `"ok":false`)) {
						t.Errorf("tool result = %s", message.Content)
					}
				}
				return textOnly("continued"), nil
			})
			run := completedOracleRun(t, m, "try oracle")
			if calls != 3 || run.Budget.UsedModelCalls != calls {
				t.Fatalf("run = %+v, calls = %d", run, calls)
			}
			assertOracleFailure(t, db, run.ID, tc.reason)
			if asked := oracleEventPayloads(t, db, run.ID, "oracle.asked"); len(asked) != 1 {
				t.Errorf("oracle.asked = %+v", asked)
			}
		})
	}
}

func TestAskOracleExcludedFromChildToolIntersection(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"default", `{"goal":"read code"}`},
		{"intersection", `{"goal":"read code","agent":"custom","allowed_tools":["read_file","ask_oracle"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := mockAgentDelegate(tc.args, toolResp(oracleCall(t, "disallowed", "try from child")), textOnly("child done"))
			m, db, workspace := testManager(t, mock)
			m.SkipPlanner = true
			m.ModelRouting = oracleRouting()
			if tc.name == "intersection" {
				writeDelegateDefinition(t, workspace, "custom", "tools: [read_file]\n", "")
			}
			parent := runAgentDelegate(t, m)
			child := delegateChildRun(t, db, parent)
			if len(mock.Requests) != 4 || !hasOracleTool(mock.Requests[0]) || hasOracleTool(mock.Requests[1]) {
				t.Fatalf("oracle exposure in parent/child requests, request count = %d", len(mock.Requests))
			}
			if tc.name == "intersection" {
				var names []string
				for _, tool := range mock.Requests[1].Tools {
					names = append(names, tool.Name)
				}
				if !slices.Equal(names, []string{"finish_turn", "read_file"}) {
					t.Errorf("child tool intersection = %v", names)
				}
			}
			grants, err := db.ListGrants(t.Context(), child.ID, child.SessionID, m.now())
			if err != nil || slices.ContainsFunc(grants, func(grant domain.Grant) bool { return grant.Tool == tools.ToolAskOracle }) {
				t.Fatalf("child grants = %+v, error = %v", grants, err)
			}
			batches, err := db.LoadBatches(t.Context(), child.ID)
			if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 1 || batches[0].Invocations[0].Status != domain.InvDenied {
				t.Fatalf("child batches = %+v, error = %v", batches, err)
			}
		})
	}
}

func oracleRouting() *config.ModelRouting {
	return &config.ModelRouting{
		ModelProfiles: map[string]config.ModelProfile{"consult": {Model: "planner-primary", FallbackModel: "planner-backup"}},
		ModelRoles:    config.ModelRoles{Planner: "consult"},
	}
}

func oracleCall(t *testing.T, id, question string) domain.ToolCall {
	t.Helper()
	return domain.ToolCall{ProviderCallID: id, Name: tools.ToolAskOracle, Arguments: must(t, map[string]string{"question": question})}
}

func hasOracleTool(req provider.Request) bool {
	return slices.ContainsFunc(req.Tools, func(tool provider.ToolDefinition) bool { return tool.Name == tools.ToolAskOracle })
}

func completedOracleRun(t *testing.T, m *Manager, text string) storage.Run {
	t.Helper()
	run, err := m.RunMessage(t.Context(), routingSession(t, m), text)
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run = %+v, error = %v", run, err)
	}
	return run
}

func oracleEventPayloads(t *testing.T, db storage.Store, runID, kind string) []map[string]any {
	t.Helper()
	events, err := db.ListEvents(t.Context(), runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var payloads []map[string]any
	for _, event := range events {
		if event.Type == kind {
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			payloads = append(payloads, payload)
		}
	}
	return payloads
}

func assertOracleFailure(t *testing.T, db storage.Store, runID, reason string) {
	t.Helper()
	failed := oracleEventPayloads(t, db, runID, "oracle.failed")
	if len(failed) != 1 || len(failed[0]) != 1 || failed[0]["reason"] != reason {
		t.Fatalf("oracle.failed = %+v, want %s", failed, reason)
	}
}

func oracleBudgetError(t *testing.T, db storage.Store, runID, callID string) *domain.Error {
	t.Helper()
	batches, err := db.LoadBatches(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, batch := range batches {
		for _, inv := range batch.Invocations {
			if inv.ProviderCallID == callID && inv.Result != nil && inv.Result.Error != nil && domain.IsCode(inv.Result.Error, domain.CodeBudget) {
				return inv.Result.Error
			}
		}
	}
	t.Fatalf("missing budget error for %s", callID)
	return nil
}
