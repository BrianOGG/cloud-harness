package provider

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

// postJSONConfig describes one stdlib HTTP JSON call with bounded retries for
// rate limits and upstream 5xx responses. SingleAttempt callers (for example
// ProfileProvider) disable the internal retry loop and receive HTTPStatusError
// immediately so the outer policy can retry or fall back.
type postJSONConfig struct {
	URL            string
	APIKey         string
	AuthHeader     string
	AuthScheme     string
	Headers        map[string]string
	Body           []byte
	Client         *http.Client
	MaxRetries     int
	RetryBaseDelay time.Duration
	SingleAttempt  bool
}

func postJSONWithRetry(ctx context.Context, cfg postJSONConfig) ([]byte, error) {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 120 * time.Second}
	}
	base := cfg.RetryBaseDelay
	if base <= 0 {
		base = 500 * time.Millisecond
	}
	maxRetries := cfg.MaxRetries
	if cfg.SingleAttempt {
		maxRetries = 0
	} else if maxRetries <= 0 {
		maxRetries = 2
	}
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := sleepRetryWithJitter(ctx, base, attempt-1); err != nil {
				return nil, domain.E(domain.CodeProviderHTTP, scrubProviderKey(err.Error(), cfg.APIKey))
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(cfg.Body))
		if err != nil {
			return nil, domain.E(domain.CodeProviderHTTP, scrubProviderKey(err.Error(), cfg.APIKey))
		}
		for k, v := range cfg.Headers {
			req.Header.Set(k, v)
		}
		if cfg.AuthHeader != "" {
			req.Header.Set(cfg.AuthHeader, cfg.AuthScheme+cfg.APIKey)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := cfg.Client.Do(req)
		if err != nil {
			return nil, domain.E(domain.CodeProviderHTTP, scrubProviderKey(err.Error(), cfg.APIKey))
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
			_ = res.Body.Close()
			message := truncateProviderMessage(scrubProviderKey(string(body), cfg.APIKey), 400)
			statusErr := NewHTTPStatusError(res.StatusCode, "HTTP "+strconv.Itoa(res.StatusCode)+": "+message)
			if isRetryableHTTPStatus(res.StatusCode) && attempt < maxRetries && ctx.Err() == nil {
				continue
			}
			return nil, statusErr
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		_ = res.Body.Close()
		if err != nil {
			return nil, domain.E(domain.CodeProviderHTTP, scrubProviderKey(err.Error(), cfg.APIKey))
		}
		return body, nil
	}
}

func isRetryableHTTPStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500 && code <= 599
}

func sleepRetryWithJitter(ctx context.Context, base time.Duration, retryIndex int) error {
	if retryIndex < 0 {
		retryIndex = 0
	}
	if retryIndex > 8 {
		retryIndex = 8
	}
	delay := base * time.Duration(1<<retryIndex)
	jitter := 0.5 + rand.Float64()*0.5
	wait := time.Duration(float64(delay) * jitter)
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func scrubProviderKey(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[redacted]")
}

func truncateProviderMessage(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
