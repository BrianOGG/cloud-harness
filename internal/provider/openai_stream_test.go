package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestStreamAssemblesMultipleToolCalls(t *testing.T) {
	const key = "sk-stream-test"
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if strings.Contains(string(b), key) {
			t.Error("api key was written into the request body")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl_tools","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"alpha.txt\"}"}}]}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"beta.txt\"}"}}]}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"call_c","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"gamma.txt\"}"}}]}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	p := &OpenAICompatible{BaseURL: srv.URL, APIKey: key, Model: "agnes-2.5-flash", Client: srv.Client()}
	resp, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "read three"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Complete {
		t.Fatal("stream with finish_reason was incomplete")
	}
	calls := resp.ToolCalls()
	if len(calls) != 3 || calls[0].ProviderCallID != "call_a" || calls[1].ProviderCallID != "call_b" || calls[2].ProviderCallID != "call_c" {
		t.Fatalf("calls %+v", calls)
	}
	if string(calls[0].Arguments) != `{"path":"alpha.txt"}` || calls[2].Name != "read_file" {
		t.Fatalf("args %+v", calls)
	}
}

func TestNonStreamingMultipleToolCallsAndOpaqueReplay(t *testing.T) {
	const key = "sk-body-test"
	payload := []byte("SECRET-REASONING-BYTES")
	sig := []byte("SIG-OAI-9")
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
		  "id":"chatcmpl_json",
		  "choices":[{
		    "finish_reason":"tool_calls",
		    "message":{
		      "role":"assistant",
		      "reasoning_content":"hidden-thought",
		      "tool_calls":[
		        {"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"alpha.txt\"}"}},
		        {"id":"call_2","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"beta.txt\"}"}},
		        {"id":"call_3","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"gamma.txt\"}"}}
		      ]
		    }
		  }]
		}`)
	}))
	defer srv.Close()
	p := &OpenAICompatible{BaseURL: srv.URL, APIKey: key, Model: "m", Client: srv.Client(), PrivatePassthrough: true}
	resp, err := p.Complete(context.Background(), Request{
		Messages:             []Message{{Role: "user", Content: "go"}},
		ContinuationProvider: "openai",
		Continuation: []domain.ResponseItem{{
			Ordinal: 0,
			Kind:    domain.ItemOpaque,
			Opaque:  &domain.OpaqueItem{Provider: "openai", Kind: "reasoning", Payload: payload, Signature: sig},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls()) != 3 || resp.ToolCalls()[0].ProviderCallID != "call_1" || resp.ToolCalls()[2].ProviderCallID != "call_3" {
		t.Fatalf("calls %+v", resp.ToolCalls())
	}
	if resp.Items[0].Kind != domain.ItemOpaque || string(resp.Items[0].Opaque.Payload) != "hidden-thought" {
		t.Fatalf("reasoning not first opaque item: %+v", resp.Items[0])
	}
	if !strings.Contains(got, base64.StdEncoding.EncodeToString(payload)) || !strings.Contains(got, base64.StdEncoding.EncodeToString(sig)) {
		t.Fatal("opaque continuation was not replayed byte-for-byte")
	}
	if strings.Contains(got, string(payload)) {
		t.Fatal("payload was rewritten into the body as text")
	}
	_ = json.Valid([]byte(got))
}

func TestIncompleteStreamIsNotDispatchable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl_cut","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`+"\n\n")
	}))
	defer srv.Close()
	p := &OpenAICompatible{BaseURL: srv.URL, APIKey: "sk-cut", Model: "m", Client: srv.Client()}
	resp, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Complete {
		t.Fatal("cut stream was marked complete")
	}
	if _, err := domain.PlanBatch("run", resp, nil); err == nil {
		t.Fatal("incomplete response was planned as a batch")
	}
}

func TestProviderErrorRedactsAuthorization(t *testing.T) {
	const key = "sk-live-secret-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad Authorization: Bearer "+key, http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := &OpenAICompatible{BaseURL: srv.URL, APIKey: key, Model: "m", Client: srv.Client()}
	_, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil {
		t.Fatal("expected http error")
	}
	if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "Bearer "+key) {
		t.Fatalf("secret leaked: %v", err)
	}
}

func TestOpenAIRequestOmitsOpaqueTopLevelByDefault(t *testing.T) {
	const key = "sk-opaque-default"
	const reasoning = "step-by-step hidden reasoning for tool call"
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "text/event-stream")
		if len(bodies) == 1 {
			_, _ = io.WriteString(w, `data: {"id":"chatcmpl_1","choices":[{"index":0,"delta":{"reasoning_content":"`+reasoning+`","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"alpha.txt\"}"}}]}}]}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl_2","choices":[{"index":0,"delta":{"content":"done"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := &OpenAICompatible{BaseURL: srv.URL, APIKey: key, Model: "gpt-test", Client: srv.Client()}
	first, err := p.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "read alpha.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Trace and history projections must never contain the reasoning content.
	traceRaw, _ := json.Marshal(domain.TraceView(first.Items))
	if strings.Contains(string(traceRaw), reasoning) || strings.Contains(domain.IndexableText(first.Items), reasoning) {
		t.Fatalf("reasoning leaked into trace or indexable text")
	}

	// Second call replays continuation from the first response alongside the assistant tool-call message.
	_, err = p.Complete(context.Background(), Request{
		Messages: []Message{
			{Role: "user", Content: "read alpha.txt"},
			{Role: "assistant", Content: first.Text(), ToolCalls: first.ToolCalls()},
			{Role: "tool", ToolCallID: "call_1", Content: `{"ok":true,"data":"alpha"}`},
		},
		ContinuationProvider: first.Provider,
		Continuation:         first.Items,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(bodies))
	}
	secondBody := bodies[1]
	if strings.Contains(secondBody, "cloud_harness_opaque") {
		t.Fatalf("request body contained non-standard cloud_harness_opaque field: %s", secondBody)
	}
	var parsed struct {
		Messages []struct {
			Role             string `json:"role"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(secondBody), &parsed); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	foundAssistant := false
	for _, m := range parsed.Messages {
		if m.Role == "assistant" {
			foundAssistant = true
			if m.ReasoningContent != reasoning {
				t.Fatalf("assistant reasoning_content = %q, want %q (body: %s)", m.ReasoningContent, reasoning, secondBody)
			}
		}
	}
	if !foundAssistant {
		t.Fatalf("no assistant message found in request body: %s", secondBody)
	}
}

func TestOpenAIReasoningReplayOmitMode(t *testing.T) {
	const key = "sk-opaque-omit"
	const reasoning = "secret-reasoning-to-omit"
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl_omit","choices":[{"index":0,"delta":{"content":"ok"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := &OpenAICompatible{
		BaseURL:         srv.URL,
		APIKey:          key,
		Model:           "gpt-test",
		Client:          srv.Client(),
		ReasoningReplay: ReasoningReplayOmit,
	}
	_, err := p.Complete(context.Background(), Request{
		Messages: []Message{
			{Role: "user", Content: "read alpha.txt"},
			{Role: "assistant", ToolCalls: []domain.ToolCall{{ProviderCallID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"alpha.txt"}`)}}},
			{Role: "tool", ToolCallID: "call_1", Content: `{"ok":true}`},
		},
		ContinuationProvider: "openai",
		Continuation: []domain.ResponseItem{
			{
				Ordinal: 0,
				Kind:    domain.ItemOpaque,
				Opaque:  &domain.OpaqueItem{Provider: "openai", Kind: "reasoning_content", Payload: []byte(reasoning)},
			},
			{
				Ordinal:  1,
				Kind:     domain.ItemToolCall,
				ToolCall: &domain.ToolCall{ProviderCallID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"alpha.txt"}`)},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotBody, "cloud_harness_opaque") {
		t.Fatalf("omit mode sent cloud_harness_opaque: %s", gotBody)
	}
	if strings.Contains(gotBody, "reasoning_content") || strings.Contains(gotBody, reasoning) {
		t.Fatalf("omit mode did not drop reasoning content: %s", gotBody)
	}
}

func TestOpenAIStreamingAssemblesAndFiresDelta(t *testing.T) {
	const key = "sk-stream-delta"
	chunks := []string{"Hello", ", ", "streaming", " world!"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for i, c := range chunks {
			if i > 0 {
				time.Sleep(10 * time.Millisecond)
			}
			payload, _ := json.Marshal(map[string]any{
				"id": "chatcmpl_stream",
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{"content": c},
				}},
			})
			_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	var mu sync.Mutex
	var gotDeltas []string
	p := &OpenAICompatible{
		BaseURL: srv.URL,
		APIKey:  key,
		Model:   "gpt-test",
		Client:  srv.Client(),
	}
	resp, err := p.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "greet"}},
		OnDelta: func(d string) {
			mu.Lock()
			gotDeltas = append(gotDeltas, d)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Complete || resp.Text() != "Hello, streaming world!" {
		t.Fatalf("unexpected assembled response: complete=%v text=%q", resp.Complete, resp.Text())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotDeltas) != len(chunks) {
		t.Fatalf("got %d deltas %v, want %d %v", len(gotDeltas), gotDeltas, len(chunks), chunks)
	}
	for i := range chunks {
		if gotDeltas[i] != chunks[i] {
			t.Fatalf("delta[%d] = %q, want %q", i, gotDeltas[i], chunks[i])
		}
	}
}

func TestOpenAIRetriesOn429ThenSucceeds(t *testing.T) {
	const key = "sk-retry-429"
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limit exceeded"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl_ok","choices":[{"index":0,"delta":{"content":"recovered"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := &OpenAICompatible{
		BaseURL:        srv.URL,
		APIKey:         key,
		Model:          "gpt-test",
		Client:         srv.Client(),
		RetryBaseDelay: 5 * time.Millisecond,
	}
	resp, err := p.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "ping"}},
	})
	if err != nil {
		t.Fatalf("expected retry to succeed, got error: %v", err)
	}
	if resp.Text() != "recovered" || !resp.Complete {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("expected exactly 2 requests, got %d", got)
	}
}

func TestOpenAINoRetryOn400(t *testing.T) {
	const key = "sk-no-retry-400"
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid request payload"}}`)
	}))
	defer srv.Close()

	p := &OpenAICompatible{
		BaseURL:        srv.URL,
		APIKey:         key,
		Model:          "gpt-test",
		Client:         srv.Client(),
		RetryBaseDelay: 5 * time.Millisecond,
	}
	_, err := p.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "ping"}},
	})
	if err == nil || !domain.IsCode(err, domain.CodeProviderHTTP) {
		t.Fatalf("expected CodeProviderHTTP error on 400, got: %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request on HTTP 400, got %d", got)
	}
}

func TestOpenAIStreamTimeoutsAndNoRetryAfterStreamBegins(t *testing.T) {
	const key = "sk-timeout-test"

	// 1. Steady stream whose total duration exceeds both ResponseHeaderTimeout and IdleTimeout succeeds.
	srvSteady := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		for _, tok := range []string{"a", "b", "c", "d", "e"} {
			time.Sleep(25 * time.Millisecond)
			_, _ = io.WriteString(w, `data: {"id":"chatcmpl_steady","choices":[{"index":0,"delta":{"content":"`+tok+`"}}]}`+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"stop","delta":{}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srvSteady.Close()

	pSteady := &OpenAICompatible{
		BaseURL:               srvSteady.URL,
		APIKey:                key,
		Model:                 "gpt-test",
		Client:                srvSteady.Client(),
		ResponseHeaderTimeout: 60 * time.Millisecond,
		IdleTimeout:           60 * time.Millisecond,
	}
	resp, err := pSteady.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "steady"}},
	})
	if err != nil {
		t.Fatalf("steady stream failed despite active reads: %v", err)
	}
	if resp.Text() != "abcde" {
		t.Fatalf("got %q, want abcde", resp.Text())
	}

	// 2. Stalled stream exceeds IdleTimeout and does NOT retry because streaming already began.
	var stalledAttempts atomic.Int32
	srvStalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stalledAttempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl_stall","choices":[{"index":0,"delta":{"content":"first"}}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(150 * time.Millisecond)
	}))
	defer srvStalled.Close()

	pStalled := &OpenAICompatible{
		BaseURL:               srvStalled.URL,
		APIKey:                key,
		Model:                 "gpt-test",
		Client:                srvStalled.Client(),
		ResponseHeaderTimeout: 60 * time.Millisecond,
		IdleTimeout:           40 * time.Millisecond,
		RetryBaseDelay:        5 * time.Millisecond,
	}
	_, err = pStalled.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "stall"}},
	})
	if err == nil || !domain.IsCode(err, domain.CodeProviderHTTP) {
		t.Fatalf("expected idle timeout error, got %v", err)
	}
	if got := stalledAttempts.Load(); got != 1 {
		t.Fatalf("expected no retry after streaming began, got %d attempts", got)
	}
}
