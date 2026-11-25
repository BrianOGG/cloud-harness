package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/config"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

type cp1SummaryPath struct {
	name    string
	compact bool
	profile bool
}

var cp1SummaryPaths = []cp1SummaryPath{
	{name: "summary"},
	{name: "compact", compact: true},
	{name: "compact_fast", compact: true, profile: true},
}

func cp1SummaryBudgetRun(testT *testing.T, maximum int) (*Manager, storage.Store, storage.Run) {
	testT.Helper()
	manager, store, _ := testManager(testT, &provider.Mock{Responses: []domain.ModelResponse{textOnly("answer")}})
	manager.SkipPlanner = true
	manager.ModelRouting = &config.ModelRouting{}
	manager.ModelPrices = map[string]config.ModelPrice{}
	manager.UseBudget = true
	manager.InitialBudget = domain.DefaultBudget()
	manager.InitialBudget.MaxModelCalls = maximum
	session, err := manager.CreateSession(testT.Context())
	if err != nil {
		testT.Fatal(err)
	}
	run, err := manager.RunMessage(testT.Context(), session.ID, "question")
	if err != nil || run.Status != domain.RunCompleted {
		testT.Fatalf("run=%+v error=%v", run, err)
	}
	return manager, store, run
}

func (path cp1SummaryPath) call(ctx context.Context, manager *Manager, run storage.Run) error {
	if path.profile {
		manager.ModelRouting = &config.ModelRouting{
			ModelProfiles: map[string]config.ModelProfile{"summary": {Model: "fast-model"}},
			ModelRoles:    config.ModelRoles{Fast: "summary"},
		}
	}
	if path.compact {
		_, err := manager.CompactSession(ctx, run.SessionID)
		return err
	}
	_, _, _, err := manager.summarize(ctx, run, "answer")
	return err
}

func TestCP1SummaryCallsRespectBudget(testT *testing.T) {
	for _, path := range cp1SummaryPaths {
		for _, maximum := range []int{1, 2} {
			name := "exhausted"
			if maximum == 2 {
				name = "available"
			}
			testT.Run(path.name+"/"+name, func(testT *testing.T) {
				manager, store, run := cp1SummaryBudgetRun(testT, maximum)
				calls := 0
				manager.Summarizer = providerFunc(func(context.Context, provider.Request) (domain.ModelResponse, error) {
					calls++
					return textOnly(`{"turn_summary":"summary","memory_delta":[],"confirmed_facts":["fact"]}`), nil
				})
				err := path.call(testT.Context(), manager, run)
				if maximum == 1 && !domain.IsCode(err, domain.CodeBudget) || maximum == 2 && err != nil {
					testT.Fatalf("maximum=%d error=%v", maximum, err)
				}
				stored, err := store.GetRun(testT.Context(), run.ID)
				if err != nil || calls != maximum-1 || stored.Budget.UsedModelCalls != maximum {
					testT.Fatalf("calls=%d budget=%+v error=%v", calls, stored.Budget, err)
				}
			})
		}
	}
}

func TestCP1SummaryFailedAttemptsConsumeBudgetAndUsage(testT *testing.T) {
	for _, path := range cp1SummaryPaths {
		testT.Run(path.name, func(testT *testing.T) {
			manager, store, run := cp1SummaryBudgetRun(testT, 3)
			failure := errors.New("summary unavailable")
			calls := 0
			manager.Summarizer = providerFunc(func(context.Context, provider.Request) (domain.ModelResponse, error) {
				calls++
				return withUsage(textOnly("partial"), 3, 2, 0), failure
			})
			for range 2 {
				if err := path.call(testT.Context(), manager, run); !errors.Is(err, failure) {
					testT.Fatalf("error=%v", err)
				}
			}
			if err := path.call(testT.Context(), manager, run); !domain.IsCode(err, domain.CodeBudget) {
				testT.Fatalf("exhausted error=%v", err)
			}
			stored, err := store.GetRun(testT.Context(), run.ID)
			if err != nil || calls != 2 || stored.Budget.UsedModelCalls != 3 || stored.Budget.UsedInputTokens != 6 || stored.Budget.UsedOutputTokens != 4 {
				testT.Fatalf("calls=%d budget=%+v error=%v", calls, stored.Budget, err)
			}
		})
	}
}

type cp1SummaryChargeFailureStore struct {
	storage.Store
	failure error
}

func (store cp1SummaryChargeFailureStore) Charge(ctx context.Context, runID string, kind domain.ChargeKind, count int) error {
	if kind == domain.ChargeModel {
		return store.failure
	}
	return store.Store.Charge(ctx, runID, kind, count)
}

func TestCP1SummaryChargeFailureStopsProvider(testT *testing.T) {
	for _, path := range cp1SummaryPaths {
		testT.Run(path.name, func(testT *testing.T) {
			manager, store, run := cp1SummaryBudgetRun(testT, 2)
			failure := errors.New("budget ledger unavailable")
			manager.Store = cp1SummaryChargeFailureStore{Store: store, failure: failure}
			calls := 0
			manager.Summarizer = providerFunc(func(context.Context, provider.Request) (domain.ModelResponse, error) {
				calls++
				return textOnly(`{"turn_summary":"summary","memory_delta":[]}`), nil
			})
			if err := path.call(testT.Context(), manager, run); !errors.Is(err, failure) || calls != 0 {
				testT.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestCP1LocalSummaryDoesNotChargeModel(testT *testing.T) {
	manager, store, run := cp1SummaryBudgetRun(testT, 1)
	if _, _, _, err := manager.summarize(testT.Context(), run, "answer"); err != nil {
		testT.Fatal(err)
	}
	stored, err := store.GetRun(testT.Context(), run.ID)
	if err != nil || stored.Budget.UsedModelCalls != 1 {
		testT.Fatalf("budget=%+v error=%v", stored.Budget, err)
	}
}
