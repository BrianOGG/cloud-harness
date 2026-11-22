package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

type profileScript func(context.Context, Request) (domain.ModelResponse, error)

func (profileScript) Name() string { return "script" }

func (s profileScript) Complete(ctx context.Context, req Request) (domain.ModelResponse, error) {
	return s(ctx, req)
}

func TestProfileProviderBackoffAndSwitch(t *testing.T) {
	for _, tc := range []struct {
		status int
		reason string
	}{
		{http.StatusTooManyRequests, "rate_limited"},
		{http.StatusInternalServerError, "upstream_5xx"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var models []string
				var times []time.Duration
				var admitted []bool
				var events []FallbackEvent
				p := &ProfileProvider{
					Role: "executor", Model: "primary", FallbackModel: "backup",
					Provider: profileScript(func(_ context.Context, req Request) (domain.ModelResponse, error) {
						models = append(models, req.Model)
						times = append(times, time.Since(start))
						if len(models) != len(admitted) {
							t.Fatal("provider attempt was not admitted first")
						}
						if req.Model == "primary" {
							return domain.ModelResponse{}, NewHTTPStatusError(tc.status, "test upstream error")
						}
						return domain.ModelResponse{Complete: true}, nil
					}),
					BeforeAttempt: func(_ context.Context, additional bool) error {
						admitted = append(admitted, additional)
						return nil
					},
					OnFallback: func(_ context.Context, ev FallbackEvent) error {
						events = append(events, ev)
						return nil
					},
				}
				for range 2 {
					if resp, err := p.Complete(t.Context(), Request{}); err != nil || !resp.Complete {
						t.Fatalf("Complete = %+v, %v", resp, err)
					}
				}
				if want := []string{"primary", "primary", "primary", "primary", "backup", "backup"}; !slices.Equal(models, want) {
					t.Errorf("models = %v; want %v", models, want)
				}
				s := time.Second
				if want := []time.Duration{0, s, 3 * s, 7 * s, 7 * s, 7 * s}; !slices.Equal(times, want) {
					t.Errorf("attempt times = %v; want %v", times, want)
				}
				if want := []bool{false, true, true, true, true, false}; !slices.Equal(admitted, want) {
					t.Errorf("additional attempts = %v; want %v", admitted, want)
				}
				if want := []FallbackEvent{{Role: "executor", FromModel: "primary", ToModel: "backup", Reason: tc.reason}}; !slices.Equal(events, want) {
					t.Errorf("switch events = %+v; want %+v", events, want)
				}
			})
		})
	}
	if d := profileRetryDelay(10); d != 8*time.Second {
		t.Errorf("maximum retry delay = %v; want 8s", d)
	}
}

func TestProfileProviderCancelsBackoffWithoutAdmittingAnotherAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		admitted := 0
		p := &ProfileProvider{
			Model: "primary", FallbackModel: "backup",
			Provider: profileScript(func(context.Context, Request) (domain.ModelResponse, error) {
				return domain.ModelResponse{}, NewHTTPStatusError(http.StatusTooManyRequests, "rate limited")
			}),
			BeforeAttempt: func(context.Context, bool) error { admitted++; return nil },
		}
		start := time.Now()
		done := make(chan error, 1)
		go func() {
			_, err := p.Complete(ctx, Request{})
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || admitted != 1 || time.Since(start) != 0 {
			t.Errorf("error = %v, admitted = %d, elapsed = %v", err, admitted, time.Since(start))
		}
	})
}

func TestProfileProviderDoesNotRetryAfterSSEOutput(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"first"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, `data: {"error":{"message":"429 rate limit"}}`+"\n\n")
	}))
	defer srv.Close()
	p := &ProfileProvider{
		Provider: &OpenAICompatible{BaseURL: srv.URL, APIKey: "fixture", Client: srv.Client()},
		Model:    "primary", FallbackModel: "backup",
		OnFallback: func(context.Context, FallbackEvent) error {
			t.Error("stream error triggered a model switch")
			return nil
		},
	}
	var deltas []string
	_, err := p.Complete(t.Context(), Request{OnDelta: func(s string) { deltas = append(deltas, s) }})
	if !domain.IsCode(err, domain.CodeProviderHTTP) || calls.Load() != 1 || !slices.Equal(deltas, []string{"first"}) {
		t.Errorf("error = %v, calls = %d, deltas = %v", err, calls.Load(), deltas)
	}
}
