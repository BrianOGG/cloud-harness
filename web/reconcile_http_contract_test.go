package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/agent"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/storage/sqlite"
	"github.com/BrianOGG/cloud-harness/internal/transport"
)

func TestReconciliationDecisionsOverHTTP(t *testing.T) {
	chat, err := os.ReadFile(filepath.Join("src", "Chat.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		decision   string
		wantStatus domain.InvStatus
	}{
		{name: "mark failed", decision: buttonDecision(t, string(chat), "标记失败"), wantStatus: domain.InvFailed},
		{name: "mark succeeded", decision: buttonDecision(t, string(chat), "标记成功"), wantStatus: domain.InvSucceeded},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "reconcile.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })

			manager := &agent.Manager{
				Store:       db,
				Provider:    &provider.Mock{},
				OwnerID:     "reconcile-http-contract",
				Workspace:   t.TempDir(),
				SkipPlanner: true,
			}
			httpServer := httptest.NewServer((&transport.Server{Manager: manager}).Handler())
			t.Cleanup(httpServer.Close)

			session, err := manager.CreateSession(ctx)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			turn := storage.Turn{ID: domain.NewID("turn_"), SessionID: session.ID, UserInput: "recover a tool result", Status: "RUNNING", CreatedAt: now}
			if err := db.CreateTurn(ctx, turn); err != nil {
				t.Fatal(err)
			}
			run := storage.Run{
				ID:         domain.NewID("run_"),
				SessionID:  session.ID,
				TurnID:     turn.ID,
				Status:     domain.RunWaiting,
				WaitReason: domain.WaitReconciliation,
				Budget:     domain.DefaultBudget(),
				Version:    1,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			if err := db.CreateRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			if err := db.SaveCheckpoint(ctx, run.ID, run.Version, []byte(`{"messages":[]}`), now); err != nil {
				t.Fatal(err)
			}
			invocation := domain.Invocation{
				ID:             domain.NewID("inv_"),
				ProviderCallID: domain.NewID("call_"),
				Name:           "test_tool",
				Arguments:      json.RawMessage(`{}`),
				Class:          domain.ActionExecute,
				Status:         domain.InvUnknown,
				Dispatched:     true,
			}
			batch := storage.StoredBatch{
				ID:          domain.NewID("batch_"),
				RunID:       run.ID,
				Status:      domain.BatchNeedsReconciliation,
				Action:      domain.ActionExecute,
				Invocations: []domain.Invocation{invocation},
			}
			if err := db.SaveBatch(ctx, batch); err != nil {
				t.Fatal(err)
			}

			body := `{"decision":"` + test.decision + `"}`
			response, err := httpServer.Client().Post(httpServer.URL+"/v1/runs/"+run.ID+"/responses?wait=1", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := response.Body.Close(); err != nil {
					t.Errorf("close response body: %v", err)
				}
			})
			if response.StatusCode != http.StatusOK {
				t.Fatalf("POST /v1/runs/%s/responses decision %q: got HTTP %d", run.ID, test.decision, response.StatusCode)
			}

			var view struct {
				Status string `json:"status"`
			}
			if err := json.NewDecoder(response.Body).Decode(&view); err != nil {
				t.Fatal(err)
			}
			if view.Status != string(domain.RunCompleted) {
				t.Fatalf("decision %q returned run status %q, want %q", test.decision, view.Status, domain.RunCompleted)
			}

			updated, err := db.LoadBatches(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(updated) != 1 || len(updated[0].Invocations) != 1 {
				t.Fatalf("expected one reconciled invocation, got %+v", updated)
			}
			if got := updated[0].Invocations[0].Status; got != test.wantStatus {
				t.Fatalf("decision %q produced invocation status %q, want %q", test.decision, got, test.wantStatus)
			}
		})
	}
}

func buttonDecision(t *testing.T, source, label string) string {
	t.Helper()
	labelIndex := strings.Index(source, ">"+label+"</button>")
	if labelIndex < 0 {
		t.Fatalf("could not find %q button in Chat.tsx", label)
	}
	buttonStart := strings.LastIndex(source[:labelIndex], "<button")
	buttonEnd := strings.Index(source[labelIndex:], "</button>")
	if buttonStart < 0 || buttonEnd < 0 {
		t.Fatalf("could not isolate %q button in Chat.tsx", label)
	}
	button := source[buttonStart : labelIndex+buttonEnd]
	match := regexp.MustCompile(`decision:\s*'([^']+)'`).FindStringSubmatch(button)
	if len(match) != 2 {
		t.Fatalf("could not read decision payload from %q button", label)
	}
	return match[1]
}
