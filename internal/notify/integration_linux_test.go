package notify_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/agent"
	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/notify"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage/sqlite"
)

func TestLocalChannelsThroughConfigLoader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, tt := range []struct {
		name, body, wantError string
	}{
		{"valid local channels", `{"notify":{"channels":[{"type":"osc9"},{"type":"desktop","events":["approval","ask_user","completion","failure"]}]}}`, ""},
		{"OSC9 URL", `{"notify":{"channels":[{"type":"osc9","url":"https://example.com"}]}}`, "does not accept url"},
		{"desktop URL", `{"notify":{"channels":[{"type":"desktop","url":"https://example.com"}]}}`, "does not accept url"},
		{"unknown OSC9 event", `{"notify":{"channels":[{"type":"osc9","events":["done"]}]}}`, `unknown event "done"`},
		{"unknown desktop event", `{"notify":{"channels":[{"type":"desktop","events":["done"]}]}}`, `unknown event "done"`},
		{"OSC9 credential field", `{"notify":{"channels":[{"type":"osc9","credential":"secret"}]}}`, "unknown field"},
		{"desktop credential field", `{"notify":{"channels":[{"type":"desktop","token":"secret"}]}}`, "unknown field"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path, func(string) string { return "" })
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Load error=%v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil || len(cfg.Notify.Channels) != 2 {
				t.Fatalf("Load channels=%+v error=%v", cfg.Notify.Channels, err)
			}
		})
	}
}

func TestManagerMissingDesktopCommandIsIsolated(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	delivered := make(chan notify.Event, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var event notify.Event
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode webhook payload: %v", err)
		}
		delivered <- event
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	n, err := notify.New(notify.Config{Channels: []notify.ChannelConfig{{Type: "desktop"}, {Type: "webhook", URL: server.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := &agent.Manager{Store: db, Provider: &provider.Mock{}, Notifier: n, DisableSnapshots: true}
	session, err := m.CreateSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	run, err := m.RunMessage(ctx, session.ID, "complete without notify-send")
	if err != nil || run.Status != domain.RunCompleted {
		t.Fatalf("run=%+v error=%v", run, err)
	}
	select {
	case event := <-delivered:
		if event.EventType != notify.EventCompleted || event.RunID != run.ID {
			t.Fatalf("webhook event=%+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("desktop failure prevented webhook delivery")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		events, err := db.ListEvents(ctx, run.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == "notify.failed" {
				var payload map[string]string
				if err := json.Unmarshal(event.Payload, &payload); err != nil || payload["channel"] != "desktop" {
					t.Fatalf("notify.failed payload=%s error=%v", event.Payload, err)
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Manager did not record desktop delivery failure")
}
