package main

import (
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
)

func TestC4ModelUsageUsesPersistedCallMetadata(t *testing.T) {
	now := time.Now().UTC()
	records := []sqlite.UsageRecord{
		{
			Provider:  "openai",
			CreatedAt: now,
			Usage: domain.Usage{
				Model:       "gpt-primary",
				InputTokens: 100,
				CostUSD:     0.0002,
				CostKnown:   true,
			},
		},
		{
			Provider:  "openai",
			CreatedAt: now,
			Usage: domain.Usage{
				Model:       "gpt-backup",
				InputTokens: 100,
				CostUSD:     0,
				CostKnown:   true,
			},
		},
		{
			Provider:  "openai",
			CreatedAt: now,
			Usage:     domain.Usage{InputTokens: 100},
		},
	}

	rows := aggregateModelUsage(records)
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	got := map[string]modelUsageStats{}
	for _, row := range rows {
		got[row.Model] = row
	}
	if got["gpt-primary"].EstimatedCostUSD != 0.0002 || !got["gpt-primary"].CostKnown {
		t.Fatalf("primary attribution = %+v", got["gpt-primary"])
	}
	if got["gpt-backup"].EstimatedCostUSD != 0 || !got["gpt-backup"].CostKnown {
		t.Fatalf("zero-cost attribution = %+v", got["gpt-backup"])
	}
	if got["unknown"].CostKnown || got["unknown"].EstimatedCostUSD != 0 {
		t.Fatalf("legacy attribution = %+v", got["unknown"])
	}
}
