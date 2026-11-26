package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChannelProtocols(t *testing.T) {
	tests := []struct {
		name, typ, basePath, wantPath string
		credential                    string
		wantAuth                      string
	}{
		{"webhook", "webhook", "", "/hook", "hook-secret", "Bearer hook-secret"},
		{"bark", "bark", "/base", "/base/device-key", "device-key", ""},
		{"ntfy", "ntfy", "", "/topic", "ntfy-secret", "Bearer ntfy-secret"},
	}
	event := Event{RunID: "run_123", EventType: EventWaitingApproval, Status: "WAITING", Summary: "command=go test", ApprovalURL: "https://approval.example/ui?run=run_123", OccurredAt: time.Unix(123, 0).UTC()}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != tt.wantPath {
					t.Errorf("request=%s %s, want POST %s", r.Method, r.URL.Path, tt.wantPath)
				}
				if got := r.Header.Get("Authorization"); got != tt.wantAuth {
					t.Errorf("authorization=%q, want %q", got, tt.wantAuth)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode payload: %v", err)
				}
				requests <- body
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			target := server.URL + tt.basePath
			if tt.typ == "webhook" {
				target += "/hook"
			}
			if tt.typ == "ntfy" {
				target += "/topic"
			}
			cfg := Config{Channels: []ChannelConfig{{Type: tt.typ, URL: target, Credential: tt.credential}}}
			n, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for err := range n.Notify(context.Background(), event) {
				t.Fatalf("delivery error: %v", err)
			}
			select {
			case body := <-requests:
				if body["run_id"] != event.RunID || body["event_type"] != event.EventType || body["status"] != event.Status || body["summary"] != event.Summary || body["approval_url"] != event.ApprovalURL {
					t.Fatalf("payload=%#v", body)
				}
			case <-time.After(time.Second):
				t.Fatal("notification request not received")
			}
		})
	}
}

func TestEventFilteringAndDefaultEvents(t *testing.T) {
	calls := make(chan string, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload Event
		_ = json.NewDecoder(r.Body).Decode(&payload)
		calls <- payload.EventType
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	filtered, err := New(Config{Channels: []ChannelConfig{{Type: "webhook", URL: server.URL, Events: []string{EventCompleted}}}})
	if err != nil {
		t.Fatal(err)
	}
	for range filtered.Notify(context.Background(), Event{EventType: EventWaitingApproval}) {
	}
	select {
	case got := <-calls:
		t.Fatalf("filtered channel received %q", got)
	case <-time.After(80 * time.Millisecond):
	}
	for range filtered.Notify(context.Background(), Event{EventType: EventCompleted}) {
	}
	select {
	case got := <-calls:
		if got != EventCompleted {
			t.Fatalf("got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("completion was not delivered")
	}

	all, err := New(Config{Channels: []ChannelConfig{{Type: "webhook", URL: server.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{EventWaitingApproval, EventAskUser, EventCompleted, EventFailed} {
		for range all.Notify(context.Background(), Event{EventType: typ}) {
		}
		select {
		case got := <-calls:
			if got != typ {
				t.Fatalf("default filter got %q, want %q", got, typ)
			}
		case <-time.After(time.Second):
			t.Fatalf("default channel did not deliver %q", typ)
		}
	}
}

func TestTimeoutAndFailureAreBounded(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	n, err := New(Config{Channels: []ChannelConfig{{Type: "webhook", URL: server.URL, TimeoutMS: 25}}})
	if err != nil {
		t.Fatal(err)
	}
	begin := time.Now()
	results := n.Notify(context.Background(), Event{RunID: "r", EventType: EventCompleted})
	if elapsed := time.Since(begin); elapsed > 30*time.Millisecond {
		t.Fatalf("Notify blocked for %s", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request not started")
	}
	select {
	case err := <-results:
		if err == nil || !strings.Contains(err.Error(), "request failed") {
			t.Fatalf("result=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out delivery did not report failure")
	}
}

func TestNoConfigurationDisablesNotifier(t *testing.T) {
	n, err := New(Config{})
	if err != nil || n != nil {
		t.Fatalf("notifier=%v err=%v, want nil, nil", n, err)
	}
}

func TestRejectsUnknownTypeAndUnsafeURL(t *testing.T) {
	for _, cfg := range []Config{
		{Channels: []ChannelConfig{{Type: "mystery", URL: "https://example.com"}}},
		{Channels: []ChannelConfig{{Type: "webhook", URL: "file:///tmp/hook"}}},
		{Channels: []ChannelConfig{{Type: "webhook", URL: "http://user:pass@example.com/hook"}}},
		{Channels: []ChannelConfig{{Type: "webhook", URL: "https://example.com/hook?token=inline-secret"}}},
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("accepted invalid config: %+v", cfg)
		}
	}
}
