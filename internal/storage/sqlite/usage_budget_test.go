package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestChargeUsageCountsChildUsageOnTheParent(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now().UTC()
	if err := db.CreateSession(ctx, storage.Session{ID: "ses", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	limited := domain.DefaultBudget()
	limited.MaxTokens, limited.MaxCostUSD = 1000, 1
	for _, run := range []storage.Run{
		{ID: "parent", SessionID: "ses", Status: domain.RunRunning, Budget: limited, Version: 1, CreatedAt: now, UpdatedAt: now},
		{ID: "child", ParentRunID: "parent", SessionID: "ses", Status: domain.RunRunning, Budget: limited, Version: 1, CreatedAt: now, UpdatedAt: now},
	} {
		if err := db.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ChargeUsage(ctx, "parent", domain.UsageCharge{InputTokens: 100, OutputTokens: 20, CachedInputTokens: 60, CostUSD: 0.2}); err != nil {
		t.Fatal(err)
	}
	// The child's call crosses both limits. It is recorded on both runs.
	if err := db.ChargeUsage(ctx, "child", domain.UsageCharge{InputTokens: 900, OutputTokens: 50, CostUSD: 0.9}); err != nil {
		t.Fatal(err)
	}
	parent, err := db.GetRun(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	child, err := db.GetRun(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if parent.Budget.UsedInputTokens != 1000 || parent.Budget.UsedOutputTokens != 70 || parent.Budget.UsedCachedInputTokens != 60 || parent.Budget.UsedCostUSD < 1.09 || parent.Budget.UsedCostUSD > 1.11 {
		t.Fatalf("parent budget=%+v", parent.Budget)
	}
	if child.Budget.UsedTokens() != 950 || child.Budget.UsedCostUSD != 0.9 {
		t.Fatalf("child budget=%+v", child.Budget)
	}
	if err := db.Charge(ctx, "parent", domain.ChargeModel, 1); !domain.IsCode(err, domain.CodeBudget) {
		t.Fatalf("parent model call past its limits: %v", err)
	}
	if err := db.ChargeUsage(ctx, "missing", domain.UsageCharge{InputTokens: 1}); !domain.IsCode(err, domain.CodeNotFound) {
		t.Fatalf("usage for a missing run: %v", err)
	}

	extended, err := db.ExtendBudget(ctx, "parent", domain.BudgetExtension{Tokens: 500, CostUSD: 1})
	if err != nil || extended.MaxTokens != 1500 || extended.MaxCostUSD != 2 {
		t.Fatalf("extended=%+v err=%v", extended, err)
	}
	if err := db.Charge(ctx, "parent", domain.ChargeModel, 1); err != nil {
		t.Fatalf("model call after the extension: %v", err)
	}
	if _, err := db.ExtendBudget(ctx, "parent", domain.BudgetExtension{}); !domain.IsCode(err, domain.CodeInvalidArgument) {
		t.Fatalf("empty extension accepted: %v", err)
	}
}
