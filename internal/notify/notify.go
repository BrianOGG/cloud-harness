// Package notify provides asynchronous run notification channels.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// Event is the deliberately small, secret-free public notification payload.
type Event struct {
	RunID       string    `json:"run_id"`
	EventType   string    `json:"event_type"`
	Status      string    `json:"status"`
	Summary     string    `json:"summary,omitempty"`
	ApprovalURL string    `json:"approval_url,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
}

const (
	EventWaitingApproval = "approval"
	EventAskUser         = "ask_user"
	EventCompleted       = "completion"
	EventFailed          = "failure"
)

type Channel interface {
	Send(context.Context, Event) error
}
type Notifier interface {
	Notify(context.Context, Event) <-chan error
}

type DeliveryError struct {
	Channel string
	Target  string
	Err     error
}

func (e DeliveryError) Error() string {
	return fmt.Sprintf("channel=%s target=%s: %v", e.Channel, e.Target, e.Err)
}

type Config struct {
	Channels        []ChannelConfig `json:"channels,omitempty"`
	ApprovalBaseURL string          `json:"approval_base_url,omitempty"`
}
type ChannelConfig struct {
	Type       string        `json:"type"`
	URL        string        `json:"url"`
	Events     []string      `json:"events,omitempty"`
	TimeoutMS  int           `json:"timeout_ms,omitempty"`
	Timeout    time.Duration `json:"-"`
	Credential string        `json:"-"`
}

type notifier struct{ channels []configuredChannel }
type configuredChannel struct {
	name    string
	events  map[string]bool
	timeout time.Duration
	ch      Channel
	target  string
}

// New validates all channel types and endpoints before process startup.
func New(cfg Config) (Notifier, error) {
	if cfg.ApprovalBaseURL != "" {
		u, err := url.Parse(cfg.ApprovalBaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("notify approval_base_url must be http or https")
		}
	}
	if len(cfg.Channels) == 0 {
		return nil, nil
	}
	n := &notifier{}
	for i, c := range cfg.Channels {
		typ := strings.ToLower(strings.TrimSpace(c.Type))
		if typ == "" {
			return nil, fmt.Errorf("notify channel %d: type is required", i+1)
		}
		if typ == "osc9" || typ == "desktop" {
			if c.URL != "" {
				return nil, fmt.Errorf("notify channel %d: %s does not accept url", i+1, typ)
			}
			if c.Credential != "" {
				return nil, fmt.Errorf("notify channel %d: %s does not accept credentials", i+1, typ)
			}
		} else {
			u, err := url.Parse(c.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return nil, fmt.Errorf("notify channel %d: url must be http or https", i+1)
			}
		}
		if c.TimeoutMS < 0 {
			return nil, fmt.Errorf("notify channel %d: timeout_ms must not be negative", i+1)
		}
		if int64(c.TimeoutMS) > (1<<63-1)/int64(time.Millisecond) {
			return nil, fmt.Errorf("notify channel %d: timeout_ms is too large", i+1)
		}
		if c.Timeout <= 0 {
			if c.TimeoutMS > 0 {
				c.Timeout = time.Duration(c.TimeoutMS) * time.Millisecond
			} else {
				c.Timeout = 5 * time.Second
			}
		}
		var ch Channel
		switch typ {
		case "webhook":
			ch = &webhook{url: c.URL, bearer: c.Credential}
		case "bark":
			if c.Credential == "" {
				return nil, fmt.Errorf("notify channel %d: bark key is missing", i+1)
			}
			ch = &bark{base: strings.TrimRight(c.URL, "/"), key: c.Credential}
		case "ntfy":
			ch = &ntfy{url: strings.TrimRight(c.URL, "/"), token: c.Credential}
		case "osc9":
			ch = &osc9{}
		case "desktop":
			var err error
			ch, err = newDesktop()
			if err != nil {
				return nil, fmt.Errorf("notify channel %d: %w", i+1, err)
			}
		default:
			return nil, fmt.Errorf("notify channel %d: unknown type %q", i+1, typ)
		}
		events := map[string]bool{}
		for _, event := range c.Events {
			event = strings.ToLower(strings.TrimSpace(event))
			if !validEvent(event) {
				return nil, fmt.Errorf("notify channel %d: unknown event %q", i+1, event)
			}
			events[event] = true
		}
		n.channels = append(n.channels, configuredChannel{
			name: typ, events: events, timeout: c.Timeout, ch: ch, target: safeTarget(c.URL),
		})
	}
	return n, nil
}

func validEvent(e string) bool {
	switch e {
	case EventWaitingApproval, EventAskUser, EventCompleted, EventFailed:
		return true
	default:
		return false
	}
}

// SafeTarget returns only a URL's host and path for diagnostics.
func SafeTarget(raw string) string { return safeTarget(raw) }

// Notify starts selected channels concurrently and returns their failures on a
// buffered result stream. It never waits for channel delivery.
func (n *notifier) Notify(ctx context.Context, e Event) <-chan error {
	var selected []configuredChannel
	for _, cc := range n.channels {
		if len(cc.events) == 0 || cc.events[e.EventType] {
			selected = append(selected, cc)
		}
	}
	results := make(chan error, len(selected))
	var wg sync.WaitGroup
	for _, cc := range selected {
		cc := cc
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, cc.timeout)
			defer cancel()
			if err := cc.ch.Send(cctx, e); err != nil && ctx.Err() == nil {
				results <- DeliveryError{Channel: cc.name, Target: cc.target, Err: err}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()
	return results
}

type webhook struct{ url, bearer string }

func (w *webhook) Send(ctx context.Context, e Event) error { return postJSON(ctx, w.url, e, w.bearer) }

type ntfy struct{ url, token string }

func (n *ntfy) Send(ctx context.Context, e Event) error { return postJSON(ctx, n.url, e, n.token) }

type bark struct{ base, key string }

func (b *bark) Send(ctx context.Context, e Event) error {
	body := notificationBody(e)
	payload := struct {
		RunID       string    `json:"run_id"`
		EventType   string    `json:"event_type"`
		Status      string    `json:"status"`
		Summary     string    `json:"summary,omitempty"`
		ApprovalURL string    `json:"approval_url,omitempty"`
		OccurredAt  time.Time `json:"occurred_at"`
		Title       string    `json:"title"`
		Body        string    `json:"body"`
		Group       string    `json:"group"`
		URL         string    `json:"url,omitempty"`
	}{e.RunID, e.EventType, e.Status, e.Summary, e.ApprovalURL, e.OccurredAt, e.EventType, body, e.RunID, e.ApprovalURL}
	return postJSON(ctx, b.base+"/"+url.PathEscape(b.key), payload, "")
}

func notificationBody(e Event) string {
	if e.Summary != "" {
		return e.Summary
	}
	return e.EventType + " " + e.Status
}

func postJSON(ctx context.Context, target string, payload any, bearer string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode payload")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request")
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http status %d", resp.StatusCode)
	}
	return nil
}

func safeTarget(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "redacted"
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	return u.Host + path.Clean(p)
}
