package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

// OpenAICompatible calls a Chat Completions endpoint with the standard library.
// Streams are assembled into one response before the caller can dispatch tools.
// Opaque continuation bytes are sent back in order when the same provider is used.
type OpenAICompatible struct {
	BaseURL string
	APIKey  string
	Model   string
	Client  *http.Client
}

func (p *OpenAICompatible) Name() string { return "openai" }

func (p *OpenAICompatible) Complete(ctx context.Context, req Request) (domain.ModelResponse, error) {
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
	body := chatRequest{Model: model, Stream: true, StreamOptions: &streamOptions{IncludeUsage: true}}
	for _, msg := range req.Messages {
		cm := chatMessage{Role: msg.Role, Content: msg.Content, ToolCallID: msg.ToolCallID}
		for _, call := range msg.ToolCalls {
			args := string(call.Arguments)
			cm.ToolCalls = append(cm.ToolCalls, chatToolCall{
				ID:   call.ProviderCallID,
				Type: "function",
				Function: struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				}{Name: call.Name, Arguments: args},
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
				Name: tool.Name, Description: tool.Description, Parameters: params,
			},
		})
	}
	if req.ContinuationProvider == "" || req.ContinuationProvider == "openai" || req.ContinuationProvider == p.Name() {
		for _, item := range req.Continuation {
			if item.Kind != domain.ItemOpaque || item.Opaque == nil {
				continue
			}
			body.Opaque = append(body.Opaque, opaqueWire{
				Ordinal:      item.Ordinal,
				Kind:         item.Opaque.Kind,
				PayloadB64:   base64.StdEncoding.EncodeToString(item.Opaque.Payload),
				SignatureB64: base64.StdEncoding.EncodeToString(item.Opaque.Signature),
			})
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return domain.ModelResponse{}, err
	}
	url := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return domain.ModelResponse{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	res, err := client.Do(httpReq)
	if err != nil {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderHTTP, scrub(err.Error(), p.APIKey))
	}
	defer res.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderHTTP, scrub(err.Error(), p.APIKey))
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderHTTP, scrub(truncate(string(respBody), 400), p.APIKey))
	}
	ctype := res.Header.Get("Content-Type")
	trim := bytes.TrimSpace(respBody)
	if strings.Contains(ctype, "text/event-stream") || bytes.HasPrefix(trim, []byte("data:")) {
		return parseSSE(bytes.NewReader(respBody), func(s string) string { return scrub(s, p.APIKey) })
	}
	parsed, err := parseChatJSON(respBody)
	if err != nil {
		return domain.ModelResponse{}, err
	}
	return parsed, nil
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
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}
