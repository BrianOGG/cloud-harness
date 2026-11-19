package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type oracleRoundTrip func(*http.Request) (*http.Response, error)

func (f oracleRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestOracleCompletionUsesDefaultModelWithoutEmittingDeltas(t *testing.T) {
	p := &OpenAICompatible{
		BaseURL: "http://fixture.invalid/v1", APIKey: "fixture", Model: "default-model",
		OnDelta: func(string) { t.Error("consultation advice leaked into main model deltas") },
		Client: &http.Client{Transport: oracleRoundTrip(func(req *http.Request) (*http.Response, error) {
			defer req.Body.Close()
			var body chatRequest
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			if body.Model != "default-model" || len(body.Tools) != 0 || len(body.Messages) != 2 || body.Messages[1].Content != "test question" {
				t.Errorf("unexpected consultation request: %+v", body)
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"advice"}}]}`))}, nil
		})},
	}
	resp, err := p.Complete(t.Context(), OracleRequest("test question"))
	if err != nil || !resp.Complete || resp.Text() != "advice" || ModelName(p, "") != p.Model {
		t.Fatalf("consultation response = %+v, error = %v", resp, err)
	}
}

type waitingOracleBody struct{ ctx context.Context }

func (b waitingOracleBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b waitingOracleBody) Close() error { return nil }

func TestOracleCompletionPreservesProviderTimeouts(t *testing.T) {
	for _, stage := range []string{"headers", "stream"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := &OpenAICompatible{
					BaseURL: "http://fixture.invalid/v1", APIKey: "fixture", Model: "test",
					ResponseHeaderTimeout: time.Second, IdleTimeout: time.Second,
					Client: &http.Client{Transport: oracleRoundTrip(func(req *http.Request) (*http.Response, error) {
						defer req.Body.Close()
						if stage == "headers" {
							<-req.Context().Done()
							return nil, req.Context().Err()
						}
						return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
							Body: waitingOracleBody{ctx: req.Context()}}, nil
					})},
				}
				if _, err := p.Complete(t.Context(), OracleRequest("test question")); !IsTimeout(err) {
					t.Fatalf("consultation returned %v for a %s timeout", err, stage)
				}
			})
		})
	}
}
