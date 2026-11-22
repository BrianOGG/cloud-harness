package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
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
	// ResponseHeaderTimeout and IdleTimeout bound a streamed attempt.
	ResponseHeaderTimeout time.Duration
	IdleTimeout           time.Duration

	stream bool
}

func postJSONWithRetry(ctx context.Context, cfg postJSONConfig) ([]byte, error) {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 120 * time.Second}
	}
	var body []byte
	err := postWithRetry(ctx, cfg, func(res *http.Response, r io.Reader) error {
		var err error
		body, err = io.ReadAll(io.LimitReader(r, 8<<20))
		if err != nil {
			return domain.E(domain.CodeProviderHTTP, scrubProviderKey(err.Error(), cfg.APIKey))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// postStreamWithRetry posts cfg.Body and hands the 2xx response to consume
// while the body is still arriving. There is no total timeout: an attempt is
// cut when the response headers or the next read take too long. A rejected
// status is retried as in postJSONWithRetry. consume may also return an
// HTTPStatusError for a failure the upstream reported inside the stream; it
// is retried the same way, so consume must only do that before it has handed
// any output to the caller.
func postStreamWithRetry(ctx context.Context, cfg postJSONConfig, consume func(res *http.Response, body io.Reader) error) error {
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	cfg.stream = true
	return postWithRetry(ctx, cfg, consume)
}

func postWithRetry(ctx context.Context, cfg postJSONConfig, consume func(res *http.Response, body io.Reader) error) error {
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
				return domain.E(domain.CodeProviderHTTP, scrubProviderKey(err.Error(), cfg.APIKey))
			}
		}
		err := postOnce(ctx, cfg, consume)
		var statusErr *HTTPStatusError
		if errors.As(err, &statusErr) && isRetryableHTTPStatus(statusErr.StatusCode) && attempt < maxRetries && ctx.Err() == nil {
			continue
		}
		return err
	}
}

func postOnce(ctx context.Context, cfg postJSONConfig, consume func(res *http.Response, body io.Reader) error) error {
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, cfg.URL, bytes.NewReader(cfg.Body))
	if err != nil {
		return domain.E(domain.CodeProviderHTTP, scrubProviderKey(err.Error(), cfg.APIKey))
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	if cfg.AuthHeader != "" {
		req.Header.Set(cfg.AuthHeader, cfg.AuthScheme+cfg.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	var headerTimedOut atomic.Bool
	var headerTimer *time.Timer
	if cfg.stream {
		req.Header.Set("Accept", "text/event-stream")
		headerTimer = time.AfterFunc(streamTimeout(cfg.ResponseHeaderTimeout, DefaultResponseHeaderTimeout), func() {
			headerTimedOut.Store(true)
			cancel()
		})
	}
	res, err := cfg.Client.Do(req)
	if headerTimer != nil {
		headerTimer.Stop()
	}
	if err != nil {
		if headerTimedOut.Load() && ctx.Err() == nil {
			return domain.E(domain.CodeProviderHTTP, "response header timeout exceeded")
		}
		return domain.E(domain.CodeProviderHTTP, scrubProviderKey(err.Error(), cfg.APIKey))
	}
	defer func() { _ = res.Body.Close() }()
	// A streamed attempt has no total timeout, so every read of its body,
	// the body of a rejected request included, is bounded by the idle timeout.
	var body io.Reader = res.Body
	if cfg.stream {
		body = &idleTimeoutReader{r: res.Body, timeout: streamTimeout(cfg.IdleTimeout, DefaultIdleTimeout), cancel: cancel}
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(body, 4<<20))
		message := truncateProviderMessage(scrubProviderKey(string(raw), cfg.APIKey), 400)
		return NewHTTPStatusError(res.StatusCode, "HTTP "+strconv.Itoa(res.StatusCode)+": "+message)
	}
	return consume(res, body)
}

func streamTimeout(configured, fallback time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return fallback
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
