package agent

import (
	"context"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
)

func TestReportedZeroUsageSurvivesFailureAndBudgetCharge(t *testing.T) {
	m, db, _ := testManager(t, &provider.Mock{})
	m.SkipPlanner = true
	m.Provider = providerFunc(func(context.Context, provider.Request) (domain.ModelResponse, error) {
		return withUsageBreakdown(textOnly("partial content"), 0, 0, 0, 0, 0, true, true), domain.E(domain.CodeProviderHTTP, "failed after usage")
	})
	run, err := m.RunMessage(t.Context(), routingSession(t, m), "exercise reported zero usage")
	if err != nil || run.Status != domain.RunFailed || !run.Budget.CacheReported || !run.Budget.ReasoningReported {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	rows, err := db.ListVisibleResponses(t.Context(), run.ID)
	if err != nil || len(rows) != 1 || !rows[0].Usage.CacheReported || !rows[0].Usage.ReasoningReported || len(rows[0].Items) != 0 {
		t.Fatalf("failed response usage=%+v err=%v", rows, err)
	}
}

func TestCompletedEventIncludesReportedZeroBreakdown(t *testing.T) {
	m, db, _ := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{
		withUsageBreakdown(textOnly("done"), 0, 0, 0, 0, 0, true, true),
	}})
	m.SkipPlanner = true
	run, err := m.RunMessage(t.Context(), routingSession(t, m), "report zero counts")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	events := eventPayloads(t, db, run.ID, "model.completed")
	if len(events) != 1 || events[0]["cache_creation_input_tokens"] != float64(0) || events[0]["reasoning_tokens"] != float64(0) {
		t.Fatalf("model.completed=%v", events)
	}
}

func TestOracleEventUsageBreakdownPresence(t *testing.T) {
	for _, reported := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "reported zero"}[reported], func(t *testing.T) {
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(oracleCall(t, "consult", "what next?")),
				withUsageBreakdown(textOnly("advice"), 12, 5, 0, 0, 0, reported, reported),
				textOnly("done"),
			}}
			m, db, _ := testManager(t, mock)
			m.SkipPlanner = true
			m.Model, m.ModelRouting = "main-model", oracleRouting()
			run := completedOracleRun(t, m, "consult")
			events := oracleEventPayloads(t, db, run.ID, "oracle.asked")
			if len(events) != 1 {
				t.Fatalf("oracle.asked=%v", events)
			}
			for _, key := range []string{"cache_creation_input_tokens", "reasoning_tokens"} {
				value, exists := events[0][key]
				if exists != reported || reported && value != float64(0) {
					t.Fatalf("%s=%v exists=%v reported=%v", key, value, exists, reported)
				}
			}
		})
	}
}
