package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/notify"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

func TestAskUserNotificationAndNoConfigZeroRequests(t *testing.T) {
	// With no notification configuration, an ordinary mock run has no outbound
	// HTTP transport activity.
	oldTransport := http.DefaultTransport
	counter := &countingRoundTripper{next: oldTransport}
	http.DefaultTransport = counter
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	plain := &provider.Mock{Responses: []domain.ModelResponse{{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "done"}}}}}
	m, _, _ := testManager(t, plain)
	session, err := m.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run, err := m.RunMessage(context.Background(), session.ID, "hello"); err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if counter.calls.Load() != 0 {
		t.Fatalf("unconfigured notification made %d HTTP requests", counter.calls.Load())
	}

	askEvents := make(chan notify.Event, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var event notify.Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode ask_user notification: %v", err)
		}
		askEvents <- event
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	ask := &provider.Mock{Responses: []domain.ModelResponse{toolResp(domain.ToolCall{ProviderCallID: "ask-1", Name: domain.ToolAskUser, Arguments: json.RawMessage(`{"question":"Deploy to staging?"}`)})}}
	askManager, _, _ := testManager(t, ask)
	n, err := notify.New(notify.Config{Channels: []notify.ChannelConfig{{Type: "webhook", URL: server.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	askManager.Notifier = n
	session, err = askManager.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := askManager.RunMessage(context.Background(), session.ID, "prepare deploy")
	if err != nil || waiting.Status != domain.RunWaiting || waiting.WaitReason != domain.WaitUser {
		t.Fatalf("run=%+v err=%v", waiting, err)
	}
	question := receiveNotifyEvent(t, askEvents)
	if question.EventType != notify.EventAskUser || question.Summary != "Deploy to staging?" {
		t.Fatalf("ask_user notification=%+v", question)
	}
	if question.ApprovalURL != "" {
		t.Fatalf("unexpected approval link without a configured base URL: %q", question.ApprovalURL)
	}
}

func TestFailureAndCancellationShareFailureNotifications(t *testing.T) {
	events := make(chan notify.Event, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var event notify.Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode notification: %v", err)
		}
		events <- event
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	n, err := notify.New(notify.Config{Channels: []notify.ChannelConfig{{Type: "webhook", URL: server.URL, Events: []string{notify.EventFailed}}}})
	if err != nil {
		t.Fatal(err)
	}

	failedManager, _, _ := testManager(t, &provider.Mock{Err: errors.New("provider broke")})
	failedManager.Notifier = n
	session, err := failedManager.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	failed, err := failedManager.RunMessage(context.Background(), session.ID, "fail")
	if err != nil || failed.Status != domain.RunFailed {
		t.Fatalf("run=%+v err=%v", failed, err)
	}
	failedEvent := receiveNotifyEvent(t, events)
	if failedEvent.EventType != notify.EventFailed || failedEvent.Status != string(domain.RunFailed) {
		t.Fatalf("failure notification=%+v", failedEvent)
	}

	waitingManager, _, _ := testManager(t, &provider.Mock{Responses: []domain.ModelResponse{toolResp(domain.ToolCall{ProviderCallID: "ask-cancel", Name: domain.ToolAskUser, Arguments: json.RawMessage(`{"question":"Continue?"}`)})}})
	waitingManager.Notifier = n
	session, err = waitingManager.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := waitingManager.RunMessage(context.Background(), session.ID, "wait")
	if err != nil || waiting.Status != domain.RunWaiting {
		t.Fatalf("run=%+v err=%v", waiting, err)
	}
	if _, err := waitingManager.Cancel(context.Background(), waiting.ID); err != nil {
		t.Fatal(err)
	}
	cancelled := receiveNotifyEvent(t, events)
	if cancelled.EventType != notify.EventFailed || cancelled.Status != string(domain.RunCancelled) {
		t.Fatalf("cancel notification=%+v", cancelled)
	}
}

type countingRoundTripper struct {
	next  http.RoundTripper
	calls atomic.Int64
}

func (c *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.next.RoundTrip(req)
}

func TestNotificationFailureAndTimeoutDoNotChangeRun(t *testing.T) {
	var logBuffer synchronizedLogBuffer
	previousLog := log.Writer()
	log.SetOutput(&logBuffer)
	t.Cleanup(func() { log.SetOutput(previousLog) })

	t.Run("server failure is recorded without secret leakage", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
		defer server.Close()
		mock := &provider.Mock{Responses: []domain.ModelResponse{{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "finished"}}}}}
		m, db, _ := testManager(t, mock)
		n, err := notify.New(notify.Config{Channels: []notify.ChannelConfig{{Type: "webhook", URL: server.URL, Credential: "hook-super-secret"}}})
		if err != nil {
			t.Fatal(err)
		}
		m.Notifier = n
		session, err := m.CreateSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		run, err := m.RunMessage(context.Background(), session.ID, "complete")
		if err != nil || run.Status != domain.RunCompleted {
			t.Fatalf("run=%+v err=%v", run, err)
		}
		waitForNotifyFailure(t, db, run.ID)
		if !strings.Contains(logBuffer.String(), "notification delivery failed") {
			t.Fatalf("delivery failure was not logged: %s", logBuffer.String())
		}
		if strings.Contains(logBuffer.String(), "hook-super-secret") {
			t.Fatalf("credential leaked into logs: %s", logBuffer.String())
		}
	})

	t.Run("timeout does not hold run completion", func(t *testing.T) {
		started := make(chan struct{}, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started <- struct{}{}
			time.Sleep(700 * time.Millisecond)
		}))
		defer server.Close()
		mock := &provider.Mock{Responses: []domain.ModelResponse{{Provider: "mock", Complete: true, Items: []domain.ResponseItem{{Ordinal: 0, Kind: domain.ItemText, Text: "finished"}}}}}
		m, db, _ := testManager(t, mock)
		m.DisableSnapshots = true
		n, err := notify.New(notify.Config{Channels: []notify.ChannelConfig{{Type: "webhook", URL: server.URL, TimeoutMS: 40}}})
		if err != nil {
			t.Fatal(err)
		}
		m.Notifier = n
		session, err := m.CreateSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		begin := time.Now()
		run, err := m.RunMessage(context.Background(), session.ID, "complete")
		if err != nil || run.Status != domain.RunCompleted {
			t.Fatalf("run=%+v err=%v", run, err)
		}
		if elapsed := time.Since(begin); elapsed > 300*time.Millisecond {
			t.Fatalf("notification delayed run by %s", elapsed)
		}
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("notification did not start")
		}
		waitForNotifyFailure(t, db, run.ID)
	})
}

type synchronizedLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *synchronizedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *synchronizedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func waitForNotifyFailure(t *testing.T, db storage.Store, runID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events, err := db.ListEvents(context.Background(), runID, 0)
		if err == nil {
			for _, event := range events {
				if event.Type == "notify.failed" {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("notify.failed run event was not recorded")
}
