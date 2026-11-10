package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/domain"
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
		io.WriteString(w, `data: {"id":"chatcmpl_tools","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"alpha.txt\"}"}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"beta.txt\"}"}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":2,"id":"call_c","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"gamma.txt\"}"}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"index":0,"finish_reason":"tool_calls","delta":{}}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
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
		io.WriteString(w, `{
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
	p := &OpenAICompatible{BaseURL: srv.URL, APIKey: key, Model: "m", Client: srv.Client()}
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
		io.WriteString(w, `data: {"id":"chatcmpl_cut","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}`+"\n\n")
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
