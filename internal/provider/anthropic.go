package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/modelconfig"
)

// Anthropic calls the Messages API with the standard library. It replays the
// same-provider continuation blocks verbatim so thinking signatures survive.
type Anthropic struct {
	BaseURL        string
	APIKey         string
	Model          string
	Client         *http.Client
	MaxTokens      int
	MaxRetries     int
	RetryBaseDelay time.Duration
	DisableImages  bool
	CapabilityID   string
	Thinking       map[string]string
	ModelMetadata  map[string]modelconfig.Model
	ModelsDev      *ModelsDev
	// ResponseHeaderTimeout and IdleTimeout bound a streamed attempt. Zero
	// uses DefaultResponseHeaderTimeout and DefaultIdleTimeout.
	ResponseHeaderTimeout time.Duration
	IdleTimeout           time.Duration
}

func (p *Anthropic) Name() string { return "anthropic" }

func (p *Anthropic) SupportsImages() bool { return !p.DisableImages }

// Probe performs the lightest connectivity check used by doctor.
func (p *Anthropic) Probe(ctx context.Context) error {
	if strings.TrimSpace(p.APIKey) == "" {
		return domain.E(domain.CodeProviderConfig, "ANTHROPIC_API_KEY is not set")
	}
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return domain.E(domain.CodeProviderHTTP, scrubProviderKey(err.Error(), p.APIKey))
	}
	req.Header.Set("x-api-key", p.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	client := p.Client
	if client == nil {
		client = &http.Client{}
	}
	res, err := client.Do(req)
	if err != nil {
		return domain.E(domain.CodeProviderHTTP, scrubProviderKey(err.Error(), p.APIKey))
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return domain.E(domain.CodeProviderHTTP, scrubProviderKey("GET /v1/models returned "+res.Status+": "+truncateProviderMessage(string(body), 400), p.APIKey))
	}
	return nil
}

func (p *Anthropic) Complete(ctx context.Context, req Request) (domain.ModelResponse, error) {
	var images []Image
	for _, msg := range req.Messages {
		images = append(images, msg.Images...)
	}
	if err := ValidateImages(images); err != nil {
		return domain.ModelResponse{}, err
	}
	if len(images) > 0 && !p.SupportsImages() {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderConfig, "anthropic model does not support image input")
	}
	if strings.TrimSpace(p.APIKey) == "" {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderConfig, "ANTHROPIC_API_KEY is not set")
	}
	model := req.Model
	if model == "" {
		model = p.Model
	}
	if model == "" {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderConfig, "ANTHROPIC_MODEL is empty")
	}
	maxTokens := p.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	identity := p.CapabilityID
	if identity == "" {
		identity = p.BaseURL
	}
	params := MapModelReasoning("anthropic", req.ReasoningEffort, ModelMetadata(p, model))
	// Build messages without the legacy effort mapping, then apply the catalog
	// mapping once so a disabled/missing level cannot inflate max_tokens.
	wireReq := req
	wireReq.ReasoningEffort = "default"
	names := newWireToolNames(req.Tools)
	wire, err := buildAnthropicRequest(model, maxTokens, wireReq, names)
	if err != nil {
		return domain.ModelResponse{}, err
	}
	wire.Thinking = params.Thinking
	if wire.Thinking != nil && wire.Thinking.Type == "enabled" {
		wire.MaxTokens = max(wire.MaxTokens, wire.Thinking.BudgetTokens+8000)
	}
	onDelta := req.OnDelta
	if req.oracle {
		onDelta = nil
	}
	wire.Stream = onDelta != nil
	body, err := json.Marshal(wire)
	if err != nil {
		return domain.ModelResponse{}, domain.E(domain.CodeProtocol, "anthropic request was not JSON")
	}
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = "https://api.anthropic.com"
	}
	cfg := postJSONConfig{
		URL:                   base + "/v1/messages",
		APIKey:                p.APIKey,
		AuthHeader:            "x-api-key",
		Headers:               map[string]string{"anthropic-version": "2023-06-01"},
		Body:                  body,
		Client:                p.Client,
		MaxRetries:            p.MaxRetries,
		RetryBaseDelay:        p.RetryBaseDelay,
		SingleAttempt:         req.singleAttempt,
		ResponseHeaderTimeout: p.ResponseHeaderTimeout,
		IdleTimeout:           p.IdleTimeout,
	}
	streamed := false
	var raw []byte
	send := func() {
		if onDelta == nil {
			raw, err = postJSONWithRetry(ctx, cfg)
		} else {
			raw, streamed, err = postDeltaStream(ctx, cfg, onDelta, parseAnthropicStream)
		}
	}
	send()
	var statusErr *HTTPStatusError
	if wire.Thinking != nil && errors.As(err, &statusErr) && reasoningRejected(statusErr.StatusCode, statusErr.Error()) && ctx.Err() == nil {
		rememberReasoningUnsupported("anthropic", identity, model, req)
		wire.Thinking = nil
		wire.MaxTokens = maxTokens
		cfg.Body, err = json.Marshal(wire)
		if err != nil {
			return domain.ModelResponse{}, err
		}
		send()
	}
	if err != nil {
		return domain.ModelResponse{}, err
	}
	resp, err := ParseAnthropicMessage(raw)
	if err != nil {
		return domain.ModelResponse{}, err
	}
	if onDelta != nil && !streamed {
		emitResponseText(resp, onDelta)
	}
	return names.restore(resp, nil)
}

type anthropicRequest struct {
	Thinking  *AnthropicThinking `json:"thinking,omitempty"`
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Stream    bool               `json:"stream,omitempty"`
}

type anthropicMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text,omitempty"`
	ID        string                `json:"id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Input     json.RawMessage       `json:"input,omitempty"`
	ToolUseID string                `json:"tool_use_id,omitempty"`
	Content   any                   `json:"content,omitempty"`
	Source    *anthropicImageSource `json:"source,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

func buildAnthropicRequest(model string, maxTokens int, req Request, names *wireToolNames) (anthropicRequest, error) {
	out := anthropicRequest{Model: model, MaxTokens: maxTokens, System: anthropicSystem(req.Messages)}
	out.Thinking = MapReasoningEffort("anthropic", req.ReasoningEffort).Thinking
	if out.Thinking != nil {
		out.MaxTokens = max(out.MaxTokens, out.Thinking.BudgetTokens+8000)
	}
	// No temperature or forced tool_choice is sent with extended thinking.
	for _, def := range req.Tools {
		params := def.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, anthropicTool{Name: names.wire(def.Name), Description: def.Description, InputSchema: params})
	}
	useContinuation := len(req.Continuation) > 0 && (req.ContinuationProvider == "" || req.ContinuationProvider == p_name_anthropic)
	assistantIdx := -1
	if useContinuation {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "assistant" {
				assistantIdx = i
				break
			}
		}
	}
	var pending []json.RawMessage
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		out.Messages = append(out.Messages, anthropicMessage{Role: "user", Content: pending})
		pending = nil
		return nil
	}
	for i, msg := range req.Messages {
		if msg.Role == "system" {
			continue
		}
		if i == assistantIdx {
			if err := flush(); err != nil {
				return anthropicRequest{}, err
			}
			blocks, err := anthropicContinuationBlocks(req.Continuation, names)
			if err != nil {
				return anthropicRequest{}, err
			}
			if len(blocks) > 0 {
				out.Messages = append(out.Messages, anthropicMessage{Role: "assistant", Content: blocks})
				continue
			}
		}
		switch msg.Role {
		case "assistant":
			if err := flush(); err != nil {
				return anthropicRequest{}, err
			}
			blocks, err := anthropicAssistantBlocks(msg, names)
			if err != nil {
				return anthropicRequest{}, err
			}
			if len(blocks) > 0 {
				out.Messages = append(out.Messages, anthropicMessage{Role: "assistant", Content: blocks})
			}
		case "tool":
			content := msg.Content
			if content == "" {
				content = "{}"
			}
			block, err := addAnthropicBlock(nil, anthropicBlock{Type: "tool_result", ToolUseID: msg.ToolCallID, Content: content})
			if err != nil {
				return anthropicRequest{}, err
			}
			pending = append(pending, block...)
		case "user":
			blocks, err := anthropicUserBlocks(msg, names)
			if err != nil {
				return anthropicRequest{}, err
			}
			pending = append(pending, blocks...)
		default:
			if strings.TrimSpace(msg.Content) == "" {
				continue
			}
			block, err := addAnthropicBlock(nil, anthropicBlock{Type: "text", Text: msg.Content})
			if err != nil {
				return anthropicRequest{}, err
			}
			pending = append(pending, block...)
		}
	}
	if err := flush(); err != nil {
		return anthropicRequest{}, err
	}
	if len(out.Messages) == 0 {
		return anthropicRequest{}, domain.E(domain.CodeInvalidArgument, "anthropic request has no messages")
	}
	return out, nil
}

func anthropicSystem(messages []Message) string {
	var parts []string
	for _, msg := range messages {
		if msg.Role == "system" && strings.TrimSpace(msg.Content) != "" {
			parts = append(parts, msg.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

func anthropicContinuationBlocks(items []domain.ResponseItem, names *wireToolNames) ([]json.RawMessage, error) {
	var blocks []json.RawMessage
	for _, item := range items {
		switch item.Kind {
		case domain.ItemText:
			if item.Text == "" {
				continue
			}
			block, err := addAnthropicBlock(blocks, anthropicBlock{Type: "text", Text: item.Text})
			if err != nil {
				return nil, err
			}
			blocks = block
		case domain.ItemToolCall:
			if item.ToolCall == nil {
				continue
			}
			args := item.ToolCall.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			if !json.Valid(args) {
				return nil, domain.E(domain.CodeProtocol, "tool arguments were not JSON")
			}
			block, err := addAnthropicBlock(blocks, anthropicBlock{Type: "tool_use", ID: item.ToolCall.ProviderCallID, Name: names.wire(item.ToolCall.Name), Input: args})
			if err != nil {
				return nil, err
			}
			blocks = block
		case domain.ItemOpaque:
			if item.Opaque == nil || item.Opaque.Provider != p_name_anthropic {
				continue
			}
			payload := bytes.TrimSpace(item.Opaque.Payload)
			if len(payload) == 0 || !json.Valid(payload) {
				continue
			}
			blocks = append(blocks, json.RawMessage(payload))
		}
	}
	return blocks, nil
}

func anthropicAssistantBlocks(msg Message, names *wireToolNames) ([]json.RawMessage, error) {
	var blocks []json.RawMessage
	if msg.Content != "" {
		var err error
		blocks, err = addAnthropicBlock(blocks, anthropicBlock{Type: "text", Text: msg.Content})
		if err != nil {
			return nil, err
		}
	}
	for _, call := range msg.ToolCalls {
		args := call.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		if !json.Valid(args) {
			return nil, domain.E(domain.CodeProtocol, "tool arguments were not JSON")
		}
		var err error
		blocks, err = addAnthropicBlock(blocks, anthropicBlock{Type: "tool_use", ID: call.ProviderCallID, Name: names.wire(call.Name), Input: args})
		if err != nil {
			return nil, err
		}
	}
	return blocks, nil
}

func anthropicUserBlocks(msg Message, names *wireToolNames) ([]json.RawMessage, error) {
	_ = names
	var blocks []json.RawMessage
	if msg.Content != "" {
		var err error
		blocks, err = addAnthropicBlock(blocks, anthropicBlock{Type: "text", Text: msg.Content})
		if err != nil {
			return nil, err
		}
	}
	for _, img := range msg.Images {
		mediaType := img.MediaType
		if mediaType == "" {
			mediaType = ImageMediaType(img.Data)
		}
		var err error
		blocks, err = addAnthropicBlock(blocks, anthropicBlock{Type: "image", Source: &anthropicImageSource{Type: "base64", MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(img.Data)}})
		if err != nil {
			return nil, err
		}
	}
	return blocks, nil
}

func addAnthropicBlock(blocks []json.RawMessage, v any) ([]json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, domain.E(domain.CodeProtocol, "anthropic content block was not JSON")
	}
	return append(blocks, raw), nil
}

const p_name_anthropic = "anthropic"
