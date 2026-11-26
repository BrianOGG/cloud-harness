package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
)

// Exercise provider -> agent -> real temporary SQLite -> HTTP projections.
// No preview process or acceptance database is used or modified.
func TestReasoningContentStaysOutOfDurableReplies(t *testing.T) {
	for _, wire := range []string{"json", "sse"} {
		t.Run(wire, func(t *testing.T) {
			const draft = "private draft: 可以回答问题和处理任务"
			const answer = "可以回答问题、处理任务。"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if wire == "json" {
					w.Header().Set("Content-Type", "application/json")
					body := map[string]any{"id": "reply_fixture", "choices": []any{map[string]any{
						"finish_reason": "stop", "message": map[string]string{
							"content":           draft + "\n</think>\n\n" + answer,
							"reasoning_content": "private structured draft",
						},
					}}}
					if err := json.NewEncoder(w).Encode(body); err != nil {
						t.Error(err)
					}
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, text := range []string{draft, "\n</thi", "nk>\n\n", answer} {
					data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": text}}}})
					_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
				}
				_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			m, h := newServer(t)
			m.Workspace = t.TempDir()
			m.Provider = &provider.OpenAICompatible{BaseURL: upstream.URL, APIKey: "fixture", Model: "agnes-3.0-flash", Client: upstream.Client()}
			ctx := context.Background()
			session, err := m.CreateSession(ctx)
			if err != nil {
				t.Fatal(err)
			}
			run, err := m.RunMessage(ctx, session.ID, "介绍一下你能做什么")
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != domain.RunCompleted || run.FinalAnswer != answer {
				t.Fatalf("run status=%s final_answer=%q", run.Status, run.FinalAnswer)
			}
			stored, err := m.Store.GetRun(ctx, run.ID)
			if err != nil || stored.FinalAnswer != answer {
				t.Fatalf("stored final_answer=%q, err=%v", stored.FinalAnswer, err)
			}
			responses, err := m.Store.ListVisibleResponses(ctx, run.ID)
			if err != nil || len(responses) != 1 {
				t.Fatalf("responses=%d, err=%v", len(responses), err)
			}
			var text strings.Builder
			for _, item := range responses[0].Items {
				if item.Kind == domain.ItemText {
					text.WriteString(item.Text)
				}
			}
			if text.String() != answer {
				t.Fatalf("stored model text=%q", text.String())
			}
			opaque, err := m.Store.LoadContinuation(ctx, responses[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			var blob domain.OpaqueBlob
			if err := json.Unmarshal(opaque, &blob); err != nil {
				t.Fatal(err)
			}
			var private strings.Builder
			for _, item := range blob.Items {
				private.Write(item.Payload)
			}
			if !strings.Contains(private.String(), draft) {
				t.Fatal("inline draft missing from restricted continuation")
			}
			for _, path := range []string{"/v1/runs/" + run.ID, "/v1/sessions/" + session.ID + "/messages"} {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
				if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "private") || strings.Contains(rec.Body.String(), "think") {
					t.Fatalf("unsafe public reply at %s: status=%d body=%s", path, rec.Code, rec.Body)
				}
			}
			page := a4ContractDecodePage(t, a4ContractGET(h, session.ID, ""), session.ID)
			count := 0
			for _, message := range page.Messages {
				if message.Kind == "response" {
					count++
					if message.Content != answer {
						t.Fatalf("transcript content=%q", message.Content)
					}
				}
			}
			if count != 1 {
				t.Fatalf("response facts=%d, want exactly one", count)
			}
		})
	}
}
