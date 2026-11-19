package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/storage"
)

func TestListRunsByStatusAcrossSessionsWithoutLimit(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, session := range []string{"ses_a", "ses_b"} {
		if err := db.CreateSession(ctx, storage.Session{ID: session, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 206; i++ {
		session := "ses_a"
		if i%2 == 1 {
			session = "ses_b"
		}
		status, reason := domain.RunWaiting, domain.WaitUser
		if i == 0 {
			status, reason = domain.RunCompleted, domain.WaitNone
		}
		if err := db.CreateRun(ctx, storage.Run{ID: fmt.Sprintf("run_%03d", i),
			SessionID: session, Status: status, WaitReason: reason, Version: 1,
			Budget: domain.DefaultBudget(), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := db.ListRunsByStatus(ctx, domain.RunWaiting)
	if err != nil || len(runs) != 205 {
		t.Fatalf("expected all 205 waiting runs, got %d (%v)", len(runs), err)
	}
	for i, run := range runs {
		if run.ID != fmt.Sprintf("run_%03d", i+1) || run.Status != domain.RunWaiting {
			t.Fatalf("status query returned unexpected run: %+v", run)
		}
	}
	if runs[0].SessionID == runs[1].SessionID {
		t.Fatal("status query missed a session")
	}
	if empty, err := db.ListRunsByStatus(ctx, domain.RunFailed); err != nil || len(empty) != 0 {
		t.Fatalf("empty status query: %+v (%v)", empty, err)
	}
}
