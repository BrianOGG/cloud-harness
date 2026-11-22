package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
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
)

func TestStatsAndDoctorReportOracleCosts(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "oracle.db")
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_oracle", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, storage.Run{ID: "run_oracle", SessionID: "ses_oracle", Status: domain.RunCompleted, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	appendOracleTestEvent(t, db, "asked-a", "oracle.asked", `{"model":"planner-a","input_tokens":100,"output_tokens":20}`)
	appendOracleTestEvent(t, db, "asked-b", "oracle.asked", `{"model":"planner-b","input_tokens":1000,"output_tokens":0}`)
	appendOracleTestEvent(t, db, "failed", "oracle.failed", `{"reason":"budget"}`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"provider":"mock","model_prices":{"planner-a":{"input":1,"output":2,"cached":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setDiagnosticEnv(t, configPath, "")

	var out, stderr bytes.Buffer
	code := Main([]string{"stats", "--db", dbPath, "--by", "run", "--json"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("stats code=%d stderr=%s", code, stderr.String())
	}
	var report statsReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.OracleConsultations != 2 || report.OracleEstimatedCostUSD != 0.00014 || len(report.OracleByModel) != 2 {
		t.Fatalf("oracle stats = %+v", report)
	}
	if report.OracleByModel[0].Model != "planner-a" || report.OracleByModel[0].ModelCalls != 1 || report.OracleByModel[0].EstimatedCostUSD != 0.00014 {
		t.Fatalf("oracle model stats = %+v", report.OracleByModel)
	}
	if report.OracleByModel[1].Model != "planner-b" || report.OracleByModel[1].EstimatedCostUSD != 0 {
		t.Fatalf("unpriced oracle model stats = %+v", report.OracleByModel[1])
	}

	out.Reset()
	stderr.Reset()
	code = Main([]string{"stats", "--db", dbPath, "--by", "run"}, &out, &stderr)
	if code != 0 || !strings.Contains(out.String(), "ORACLE CONSULTATIONS\t2") || !strings.Contains(out.String(), "ORACLE ESTIMATED COST (USD)\t0.0001") {
		t.Fatalf("text stats code=%d out=%q stderr=%q", code, out.String(), stderr.String())
	}

	out.Reset()
	stderr.Reset()
	code = Main([]string{"doctor", "--db", dbPath, "--workspace", t.TempDir(), "--provider", "mock", "--json"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("doctor code=%d stderr=%s", code, stderr.String())
	}
	var checks []doctorItem
	if err := json.Unmarshal(out.Bytes(), &checks); err != nil {
		t.Fatal(err)
	}
	oracle := findDoctor(checks, "oracle")
	if oracle == nil || oracle.Oracle == nil || oracle.Oracle.Consultations != 2 || oracle.Oracle.EstimatedCostUSD != 0.00014 || oracle.Oracle.BudgetFailures != 1 {
		t.Fatalf("doctor oracle summary = %+v", oracle)
	}
	if !strings.Contains(oracle.Detail, "budget_failures=1") {
		t.Fatalf("doctor oracle detail = %q", oracle.Detail)
	}
	out.Reset()
	stderr.Reset()
	code = Main([]string{"doctor", "--db", dbPath, "--workspace", t.TempDir(), "--provider", "mock"}, &out, &stderr)
	if code != 0 || !strings.Contains(out.String(), "oracle: ok - consultations=2 estimated_cost_usd=0.0001 budget_failures=1") {
		t.Fatalf("text doctor code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
}

func TestStatsWithoutOracleUsesStableEmptyFields(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "empty-oracle.db")
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_no_oracle", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, storage.Run{ID: "run_no_oracle", SessionID: "ses_no_oracle", Status: domain.RunCompleted, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"provider":"mock"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setDiagnosticEnv(t, configPath, "")
	var out, stderr bytes.Buffer
	if code := Main([]string{"stats", "--db", dbPath, "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("stats code=%d stderr=%s", code, stderr.String())
	}
	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report["oracle_consultations"] != float64(0) || report["oracle_estimated_cost_usd"] != float64(0) {
		t.Fatalf("empty oracle scalar fields = %#v", report)
	}
	models, ok := report["oracle_by_model"].([]any)
	if !ok || len(models) != 0 {
		t.Fatalf("empty oracle model field = %#v", report["oracle_by_model"])
	}
	modelUsage, ok := report["model_usage_by_model"].([]any)
	if !ok || len(modelUsage) != 0 {
		t.Fatalf("empty all-run model usage field = %#v", report["model_usage_by_model"])
	}
	out.Reset()
	stderr.Reset()
	if code := Main([]string{"stats", "--db", dbPath}, &out, &stderr); code != 0 || !strings.Contains(out.String(), "ORACLE CONSULTATIONS\t0") || !strings.Contains(out.String(), "(none)\t0\t0.0000") {
		t.Fatalf("empty text stats code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
	out.Reset()
	stderr.Reset()
	if code := Main([]string{"doctor", "--db", dbPath, "--workspace", t.TempDir(), "--provider", "mock", "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("empty doctor code=%d stderr=%s", code, stderr.String())
	}
	var checks []doctorItem
	if err := json.Unmarshal(out.Bytes(), &checks); err != nil {
		t.Fatal(err)
	}
	oracle := findDoctor(checks, "oracle")
	if oracle == nil || oracle.Oracle == nil || oracle.Oracle.Consultations != 0 || oracle.Oracle.EstimatedCostUSD != 0 || oracle.Oracle.BudgetFailures != 0 {
		t.Fatalf("empty doctor oracle summary = %+v", oracle)
	}
}

func TestStatsIncludesAllRunModelUsageByModel(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "all-run-model-usage.db")
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_all_usage", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, storage.Run{ID: "run_all_usage", SessionID: "ses_all_usage", Status: domain.RunCompleted, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	responses := []storage.StoredResponse{
		{ID: "recent-priced", RunID: "run_all_usage", Provider: "openai", Complete: true, Usage: domain.Usage{InputTokens: 100, OutputTokens: 10, CachedInputTokens: 20}, CreatedAt: now},
		{ID: "old-priced", RunID: "run_all_usage", Provider: "openai", Complete: true, Usage: domain.Usage{InputTokens: 200, OutputTokens: 10, CachedInputTokens: 50}, CreatedAt: now.AddDate(0, 0, -90)},
		{ID: "recent-unpriced", RunID: "run_all_usage", Provider: "mock", Complete: true, Usage: domain.Usage{InputTokens: 50, OutputTokens: 10}, CreatedAt: now},
	}
	for _, response := range responses {
		if err := db.SaveModelResponse(ctx, response); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"provider":"mock","openai_model":"gpt-main","model_prices":{"gpt-main":{"input":1,"output":2,"cached":0.5}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setDiagnosticEnv(t, configPath, "")
	var out, stderr bytes.Buffer
	if code := Main([]string{"stats", "--db", dbPath, "--days", "1", "--by", "run", "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("stats code=%d stderr=%s", code, stderr.String())
	}
	var report statsReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Totals.ModelCalls != 2 || len(report.ModelUsageByModel) != 2 {
		t.Fatalf("windowed and all-run model usage = totals=%+v by_model=%+v", report.Totals, report.ModelUsageByModel)
	}
	priced := report.ModelUsageByModel[0]
	if priced.Model != "gpt-main" || priced.ModelCalls != 2 || priced.EstimatedCostUSD != 0.000305 {
		t.Fatalf("priced all-run usage = %+v", priced)
	}
	unpriced := report.ModelUsageByModel[1]
	if unpriced.Model != "mock" || unpriced.ModelCalls != 1 || unpriced.EstimatedCostUSD != 0 {
		t.Fatalf("unpriced all-run usage = %+v", unpriced)
	}

	out.Reset()
	stderr.Reset()
	if code := Main([]string{"stats", "--db", dbPath, "--days", "1", "--by", "run"}, &out, &stderr); code != 0 ||
		!strings.Contains(out.String(), "MODEL USAGE BY MODEL (ALL RUNS)") || !strings.Contains(out.String(), "gpt-main\t2\t0.0003") || !strings.Contains(out.String(), "mock\t1\t0.0000") {
		t.Fatalf("text stats code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
	}
}

func TestM1116ScriptedOracleRunMatchesStatsEventLedger(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "scripted-oracle.db")
	configPath := filepath.Join(t.TempDir(), "config.json")
	const inputPrice, outputPrice = 0.25, 0.5
	if err := os.WriteFile(configPath, []byte(`{"provider":"mock","model_prices":{"planner-scripted":{"input":0.25,"output":0.5,"cached":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setDiagnosticEnv(t, configPath, "")
	t.Setenv("CLOUD_HARNESS_ORACLE_MAX_COST_USD", "")

	const inputTokens, outputTokens = 1234, 234
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{
			ProviderCallID: "consult", Name: tools.ToolAskOracle, Arguments: json.RawMessage(`{"question":"Why is this fixture failing?"}`),
		}}}},
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{
			{Ordinal: 0, Kind: domain.ItemText, Text: "Check the fixture setup."},
			{Ordinal: 1, Kind: domain.ItemUsage, Usage: &domain.Usage{InputTokens: inputTokens, OutputTokens: outputTokens}},
		}},
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "The fixture is fixed."}}},
	}}
	db, err := sqlite.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	broker := tools.NewBroker(db, db)
	if err := tools.RegisterLocal(broker, root); err != nil {
		t.Fatal(err)
	}
	mgr := &agent.Manager{Store: db, Provider: mock, Broker: broker, Workspace: root, OwnerID: "m11-16", Model: "main-scripted"}
	mgr.SkipPlanner = true
	mgr.ModelRouting = &config.ModelRouting{
		ModelProfiles: map[string]config.ModelProfile{"planner-route": {Model: "planner-scripted"}},
		ModelRoles:    config.ModelRoles{Planner: "planner-route"},
	}
	session, err := mgr.CreateSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run, err := mgr.RunMessage(t.Context(), session.ID, "inspect this fixture")
	if err != nil || run.Status != domain.RunCompleted || len(mock.Requests) != 3 {
		t.Fatalf("scripted run=%+v requests=%d error=%v", run, len(mock.Requests), err)
	}
	if mock.Requests[1].Model != "planner-scripted" {
		t.Fatalf("oracle request model=%q", mock.Requests[1].Model)
	}
	events, err := db.ListEvents(t.Context(), run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var askedEvents []storage.Event
	for _, event := range events {
		if event.Type == "oracle.asked" {
			askedEvents = append(askedEvents, event)
		}
	}
	if len(askedEvents) != 1 {
		t.Fatalf("oracle.asked events = %d", len(askedEvents))
	}
	var asked struct {
		Model        string `json:"model"`
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
	}
	if err := json.Unmarshal(askedEvents[0].Payload, &asked); err != nil {
		t.Fatal(err)
	}
	eventCost := (float64(asked.InputTokens)*inputPrice + float64(asked.OutputTokens)*outputPrice) / 1_000_000

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := Main([]string{"stats", "--db", dbPath, "--json"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("stats code=%d stderr=%s", code, stderr.String())
	}
	var report statsReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.OracleConsultations != 1 || math.Abs(report.OracleEstimatedCostUSD-eventCost) > 1e-12 || len(report.OracleByModel) != 1 || report.OracleByModel[0].Model != asked.Model || math.Abs(report.OracleByModel[0].EstimatedCostUSD-eventCost) > 1e-12 {
		t.Fatalf("stats did not match event ledger: events=%+v event_cost=%.12f report=%+v", asked, eventCost, report)
	}
	t.Logf("event comparison: oracle.asked model=%s input_tokens=%d output_tokens=%d; (input_tokens*%.2f + output_tokens*%.2f)/1,000,000 = %.12f USD; stats oracle_consultations=%d oracle_estimated_cost_usd=%.12f; MATCH", asked.Model, asked.InputTokens, asked.OutputTokens, inputPrice, outputPrice, eventCost, report.OracleConsultations, report.OracleEstimatedCostUSD)
	t.Logf("stats command JSON: %s", strings.TrimSpace(out.String()))
}

func appendOracleTestEvent(t *testing.T, db *sqlite.DB, id, typ, payload string) {
	t.Helper()
	if _, err := db.AppendEvent(context.Background(), storage.Event{ID: "evt_" + id, RunID: "run_oracle", Type: typ, Payload: json.RawMessage(payload), Persistent: true, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
}
