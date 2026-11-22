package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func TestOpenAICompatibleOneCall(t *testing.T) {
	const key = "sk-live-secret-value"
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if strings.Contains(r.URL.String(), key) {
			t.Error("api key leaked into the URL")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","choices":[{"message":{"role":"assistant","content":"pong"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`)
	}))
	defer srv.Close()
	p := &OpenAICompatible{BaseURL: srv.URL, APIKey: key, Model: "gpt-test", Client: srv.Client()}
	resp, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "ping"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "pong" || !resp.Complete || resp.Provider != "openai" {
		t.Fatalf("%+v", resp)
	}
	var usage domain.Usage
	for _, it := range resp.Items {
		if it.Kind == domain.ItemUsage && it.Usage != nil {
			usage = *it.Usage
		}
	}
	if usage.InputTokens != 3 || usage.OutputTokens != 1 || usage.CachedInputTokens != 2 {
		t.Fatalf("usage %+v", usage)
	}
	if gotAuth != "Bearer "+key {
		t.Fatalf("auth %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"ping"`) || strings.Contains(gotBody, key) {
		t.Fatalf("body %s", gotBody)
	}
}

func TestOpenAIKeyRequired(t *testing.T) {
	p := &OpenAICompatible{BaseURL: "https://example.invalid", Model: "m"}
	_, err := p.Complete(context.Background(), Request{})
	if !domain.IsCode(err, domain.CodeProviderConfig) {
		t.Fatalf("%v", err)
	}
}

func TestOpenAIProbeUsesModelsEndpoint(t *testing.T) {
	const key = "sk-probe-test"
	var gotMethod, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	p := &OpenAICompatible{BaseURL: srv.URL, APIKey: key, Client: srv.Client()}
	if err := p.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet || gotPath != "/models" || gotAuth != "Bearer "+key {
		t.Fatalf("request %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}

	if err := (&OpenAICompatible{BaseURL: srv.URL}).Probe(context.Background()); !domain.IsCode(err, domain.CodeProviderConfig) {
		t.Fatalf("missing key error %v", err)
	}
}

func TestOpenAIErrorScrubsKey(t *testing.T) {
	const key = "sk-live-secret-value"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key "+key, http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := &OpenAICompatible{BaseURL: srv.URL, APIKey: key, Model: "m", Client: srv.Client()}
	_, err := p.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("key leaked in error: %v", err)
	}
}
