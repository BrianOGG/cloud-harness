package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestC4OracleCostSnapshotSurvivesPriceChanges(t *testing.T) {
	events := []storage.Event{
		{Type: "oracle.asked", Payload: json.RawMessage(`{"model":"planner-call","input_tokens":100,"output_tokens":10,"cached_input_tokens":20,"cost_usd":0.0002,"cost_known":true}`)},
		{Type: "oracle.asked", Payload: json.RawMessage(`{"model":"planner-unknown","input_tokens":100,"output_tokens":10,"cost_usd":0,"cost_known":false}`)},
	}
	summary, err := SummarizeOracleEvents(events, map[string]config.ModelPrice{
		"planner-call":    {Input: 1000, Output: 1000, Cached: 1000},
		"planner-unknown": {Input: 1000, Output: 1000},
	})
	if err != nil || summary.CostUSD != 0.0002 || summary.CostKnown {
		t.Fatalf("summary=%+v error=%v", summary, err)
	}
}

func TestC4FallbackUsageUsesBackupModelAndPrice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		profile := &provider.ProfileProvider{
			Provider: providerFunc(func(_ context.Context, req provider.Request) (domain.ModelResponse, error) {
				calls++
				if req.Model == "primary" {
					return domain.ModelResponse{}, provider.NewHTTPStatusError(http.StatusTooManyRequests, "limited")
				}
				return withUsage(textOnly("backup response"), 100, 10, 0), nil
			}),
			Role: "executor", Model: "primary", FallbackModel: "backup",
		}
		resp, err := profile.Complete(t.Context(), provider.Request{})
		if err != nil || !resp.Complete || calls != 5 {
			t.Fatalf("response=%+v err=%v calls=%d", resp, err, calls)
		}
		manager := &Manager{ModelPrices: map[string]config.ModelPrice{
			"primary": {Input: 1000, Output: 1000},
			"backup":  {Input: 2, Output: 4},
		}}
		usage := manager.annotateUsage(profile, "legacy", resp)
		if usage.Model != "backup" || !usage.CostKnown || usage.CostUSD != 0.00024 {
			t.Fatalf("fallback usage=%+v", usage)
		}
	})
}

func TestC4UsageCostKnownDistinguishesUnknownAndZero(t *testing.T) {
	manager := &Manager{ModelPrices: map[string]config.ModelPrice{"free-model": {}}}
	response := withUsage(textOnly("ok"), 10, 2, 0)
	knownZero := manager.usageForModel("free-model", response)
	if !knownZero.CostKnown || knownZero.CostUSD != 0 {
		t.Fatalf("known zero cost=%+v", knownZero)
	}
	unknown := manager.usageForModel("unpriced-model", response)
	if unknown.CostKnown || unknown.CostUSD != 0 {
		t.Fatalf("unknown cost=%+v", unknown)
	}
}

func TestC4FailedCallUsageIsPersistedWithoutPartialContent(t *testing.T) {
	manager, db, _ := testManager(t, &provider.Mock{})
	manager.SkipPlanner = true
	manager.Model = "call-model"
	manager.ModelPrices = map[string]config.ModelPrice{"call-model": {Input: 2, Output: 4}}
	failure := domain.E(domain.CodeProviderHTTP, "upstream failed after usage")
	manager.Provider = providerFunc(func(context.Context, provider.Request) (domain.ModelResponse, error) {
		return withUsage(textOnly("partial response must not be stored"), 3, 2, 0), failure
	})
	run, err := manager.RunMessage(t.Context(), routingSession(t, manager), "exercise partial usage")
	if err != nil || run.Status != domain.RunFailed || run.Budget.UsedInputTokens != 3 || run.Budget.UsedOutputTokens != 2 {
		t.Fatalf("run=%+v error=%v", run, err)
	}
	if run.Budget.UsedCostUSD != 0.000014 {
		t.Fatalf("failed call cost was not charged: %+v", run.Budget)
	}
	rows, err := db.ListVisibleResponses(t.Context(), run.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("failed usage rows=%+v error=%v", rows, err)
	}
	if rows[0].Complete || rows[0].Usage.Model != "call-model" || !rows[0].Usage.CostKnown || rows[0].Usage.CostUSD != 0.000014 || len(rows[0].Items) != 0 {
		t.Fatalf("failed usage metadata=%+v", rows[0])
	}
}

func TestC4FastSummaryPersistsRoutedModelAndCost(t *testing.T) {
	manager, store, run := cp1SummaryBudgetRun(t, 4)
	manager.ModelPrices = map[string]config.ModelPrice{"fast-call": {Input: 2, Output: 4}}
	manager.ModelRouting = &config.ModelRouting{
		ModelProfiles: map[string]config.ModelProfile{"fast": {Model: "fast-call"}},
		ModelRoles:    config.ModelRoles{Fast: "fast"},
	}
	manager.Summarizer = providerFunc(func(context.Context, provider.Request) (domain.ModelResponse, error) {
		return withUsage(textOnly(`{"turn_summary":"summary","memory_delta":[]}`), 3, 2, 0), nil
	})
	ctx, err := manager.withModelRouting(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	job := storage.SummaryJob{
		ID: domain.NewID("sumjob_"), SessionID: run.SessionID, TurnID: run.TurnID, RunID: run.ID,
		Status: "pending", CreatedAt: manager.now(), UpdatedAt: manager.now(),
	}
	if err := store.SaveSummaryJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := manager.runModelSummary(ctx, run, job, "answer"); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListVisibleResponses(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Usage.Model == "fast-call" {
			if !row.Usage.CostKnown || row.Usage.CostUSD != 0.000014 {
				t.Fatalf("fast summary usage=%+v", row.Usage)
			}
			return
		}
	}
	t.Fatalf("fast profile usage not persisted: %+v", rows)
}
