package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

func TestAnthropicMessagesShapeParseAndUsage(t *testing.T) {
	const key = "anthropic-test-key"
	var gotPath, gotAuth, gotVersion string
	var body struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		System    string `json:"system"`
		Messages  []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotVersion = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), key) {
			t.Error("api key leaked into request body")
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","content":[{"type":"text","text":"pong"},{"type":"tool_use","id":"toolu_1","name":"mcp_files_read","input":{"path":"a"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":3}}`)
	}))
	defer srv.Close()
	p := &Anthropic{BaseURL: srv.URL, APIKey: key, Model: "claude-test", Client: srv.Client()}
	resp, err := p.Complete(context.Background(), Request{
		Messages: []Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "look", Images: []Image{testProviderImage()}},
		},
		Tools: []ToolDefinition{{Name: "mcp.files.read", Description: "read", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/messages" || gotAuth != key || gotVersion != "2023-06-01" {
		t.Fatalf("request path=%q auth=%q version=%q", gotPath, gotAuth, gotVersion)
	}
	if body.Model != "claude-test" || body.MaxTokens != 8192 || body.System != "be brief" {
		t.Fatalf("body %+v", body)
	}
	if len(body.Messages) != 1 || body.Messages[0].Role != "user" || len(body.Messages[0].Content) != 2 {
		t.Fatalf("messages %+v", body.Messages)
	}
	if body.Messages[0].Content[0]["type"] != "text" || body.Messages[0].Content[1]["type"] != "image" {
		t.Fatalf("content %+v", body.Messages[0].Content)
	}
	if len(body.Tools) != 1 || body.Tools[0].Name != "mcp_files_read" || !strings.Contains(string(body.Tools[0].InputSchema), "path") {
		t.Fatalf("tools %+v", body.Tools)
	}
	if resp.Provider != "anthropic" || !resp.Complete {
		t.Fatalf("response %+v", resp)
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "mcp.files.read" || calls[0].ProviderCallID != "toolu_1" {
		t.Fatalf("tool calls %+v", calls)
	}
	var usage domain.Usage
	for _, item := range resp.Items {
		if item.Kind == domain.ItemUsage && item.Usage != nil {
			usage = *item.Usage
		}
	}
	if usage.InputTokens != 5 || usage.OutputTokens != 2 || usage.CachedInputTokens != 3 {
		t.Fatalf("usage %+v", usage)
	}
}

func TestAnthropicContinuationReplaysOpaqueBlocks(t *testing.T) {
	var body struct {
		Messages []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_2","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()
	p := &Anthropic{BaseURL: srv.URL, APIKey: "k", Model: "claude-test", Client: srv.Client()}
	_, err := p.Complete(context.Background(), Request{
		Messages: []Message{
			{Role: "user", Content: "go"},
			{Role: "assistant", ToolCalls: []domain.ToolCall{{ProviderCallID: "toolu_1", Name: "ask_user", Arguments: json.RawMessage(`{"question":"?"}`)}}},
			{Role: "tool", ToolCallID: "toolu_1", Content: `{"answer":"yes"}`},
		},
		Continuation: []domain.ResponseItem{
			{Kind: domain.ItemOpaque, Opaque: &domain.OpaqueItem{Provider: "anthropic", Kind: "thinking", Payload: []byte(`{"type":"thinking","thinking":"secret","signature":"sig"}`)}},
			{Kind: domain.ItemText, Text: "Need answer"},
			{Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "toolu_1", Name: "ask_user", Arguments: json.RawMessage(`{"question":"?"}`)}},
			{Kind: domain.ItemUsage, Usage: &domain.Usage{InputTokens: 1, OutputTokens: 1}},
		},
		ContinuationProvider: "anthropic",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages %+v", body.Messages)
	}
	assistant := body.Messages[1].Content
	if len(assistant) != 3 || assistant[0]["type"] != "thinking" || assistant[1]["type"] != "text" || assistant[2]["type"] != "tool_use" {
		t.Fatalf("assistant blocks %+v", assistant)
	}
	if assistant[0]["signature"] != "sig" || assistant[1]["text"] != "Need answer" {
		t.Fatalf("assistant block payload %+v", assistant)
	}
	user := body.Messages[2].Content
	if len(user) != 1 || user[0]["type"] != "tool_result" || user[0]["tool_use_id"] != "toolu_1" {
		t.Fatalf("tool result blocks %+v", user)
	}
}

func TestOpenAIResponsesShapeParseAndUsage(t *testing.T) {
	const key = "openai-responses-key"
	var gotPath, gotAuth string
	var body struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Store        bool   `json:"store"`
		Input        []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL string `json:"image_url"`
			} `json:"content"`
		} `json:"input"`
		Tools []struct {
			Type       string          `json:"type"`
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"tools"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), key) {
			t.Error("api key leaked into request body")
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[]},{"type":"function_call","call_id":"call_1","name":"mcp_files_read","arguments":"{\"path\":\"a\"}","status":"completed"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":7,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}`)
	}))
	defer srv.Close()
	p := &OpenAIResponses{BaseURL: srv.URL, APIKey: key, Model: "gpt-test", Client: srv.Client()}
	resp, err := p.Complete(context.Background(), Request{
		Messages: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "go", Images: []Image{testProviderImage()}},
		},
		Tools: []ToolDefinition{{Name: "mcp.files.read", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/responses" || gotAuth != "Bearer "+key {
		t.Fatalf("request path=%q auth=%q", gotPath, gotAuth)
	}
	if body.Model != "gpt-test" || body.Instructions != "sys" || body.Store {
		t.Fatalf("body %+v", body)
	}
	if len(body.Input) != 1 || body.Input[0].Role != "user" || len(body.Input[0].Content) != 2 || body.Input[0].Content[1].Type != "input_image" {
		t.Fatalf("input %+v", body.Input)
	}
	if len(body.Tools) != 1 || body.Tools[0].Name != "mcp_files_read" {
		t.Fatalf("tools %+v", body.Tools)
	}
	if resp.Provider != "openai-responses" || !resp.Complete {
		t.Fatalf("response %+v", resp)
	}
	if len(resp.Items) < 3 || resp.Items[0].Kind != domain.ItemOpaque || string(resp.Items[0].Opaque.Payload) != `{"type":"reasoning","id":"rs_1","summary":[]}` {
		t.Fatalf("items %+v", resp.Items)
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "mcp.files.read" || calls[0].ProviderCallID != "call_1" {
		t.Fatalf("tool calls %+v", calls)
	}
	var usage domain.Usage
	for _, item := range resp.Items {
		if item.Kind == domain.ItemUsage && item.Usage != nil {
			usage = *item.Usage
		}
	}
	if usage.InputTokens != 7 || usage.OutputTokens != 3 || usage.CachedInputTokens != 2 {
		t.Fatalf("usage %+v", usage)
	}
}

func TestOpenAIResponsesContinuationReplaysReasoningAndCalls(t *testing.T) {
	var input []json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []json.RawMessage `json:"input"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		input = body.Input
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer srv.Close()
	p := &OpenAIResponses{BaseURL: srv.URL, APIKey: "k", Model: "gpt-test", Client: srv.Client()}
	_, err := p.Complete(context.Background(), Request{
		Messages: []Message{
			{Role: "user", Content: "go"},
			{Role: "assistant", ToolCalls: []domain.ToolCall{{ProviderCallID: "call_1", Name: "ask_user", Arguments: json.RawMessage(`{"question":"?"}`)}}},
			{Role: "tool", ToolCallID: "call_1", Content: `{"answer":"yes"}`},
		},
		Continuation: []domain.ResponseItem{
			{Kind: domain.ItemOpaque, Opaque: &domain.OpaqueItem{Provider: "openai-responses", Kind: "reasoning", Payload: []byte(`{"type":"reasoning","id":"rs_1","encrypted_content":"secret"}`)}},
			{Kind: domain.ItemText, Text: "Need answer"},
			{Kind: domain.ItemToolCall, ToolCall: &domain.ToolCall{ProviderCallID: "call_1", Name: "ask_user", Arguments: json.RawMessage(`{"question":"?"}`)}},
		},
		ContinuationProvider: "openai-responses",
	})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, raw := range input {
		var item struct {
			Type   string `json:"type"`
			Role   string `json:"role"`
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		types = append(types, item.Type+":"+item.Role+":"+item.CallID)
	}
	want := []string{"message:user:", "reasoning::", "message:assistant:", "function_call::call_1", "function_call_output::call_1"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("input types %v raw %s", types, input)
	}
}

func TestPostJSONRetryStatusesAndScrub(t *testing.T) {
	const key = "retry-secret-key"
	t.Run("429 and 5xx retry then succeed", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			if n <= 2 {
				status := http.StatusTooManyRequests
				if n == 2 {
					status = http.StatusServiceUnavailable
				}
				http.Error(w, "upstream said "+key, status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		}))
		defer srv.Close()
		body, err := postJSONWithRetry(context.Background(), postJSONConfig{
			URL: srv.URL, APIKey: key, AuthHeader: "Authorization", AuthScheme: "Bearer ", Body: []byte(`{}`), Client: srv.Client(), MaxRetries: 2, RetryBaseDelay: time.Millisecond,
		})
		if err != nil || string(body) != `{"ok":true}` || calls.Load() != 3 {
			t.Fatalf("body=%s err=%v calls=%d", body, err, calls.Load())
		}
	})
	t.Run("400 does not retry and scrubs key", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Error(w, "bad key "+key, http.StatusBadRequest)
		}))
		defer srv.Close()
		_, err := postJSONWithRetry(context.Background(), postJSONConfig{
			URL: srv.URL, APIKey: key, AuthHeader: "Authorization", AuthScheme: "Bearer ", Body: []byte(`{}`), Client: srv.Client(), MaxRetries: 2, RetryBaseDelay: time.Millisecond,
		})
		var statusErr *HTTPStatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("err=%v", err)
		}
		if calls.Load() != 1 || strings.Contains(err.Error(), key) {
			t.Fatalf("calls=%d err=%v", calls.Load(), err)
		}
	})
}
