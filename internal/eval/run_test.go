package eval

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/config"
	"github.com/cloud-harness/cloud-harness/internal/provider"
)

func TestApprovalSkipAndCrashRecovery(t *testing.T) {
	ctx := context.Background()
	for _, task := range []string{"batch-approval-skip", "recover-mid-batch"} {
		root := t.TempDir()
		result, err := Run(ctx, Options{
			Task:      task,
			Config:    config.Config{DBPath: filepath.Join(root, "eval.db"), Provider: "mock"},
			Workspace: root,
		})
		if err != nil {
			t.Fatal(task, err)
		}
		if !result.Pass {
			t.Fatalf("%s failures %#v calls %#v status %s", task, result.Failures, result.ToolCalls, result.Status)
		}
		if result.RunID == "" {
			t.Fatal("missing run id")
		}
	}
}

func TestRetrievalPlannerAndCacheEvals(t *testing.T) {
	ctx := context.Background()
	for _, task := range []string{"retrieve-chinese-term", "retrieve-path-and-id", "planner-compare", "cache-baseline", "incident-wait-for-ready"} {
		root := t.TempDir()
		result, err := Run(ctx, Options{
			Task:      task,
			Config:    config.Config{DBPath: filepath.Join(root, "eval.db"), Provider: "mock"},
			Workspace: root,
		})
		if err != nil {
			t.Fatal(task, err)
		}
		if !result.Pass {
			t.Fatalf("%s failures %#v metrics %s", task, result.Failures, result.Metrics)
		}
	}
}

func TestBatchThreeReadsStreamingOpenAI(t *testing.T) {
	const key = "sk-eval-fixture"
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+key {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), key) {
			t.Error("request body contains the api key")
		}
		n++
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			chunks := []string{
				`data: {"id":"chatcmpl_1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"alpha.txt\"}"}}]}}]}`,
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"beta.txt\"}"}}]}}]}`,
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"call_c","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"gamma.txt\"}"}}]}}]}`,
				`data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`,
				"data: [DONE]",
			}
			for _, c := range chunks {
				io.WriteString(w, c+"\n\n")
			}
			return
		}
		io.WriteString(w, `data: {"id":"chatcmpl_2","choices":[{"index":0,"delta":{"content":"alpha, beta, and gamma were read."}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	result, err := Run(context.Background(), Options{
		Task: "batch-three-reads",
		Config: config.Config{
			DBPath:   filepath.Join(t.TempDir(), "eval.db"),
			Provider: "openai",
		},
		Provider: &provider.OpenAICompatible{
			BaseURL: srv.URL, APIKey: key, Model: "agnes-2.5-flash", Client: srv.Client(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), key) || strings.Contains(string(raw), "Bearer") {
		t.Fatalf("result leaked credentials: %s", raw)
	}
	if !result.Pass {
		t.Fatalf("failures %#v calls %#v", result.Failures, result.ToolCalls)
	}
	if result.Status != "COMPLETED" || result.RunID == "" || len(result.ToolCalls) != 3 {
		t.Fatalf("%+v", result)
	}
	if result.ToolCalls[0].ProviderCallID != "call_a" || result.ToolCalls[1].ProviderCallID != "call_b" || result.ToolCalls[2].ProviderCallID != "call_c" {
		t.Fatalf("order %+v", result.ToolCalls)
	}
	if n != 2 {
		t.Fatalf("provider calls %d", n)
	}
}
