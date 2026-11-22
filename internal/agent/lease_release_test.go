package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// leaseHeld reports whether a lease on the run is still stored: another owner
// can take the lease only when none is.
func leaseHeld(t *testing.T, m *Manager, runID string) bool {
	t.Helper()
	now := m.now()
	_, err := m.Store.AcquireLease(context.Background(), runID, "probe", now, now.Add(time.Minute))
	if err == nil {
		if err := m.Store.ReleaseLease(context.Background(), runID, "probe"); err != nil {
			t.Fatal(err)
		}
		return false
	}
	if !domain.IsCode(err, domain.CodeLease) {
		t.Fatal(err)
	}
	return true
}

func answering() *blockingProvider {
	return newBlockingProvider(func(int) *domain.ModelResponse {
		return &domain.ModelResponse{Provider: "blocking", Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemText, Text: "done"}}}
	})
}

func TestRunReleasesItsLeaseWhenItEnds(t *testing.T) {
	m, session := backgroundManager(t, answering())
	run, err := m.RunMessage(t.Context(), session.ID, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunCompleted {
		t.Fatalf("status = %s", run.Status)
	}
	if leaseHeld(t, m, run.ID) {
		t.Fatal("lease still held after the run completed")
	}
}

func TestBackgroundRunReleasesItsLeaseWhenItEnds(t *testing.T) {
	m, session := backgroundManager(t, answering())
	ex, err := m.StartMessage(t.Context(), session.ID, "hello", RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, ex)
	if got := statusOf(t, m, ex.Run.ID); got.Status != domain.RunCompleted {
		t.Fatalf("status = %s", got.Status)
	}
	if leaseHeld(t, m, ex.Run.ID) {
		t.Fatal("lease still held after the run completed")
	}
}

// Resume derives a cancellable context after it takes the lease, and that
// context is cancelled before the lease is released.
func TestResumedRunReleasesItsLeaseWhenItEnds(t *testing.T) {
	ask := domain.ToolCall{ProviderCallID: "q", Name: "ask_user", Arguments: json.RawMessage(`{"question":"which one?"}`)}
	m, session := backgroundManager(t, newBlockingProvider(func(call int) *domain.ModelResponse {
		if call == 1 {
			return &domain.ModelResponse{Provider: "blocking", Complete: true, Items: []domain.ResponseItem{{Kind: domain.ItemToolCall, ToolCall: &ask}}}
		}
		return answering().respond(call)
	}))
	run, err := m.RunMessage(t.Context(), session.ID, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitUser {
		t.Fatalf("run = %s/%s", run.Status, run.WaitReason)
	}
	if leaseHeld(t, m, run.ID) {
		t.Fatal("lease still held while the run waits for the user")
	}
	ex, err := m.StartResume(t.Context(), run.ID, ResumeInput{Answer: "the first"})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, ex)
	if got := statusOf(t, m, run.ID); got.Status != domain.RunCompleted {
		t.Fatalf("status = %s", got.Status)
	}
	if leaseHeld(t, m, run.ID) {
		t.Fatal("lease still held after the resumed run completed")
	}
}
