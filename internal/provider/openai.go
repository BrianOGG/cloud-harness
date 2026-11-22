package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

const (
	DefaultResponseHeaderTimeout = 30 * time.Second
	DefaultIdleTimeout           = 120 * time.Second
	DefaultRetryBaseDelay        = 500 * time.Millisecond
	DefaultMaxRetries            = 2

	ReasoningReplayMessage = "message"
	ReasoningReplayOmit    = "omit"
)

// OpenAICompatible calls a Chat Completions endpoint with the standard library.
// Streams are assembled into one response before the caller can dispatch tools.
// Opaque continuation bytes are replayed according to ReasoningReplay and PrivatePassthrough.
type OpenAICompatible struct {
	BaseURL               string
	APIKey                string
	Model                 string
	Client                *http.Client
	ReasoningReplay       string
	PrivatePassthrough    bool
	OnDelta               func(string)
	ResponseHeaderTimeout time.Duration
	IdleTimeout           time.Duration
	RetryBaseDelay        time.Duration
	MaxRetries            int
	// DisableImages declares a text-only compatible endpoint or model.
	DisableImages bool
}

func (p *OpenAICompatible) Name() string { return "openai" }

func (p *OpenAICompatible) SupportsImages() bool { return p.supportsModelImages(p.Model) }

func (p *OpenAICompatible) supportsModelImages(model string) bool {
	if p.DisableImages {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	// Compatible APIs do not expose a standard modality-discovery endpoint.
	// Reject established text-only OpenAI families locally; other models must
	// accept the image blocks or return a provider error, never a text fallback.
	for _, family := range []string{"gpt-3.5", "gpt-4", "gpt-4-32k", "gpt-4-0314", "gpt-4-0613", "gpt-4-32k-0314", "gpt-4-32k-0613", "gpt-4-1106-preview", "gpt-4-0125-preview", "gpt-4-turbo-preview", "o1-mini", "o1-preview", "o3-mini"} {
		if model == family {
			return false
		}
	}
	return !strings.HasPrefix(model, "gpt-3.5-") && !strings.HasPrefix(model, "o1-mini-") && !strings.HasPrefix(model, "o1-preview-") && !strings.HasPrefix(model, "o3-mini-")
}

// Probe performs the lightest provider connectivity check used by doctor. It
// deliberately calls GET /models instead of a completion, and all returned
// errors are scrubbed so the API key cannot appear in diagnostics.
func (p *OpenAICompatible) Probe(ctx context.Context) error {
	if strings.TrimSpace(p.APIKey) == "" {
		return domain.E(domain.CodeProviderConfig, "OPENAI_API_KEY is not set")
	}
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return domain.E(domain.CodeProviderHTTP, scrub(err.Error(), p.APIKey))
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	client := p.Client
	if client == nil {
		client = &http.Client{}
	}
	res, err := client.Do(req)
	if err != nil {
		return domain.E(domain.CodeProviderHTTP, scrub(err.Error(), p.APIKey))
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return domain.E(domain.CodeProviderHTTP, scrub("GET /models returned "+res.Status+": "+truncate(string(body), 400), p.APIKey))
	}
	return nil
}

func (p *OpenAICompatible) Complete(ctx context.Context, req Request) (_ domain.ModelResponse, completionErr error) {
	var images []Image
	for _, msg := range req.Messages {
		images = append(images, msg.Images...)
	}
	if err := ValidateImages(images); err != nil {
		return domain.ModelResponse{}, err
	}
	if strings.TrimSpace(p.APIKey) == "" {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderConfig, "OPENAI_API_KEY is not set; the mock provider needs no key")
	}
	model := p.Model
	if req.Model != "" {
		model = req.Model
	}
	if model == "" {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderConfig, "OPENAI_MODEL is empty")
	}
	if len(images) > 0 && !p.supportsModelImages(model) {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderConfig, "configured provider/model does not support image input")
	}
	replayMode := strings.ToLower(strings.TrimSpace(p.ReasoningReplay))
	if replayMode == "" {
		replayMode = ReasoningReplayMessage
	}
	if replayMode != ReasoningReplayMessage && replayMode != ReasoningReplayOmit {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderConfig, "invalid openai_reasoning_replay: "+p.ReasoningReplay)
	}
	body := chatRequest{Model: model, Stream: true, StreamOptions: &streamOptions{IncludeUsage: true}}
	names := newWireToolNames(req.Tools)
	for _, msg := range req.Messages {
		cm := chatMessage{Role: msg.Role, ToolCallID: msg.ToolCallID}
		if msg.Content != "" {
			cm.Content = msg.Content
		}
		if len(msg.Images) > 0 {
			content := make([]chatContentBlock, 0, len(msg.Images)+1)
			if msg.Content != "" {
				content = append(content, chatContentBlock{Type: "text", Text: msg.Content})
			}
			for _, image := range msg.Images {
				content = append(content, chatContentBlock{
					Type: "image_url",
					ImageURL: &chatImageURL{
						URL:    "data:" + image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(image.Data),
						Detail: "auto",
					},
				})
			}
			cm.Content = content
		}
		for _, call := range msg.ToolCalls {
			args := string(call.Arguments)
			cm.ToolCalls = append(cm.ToolCalls, chatToolCall{
				ID:   call.ProviderCallID,
				Type: "function",
				Function: struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				}{Name: names.wire(call.Name), Arguments: args},
			})
		}
		body.Messages = append(body.Messages, cm)
	}
	for _, tool := range req.Tools {
		params := tool.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		body.Tools = append(body.Tools, chatTool{
			Type: "function",
			Function: chatFn{
				Name: names.wire(tool.Name), Description: tool.Description, Parameters: params,
			},
		})
	}
	if req.ContinuationProvider == "" || req.ContinuationProvider == "openai" || req.ContinuationProvider == p.Name() {
		var reasoning strings.Builder
		for _, item := range req.Continuation {
			if item.Kind != domain.ItemOpaque || item.Opaque == nil {
				continue
			}
			if item.Opaque.Provider != "" && item.Opaque.Provider != "openai" && item.Opaque.Provider != p.Name() {
				continue
			}
			if item.Opaque.Kind == "reasoning_content" {
				if replayMode == ReasoningReplayOmit {
					continue
				}
				reasoning.Write(item.Opaque.Payload)
			}
			if p.PrivatePassthrough {
				body.Opaque = append(body.Opaque, opaqueWire{
					Ordinal:      item.Ordinal,
					Kind:         item.Opaque.Kind,
					PayloadB64:   base64.StdEncoding.EncodeToString(item.Opaque.Payload),
					SignatureB64: base64.StdEncoding.EncodeToString(item.Opaque.Signature),
				})
			}
		}
		if replayMode == ReasoningReplayMessage && reasoning.Len() > 0 {
			rc := reasoning.String()
			placed := false
			for i := len(body.Messages) - 1; i >= 0; i-- {
				if body.Messages[i].Role == "assistant" {
					body.Messages[i].ReasoningContent = rc
					placed = true
					break
				}
			}
			if !placed {
				body.Messages = append(body.Messages, chatMessage{
					Role:             "assistant",
					ReasoningContent: rc,
				})
			}
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return domain.ModelResponse{}, err
	}
	url := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"

	headerTimeout := p.ResponseHeaderTimeout
	if headerTimeout <= 0 {
		headerTimeout = DefaultResponseHeaderTimeout
	}
	idleTimeout := p.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = DefaultIdleTimeout
	}
	baseDelay := p.RetryBaseDelay
	if baseDelay <= 0 {
		baseDelay = DefaultRetryBaseDelay
	}
	maxRetries := p.MaxRetries
	if req.singleAttempt {
		maxRetries = 0
	} else if maxRetries <= 0 {
		maxRetries = DefaultMaxRetries
	}

	var onDelta func(string)
	switch {
	case req.oracle:
	case p.OnDelta != nil && req.OnDelta != nil:
		onDelta = func(d string) {
			p.OnDelta(d)
			req.OnDelta(d)
		}
	case req.OnDelta != nil:
		onDelta = req.OnDelta
	case p.OnDelta != nil:
		onDelta = p.OnDelta
	}

	client := p.Client
	if client == nil {
		client = &http.Client{}
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepWithJitter(ctx, baseDelay, attempt-1); err != nil {
				return domain.ModelResponse{}, domain.E(domain.CodeProviderHTTP, scrub(err.Error(), p.APIKey))
			}
		}

		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, url, bytes.NewReader(raw))
		if err != nil {
			cancelAttempt()
			return domain.ModelResponse{}, err
		}
		httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
		httpReq.Header.Set("Content-Type", "application/json")

		var headerTimedOut atomic.Bool
		headerTimer := time.AfterFunc(headerTimeout, func() {
			headerTimedOut.Store(true)
			cancelAttempt()
		})
		res, err := client.Do(httpReq)
		headerTimer.Stop()
		if err != nil {
			cancelAttempt()
			if req.oracle && (headerTimedOut.Load() || IsTimeout(err)) {
				return domain.ModelResponse{}, context.DeadlineExceeded
			}
			if headerTimedOut.Load() && ctx.Err() == nil {
				return domain.ModelResponse{}, domain.E(domain.CodeProviderHTTP, "response header timeout exceeded")
			}
			lastErr = domain.E(domain.CodeProviderHTTP, scrub(err.Error(), p.APIKey))
			if ctx.Err() == nil && attempt < maxRetries && isRetryableTransportErr(err) {
				continue
			}
			return domain.ModelResponse{}, lastErr
		}

		if res.StatusCode < 200 || res.StatusCode >= 300 {
			bodyReader := &idleTimeoutReader{r: res.Body, timeout: idleTimeout, cancel: cancelAttempt}
			respBody, _ := io.ReadAll(io.LimitReader(bodyReader, 4<<20))
			_ = res.Body.Close()
			cancelAttempt()
			message := string(respBody)
			if len(images) > 0 {
				message = imageDataURLRE.ReplaceAllString(message, "[image data omitted]")
			}
			message = scrub(truncate(message, 400), p.APIKey)
			if len(images) > 0 {
				message = "provider/model rejected image input: " + message
			}
			if req.singleAttempt {
				return domain.ModelResponse{}, NewHTTPStatusError(res.StatusCode, message)
			}
			lastErr = domain.E(domain.CodeProviderHTTP, message)
			if isRetryableStatus(res.StatusCode) && attempt < maxRetries && ctx.Err() == nil {
				continue
			}
			return domain.ModelResponse{}, lastErr
		}

		defer cancelAttempt()
		defer res.Body.Close()
		bodyReader := &idleTimeoutReader{r: res.Body, timeout: idleTimeout, cancel: cancelAttempt}
		if req.oracle {
			defer func() {
				if completionErr != nil && bodyReader.didTimeout() {
					completionErr = context.DeadlineExceeded
				}
			}()
		}
		scrubber := func(s string) string {
			if len(images) > 0 {
				s = imageDataURLRE.ReplaceAllString(s, "[image data omitted]")
			}
			return scrub(s, p.APIKey)
		}
		ctype := res.Header.Get("Content-Type")
		if strings.Contains(ctype, "text/event-stream") {
			return names.restore(parseSSE(bodyReader, scrubber, onDelta))
		}
		br := bufio.NewReader(bodyReader)
		if !strings.Contains(ctype, "application/json") && looksLikeSSE(br) {
			return names.restore(parseSSE(br, scrubber, onDelta))
		}
		respBody, err := io.ReadAll(io.LimitReader(br, 4<<20))
		if err != nil {
			return domain.ModelResponse{}, domain.E(domain.CodeProviderHTTP, scrub(err.Error(), p.APIKey))
		}
		parsed, err := parseChatJSON(respBody, onDelta)
		if err != nil {
			return domain.ModelResponse{}, err
		}
		return names.restore(parsed, nil)
	}
	return domain.ModelResponse{}, lastErr
}

type idleTimeoutReader struct {
	r        io.ReadCloser
	timeout  time.Duration
	cancel   context.CancelFunc
	mu       sync.Mutex
	timedOut bool
}

func (ir *idleTimeoutReader) Read(p []byte) (int, error) {
	if ir.timeout <= 0 {
		return ir.r.Read(p)
	}
	timer := time.AfterFunc(ir.timeout, func() {
		ir.mu.Lock()
		ir.timedOut = true
		ir.mu.Unlock()
		ir.cancel()
	})
	n, err := ir.r.Read(p)
	timer.Stop()
	if err != nil && ir.didTimeout() {
		return n, domain.E(domain.CodeProviderHTTP, "stream read idle timeout exceeded")
	}
	return n, err
}

func (ir *idleTimeoutReader) didTimeout() bool {
	ir.mu.Lock()
	defer ir.mu.Unlock()
	return ir.timedOut
}

func (ir *idleTimeoutReader) Close() error {
	return ir.r.Close()
}

func looksLikeSSE(br *bufio.Reader) bool {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return false
		}
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
			continue
		}
		_ = br.UnreadByte()
		if b != 'd' {
			return false
		}
		peek, err := br.Peek(5)
		if err != nil {
			return false
		}
		return bytes.Equal(peek, []byte("data:"))
	}
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func isRetryableTransportErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "server closed")
}

func sleepWithJitter(ctx context.Context, base time.Duration, retryIndex int) error {
	if base <= 0 {
		base = DefaultRetryBaseDelay
	}
	delay := base * time.Duration(1<<retryIndex)
	jitter := 0.5 + rand.Float64()*0.5
	wait := time.Duration(float64(delay) * jitter)
	if wait <= 0 {
		wait = time.Millisecond
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Redact removes API keys and bearer tokens from a string that might be logged.
func Redact(s, key string) string { return scrub(s, key) }

func scrub(s, key string) string {
	if key != "" {
		s = strings.ReplaceAll(s, key, "[redacted]")
	}
	s = bearerRE.ReplaceAllString(s, "Bearer [redacted]")
	s = strings.ReplaceAll(s, "Authorization:", "Authorization: [redacted]")
	return s
}

var bearerRE = regexp.MustCompile(`(?i)Bearer\s+\S+`)

var imageDataURLRE = regexp.MustCompile(`(?i)data:image/(?:png|jpeg|webp|gif);base64,[a-z0-9+/=]*`)

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	Tools         []chatTool     `json:"tools,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	Opaque        []opaqueWire   `json:"cloud_harness_opaque,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	PromptDetails    struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u chatUsage) domain() domain.Usage {
	return domain.Usage{
		InputTokens:       u.PromptTokens,
		OutputTokens:      u.CompletionTokens,
		CachedInputTokens: u.PromptDetails.CachedTokens,
	}
}

type opaqueWire struct {
	Ordinal      int    `json:"ordinal"`
	Kind         string `json:"kind"`
	PayloadB64   string `json:"payload_b64"`
	SignatureB64 string `json:"signature_b64,omitempty"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function chatFn `json:"function"`
}

type chatFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type chatContentBlock struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}
