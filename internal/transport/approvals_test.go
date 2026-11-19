package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/notify"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
	"github.com/cloud-harness/cloud-harness/internal/tools"
)

type approvalNotifications chan notify.Event

func (n approvalNotifications) Notify(_ context.Context, event notify.Event) <-chan error {
	n <- event
	done := make(chan error)
	close(done)
	return done
}

func (n approvalNotifications) next(t *testing.T) notify.Event {
	t.Helper()
	select {
	case event := <-n:
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("missing lifecycle notification")
		return notify.Event{}
	}
}

func TestApprovalsQueueContextAndOrder(t *testing.T) {
	ctx := context.Background()
	m, _ := newServer(t)
	m.Workspace = t.TempDir()
	m.DisableSnapshots = true
	m.Broker = tools.NewBroker(m.Store, m.Store)
	if err := tools.RegisterLocal(m.Broker, m.Workspace); err != nil {
		t.Fatal(err)
	}
	m.ApprovalBaseURL = "https://harness.example/root/"
	notifications := make(approvalNotifications, 8)
	m.Notifier = notifications
	command := "rm -rf -- obsolete-artifacts # " + strings.Repeat("pending command context ", 12)
	question := "部署到哪个区域？\nPlease choose the region verbatim."
	toolResponse := func(id, name, key, text string) domain.ModelResponse {
		args, err := json.Marshal(map[string]string{key: text})
		if err != nil {
			t.Fatal(err)
		}
		return domain.ModelResponse{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{
			Ordinal: 0, Kind: domain.ItemToolCall,
			ToolCall: &domain.ToolCall{ProviderCallID: id, Name: name, Arguments: args},
		}}}
	}
	m.Provider = &provider.Mock{Responses: []domain.ModelResponse{
		toolResponse("old-command", "run_command", "command", "rm -rf -- first-request"),
		toolResponse("question", domain.ToolAskUser, "question", question),
		{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "done"}}},
		toolResponse("current-command", "run_command", "command", command),
	}}
	now := time.Date(2026, 11, 19, 12, 0, 0, 500000000, time.UTC)
	var clock atomic.Int64
	m.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	start := func(at time.Time) storage.Run {
		t.Helper()
		clock.Store(at.UnixNano())
		session, err := m.CreateSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		run, err := m.RunMessage(ctx, session.ID, "queue fixture")
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	approval := start(now.Add(-12 * time.Minute))
	_ = notifications.next(t)
	ask := start(now.Add(-10 * time.Minute))
	askNotification := notifications.next(t)
	done := start(now.Add(-8 * time.Minute))
	_ = notifications.next(t)
	if done.Status != domain.RunCompleted {
		t.Fatalf("expected a completed run, got %+v", done)
	}
	// The first-created run resumes and waits a second time, after ask_user.
	// Its old wait must not determine its position or its current summary.
	clock.Store(now.Add(-120500 * time.Millisecond).UnixNano())
	var err error
	approval, err = m.Resume(ctx, approval.ID, agent.ResumeInput{Decision: "deny"})
	if err != nil || approval.Status != domain.RunWaiting || approval.WaitReason != domain.WaitApproval {
		t.Fatalf("second approval wait: %+v, %v", approval, err)
	}
	approvalNotification := notifications.next(t)
	// A later timestamp update and event do not restart the waiting clock.
	if err := m.Store.ExtendModelBudget(ctx, approval.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Store.AppendEvent(ctx, storage.Event{RunID: approval.ID, Type: "notify.failed", Persistent: true, CreatedAt: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	for _, reason := range []domain.WaitReason{domain.WaitBudget, domain.WaitChild, domain.WaitReconciliation} {
		if err := m.Store.CreateRun(ctx, storage.Run{
			ID: "run_" + string(reason), SessionID: done.SessionID, Status: domain.RunWaiting,
			WaitReason: reason, Budget: domain.DefaultBudget(), Version: 1,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := m.Store.ListRunsByStatus(ctx, domain.RunWaiting)
	if err != nil {
		t.Fatal(err)
	}
	h := (&Server{Manager: m, Now: func() time.Time { return now }}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/approvals", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("queue %d %s, headers=%v", rec.Code, rec.Body, rec.Header())
	}
	var queue []struct {
		RunID          string    `json:"run_id"`
		SessionID      string    `json:"session_id"`
		Status         string    `json:"status"`
		WaitReason     string    `json:"wait_reason"`
		Summary        string    `json:"summary"`
		WaitingSince   time.Time `json:"waiting_since"`
		WaitingSeconds int64     `json:"waiting_seconds"`
		ApprovalURL    string    `json:"approval_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &queue); err != nil {
		t.Fatal(err)
	}
	if len(queue) != 2 || queue[0].RunID != ask.ID || queue[1].RunID != approval.ID {
		t.Fatalf("expected ask_user then current approval, got %s", rec.Body)
	}
	wants := []struct {
		run     storage.Run
		reason  string
		summary string
		since   time.Time
		seconds int64
		event   notify.Event
	}{
		{ask, "ask_user", question, now.Add(-10 * time.Minute), 600, askNotification},
		{approval, "approval", command, now.Add(-120500 * time.Millisecond), 120, approvalNotification},
	}
	for i, want := range wants {
		got := queue[i]
		if got.SessionID != want.run.SessionID || got.Status != "WAITING" || got.WaitReason != want.reason || got.Summary != want.summary || !got.WaitingSince.Equal(want.since) || got.WaitingSeconds != want.seconds {
			t.Fatalf("queue item %d: %+v, expected %+v", i, got, want)
		}
		if got.ApprovalURL != "https://harness.example/root/approval?run="+want.run.ID {
			t.Fatalf("approval URL: %q", got.ApprovalURL)
		}
		info, err := m.WaitInfo(ctx, want.run)
		if err != nil || info.Summary() != got.Summary || got.Summary != want.event.Summary || got.ApprovalURL != want.event.ApprovalURL {
			t.Fatalf("queue, WaitInfo, notification mismatch: %+v / %+v / %+v (%v)", got, info, want.event, err)
		}
	}
	after, err := m.Store.ListRunsByStatus(ctx, domain.RunWaiting)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("queue read changed persisted run state: %v", err)
	}
	m.ApprovalBaseURL = ""
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/approvals", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "approval_url") {
		t.Fatalf("unconfigured approval URL should be omitted: %d %s", rec.Code, rec.Body)
	}
}

func TestApprovalsEmptyQueue(t *testing.T) {
	_, h := newServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/approvals", nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("expected 200 and [], got %d %s", rec.Code, rec.Body)
	}
}

func TestApprovalsStorageErrorEnvelope(t *testing.T) {
	m, h := newServer(t)
	if err := m.Store.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/approvals", nil))
	if rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) != `{"error":{"code":"internal","message":"request failed"}}` {
		t.Fatalf("storage error envelope: %d %s", rec.Code, rec.Body)
	}
}

func TestApprovalsLegacyWaitAndClockSkew(t *testing.T) {
	m, _ := newServer(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Legacy runs without a waiting event use updated_at. A future timestamp
	// clamps elapsed seconds to zero, with run id as the tie breaker.
	for _, id := range []string{"run_z", "run_a"} {
		if err := m.Store.CreateRun(ctx, storage.Run{ID: id, SessionID: session.ID,
			Status: domain.RunWaiting, WaitReason: domain.WaitUser, Version: 1,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	h := (&Server{Manager: m, Now: func() time.Time { return now }}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/approvals", nil))
	var queue []approvalView
	if err := json.Unmarshal(rec.Body.Bytes(), &queue); err != nil || rec.Code != http.StatusOK || len(queue) != 2 {
		t.Fatalf("legacy queue: %d %s (%v)", rec.Code, rec.Body, err)
	}
	if queue[0].RunID != "run_a" || queue[1].RunID != "run_z" {
		t.Fatalf("unstable tie order: %+v", queue)
	}
	for _, item := range queue {
		if item.WaitingSeconds != 0 || !item.WaitingSince.Equal(now.Add(time.Minute)) {
			t.Fatalf("legacy clock: %+v", item)
		}
	}
}
