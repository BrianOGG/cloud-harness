package provider

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

const (
	profileMaxRetries = 3
	profileMaxDelay   = 8 * time.Second
)

// HTTPStatusError identifies a rejected HTTP response before any model output.
// Read errors and error frames within a response stream use ordinary errors.
type HTTPStatusError struct {
	StatusCode int
	cause      *domain.Error
}

func NewHTTPStatusError(statusCode int, message string) *HTTPStatusError {
	return &HTTPStatusError{StatusCode: statusCode, cause: domain.E(domain.CodeProviderHTTP, message)}
}

func (e *HTTPStatusError) Error() string { return e.cause.Error() }
func (e *HTTPStatusError) Unwrap() error { return e.cause }

const FallbackEventType = "provider.fallback"

// FallbackEvent includes only the route and the category of upstream failure.
type FallbackEvent struct {
	Role      string `json:"role"`
	FromModel string `json:"from_model"`
	ToModel   string `json:"to_model"`
	Reason    string `json:"reason"`
}

// ProfileProvider owns retries and the active model for calls made by one
// sequential role in a run. The caller restores a previous switch when it
// creates a new instance (for example, upon resuming a waiting run).
type ProfileProvider struct {
	Provider
	Role          string
	Model         string
	FallbackModel string
	// BeforeAttempt can deny a call before any request is sent. additional is
	// true for each retry and the first fallback attempt within a completion.
	BeforeAttempt func(ctx context.Context, additional bool) error
	OnFallback    func(context.Context, FallbackEvent) error
}

func (p *ProfileProvider) Complete(ctx context.Context, req Request) (domain.ModelResponse, error) {
	req.Model = p.Model
	req.singleAttempt = true
	for retry := 0; ; retry++ {
		if retry > 0 {
			if err := waitForRetry(ctx, profileRetryDelay(retry-1)); err != nil {
				return domain.ModelResponse{}, err
			}
		}
		if err := p.beforeAttempt(ctx, retry > 0); err != nil {
			return domain.ModelResponse{}, err
		}
		resp, err := p.Provider.Complete(ctx, req)
		reason := retryReason(err)
		if reason == "" || len(resp.Items) > 0 || ctx.Err() != nil {
			return resp, completionError(err)
		}
		if retry < profileMaxRetries {
			continue
		}
		if p.FallbackModel == "" || p.FallbackModel == p.Model {
			return resp, completionError(err)
		}
		// Remember the switch even if the budget requires a wait before the
		// backup can be called. A resumed run can then start on the backup.
		if p.OnFallback != nil {
			ev := FallbackEvent{Role: p.Role, FromModel: p.Model, ToModel: p.FallbackModel, Reason: reason}
			if err := p.OnFallback(ctx, ev); err != nil {
				return domain.ModelResponse{}, err
			}
		}
		p.Model, p.FallbackModel = p.FallbackModel, ""
		if err := p.beforeAttempt(ctx, true); err != nil {
			return domain.ModelResponse{}, err
		}
		req.Model = p.Model
		resp, err = p.Provider.Complete(ctx, req)
		return resp, completionError(err)
	}
}

func (p *ProfileProvider) beforeAttempt(ctx context.Context, additional bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.BeforeAttempt != nil {
		return p.BeforeAttempt(ctx, additional)
	}
	return nil
}

func retryReason(err error) string {
	var status *HTTPStatusError
	if errors.As(err, &status) {
		switch {
		case status.StatusCode == http.StatusTooManyRequests:
			return "rate_limited"
		case status.StatusCode >= 500 && status.StatusCode < 600:
			return "upstream_5xx"
		}
	}
	return ""
}

func completionError(err error) error {
	var status *HTTPStatusError
	if errors.As(err, &status) {
		return status.cause
	}
	return err
}

func profileRetryDelay(retry int) time.Duration {
	delay := time.Second
	for i := 0; i < retry && delay < profileMaxDelay; i++ {
		delay *= 2
	}
	return delay
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
