package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestC4UsageMetadataSurvivesReopenAndLegacyRowsStayUnknown(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.CreateSession(ctx, storage.Session{ID: "ses_c4", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateRun(ctx, storage.Run{ID: "run_c4", SessionID: "ses_c4", Status: domain.RunRunning, Budget: domain.DefaultBudget(), Version: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, record := range []storage.StoredResponse{
		{ID: "priced", RunID: "run_c4", Provider: "openai", Complete: true, Usage: domain.Usage{InputTokens: 100, Model: "call-model", CostUSD: 0.0002, CostKnown: true}, CreatedAt: now},
		{ID: "zero", RunID: "run_c4", Provider: "openai", Complete: true, Usage: domain.Usage{InputTokens: 100, Model: "free-model", CostKnown: true}, CreatedAt: now},
		{ID: "legacy", RunID: "run_c4", Provider: "openai", Complete: true, Usage: domain.Usage{InputTokens: 100}, CreatedAt: now},
	} {
		if err := db.SaveModelResponse(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err := ReadUsage(ctx, path, now.Add(-time.Minute))
	if err != nil || len(rows) != 3 {
		t.Fatalf("usage rows=%+v error=%v", rows, err)
	}
	byID := make(map[string]domain.Usage, len(rows))
	for _, row := range rows {
		byID[row.ID] = row.Usage
	}
	if byID["priced"].Model != "call-model" || byID["priced"].CostUSD != 0.0002 || !byID["priced"].CostKnown {
		t.Fatalf("priced metadata=%+v", byID["priced"])
	}
	if byID["zero"].Model != "free-model" || byID["zero"].CostUSD != 0 || !byID["zero"].CostKnown {
		t.Fatalf("zero metadata=%+v", byID["zero"])
	}
	if byID["legacy"].Model != "" || byID["legacy"].CostKnown {
		t.Fatalf("legacy metadata was inferred: %+v", byID["legacy"])
	}
}
