package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNetworkPayloadBytesUnchanged(t *testing.T) {
	for _, summary := range []string{"done", ""} {
		for _, typ := range []string{"webhook", "bark", "ntfy"} {
			t.Run(typ+"/"+summary, func(t *testing.T) {
				payloads := make(chan string, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer r.Body.Close()
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read payload: %v", err)
					}
					payloads <- string(body)
					w.WriteHeader(http.StatusNoContent)
				}))
				defer server.Close()
				n, err := New(Config{Channels: []ChannelConfig{{Type: typ, URL: server.URL, Credential: "credential"}}})
				if err != nil {
					t.Fatal(err)
				}
				for err := range n.Notify(context.Background(), Event{RunID: "r", EventType: EventCompleted, Status: "COMPLETED", Summary: summary, OccurredAt: time.Unix(123, 0).UTC()}) {
					t.Fatal(err)
				}
				want := `{"run_id":"r","event_type":"completion","status":"COMPLETED"`
				if summary != "" {
					want += `,"summary":"done"`
				}
				want += `,"occurred_at":"1970-01-01T00:02:03Z"`
				if typ == "bark" {
					body := "done"
					if summary == "" {
						body = "completion COMPLETED"
					}
					want += `,"title":"completion","body":"` + body + `","group":"r"`
				}
				want += "}"
				if got := <-payloads; got != want {
					t.Fatalf("payload=%s, want %s", got, want)
				}
			})
		}
	}
}
