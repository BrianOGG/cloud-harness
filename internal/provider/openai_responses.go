package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// OpenAIResponses calls the Responses API with the standard library and reuses
// the OpenAI base URL, model, and credential settings at /responses.
type OpenAIResponses struct {
	BaseURL         string
	APIKey          string
	Model           string
	Client          *http.Client
	ReasoningReplay string
	MaxRetries      int
	RetryBaseDelay  time.Duration
	DisableImages   bool
	// ResponseHeaderTimeout and IdleTimeout bound a streamed attempt. Zero
	// uses DefaultResponseHeaderTimeout and DefaultIdleTimeout.
	ResponseHeaderTimeout time.Duration
	IdleTimeout           time.Duration
}

func (p *OpenAIResponses) Name() string { return "openai-responses" }

func (p *OpenAIResponses) SupportsImages() bool { return !p.DisableImages }

func (p *OpenAIResponses) Complete(ctx context.Context, req Request) (domain.ModelResponse, error) {
	var images []Image
	for _, msg := range req.Messages {
		images = append(images, msg.Images...)
	}
	if err := ValidateImages(images); err != nil {
		return domain.ModelResponse{}, err
	}
	if len(images) > 0 && !p.SupportsImages() {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderConfig, "openai-responses model does not support image input")
	}
	if strings.TrimSpace(p.APIKey) == "" {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderConfig, "OPENAI_API_KEY is not set")
	}
	model := req.Model
	if model == "" {
		model = p.Model
	}
	if model == "" {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderConfig, "OPENAI_MODEL is empty")
	}
	replayMode := strings.ToLower(strings.TrimSpace(p.ReasoningReplay))
	if replayMode == "" {
		replayMode = ReasoningReplayMessage
	}
	if replayMode != ReasoningReplayMessage && replayMode != ReasoningReplayOmit {
		return domain.ModelResponse{}, domain.E(domain.CodeProviderConfig, "invalid openai_reasoning_replay: "+p.ReasoningReplay)
	}
	names := newWireToolNames(req.Tools)
	wire, err := buildOpenAIResponsesRequest(model, req, names, replayMode == ReasoningReplayOmit)
	if err != nil {
		return domain.ModelResponse{}, err
	}
	onDelta := req.OnDelta
	if req.oracle {
		onDelta = nil
	}
	wire.Stream = onDelta != nil
	body, err := json.Marshal(wire)
	if err != nil {
		return domain.ModelResponse{}, domain.E(domain.CodeProtocol, "openai response request was not JSON")
	}
	base := strings.TrimRight(p.BaseURL, "/")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	cfg := postJSONConfig{
		URL:                   base + "/responses",
		APIKey:                p.APIKey,
		AuthHeader:            "Authorization",
		AuthScheme:            "Bearer ",
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
	if onDelta == nil {
		raw, err = postJSONWithRetry(ctx, cfg)
	} else {
		raw, streamed, err = postDeltaStream(ctx, cfg, onDelta, parseOpenAIResponsesStream)
	}
	if err != nil {
		return domain.ModelResponse{}, err
	}
	resp, err := ParseOpenAIResponses(raw)
	if err != nil {
		return domain.ModelResponse{}, err
	}
	if onDelta != nil && !streamed {
		emitResponseText(resp, onDelta)
	}
	return names.restore(resp, nil)
}

type openAIResponsesRequest struct {
	Model        string                `json:"model"`
	Instructions string                `json:"instructions,omitempty"`
	Input        []json.RawMessage     `json:"input"`
	Tools        []openAIResponsesTool `json:"tools,omitempty"`
	Store        bool                  `json:"store"`
	Stream       bool                  `json:"stream,omitempty"`
}

type openAIResponsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openAIResponsesMessage struct {
	Type    string                       `json:"type"`
	Role    string                       `json:"role"`
	Content []openAIResponsesContentPart `json:"content"`
}

type openAIResponsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type openAIResponsesFunctionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

type openAIResponsesFunctionOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

func buildOpenAIResponsesRequest(model string, req Request, names *wireToolNames, omitReasoning bool) (openAIResponsesRequest, error) {
	out := openAIResponsesRequest{Model: model, Instructions: openAIResponsesInstructions(req.Messages), Store: false}
	for _, def := range req.Tools {
		params := def.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, openAIResponsesTool{Type: "function", Name: names.wire(def.Name), Description: def.Description, Parameters: params})
	}
	useContinuation := len(req.Continuation) > 0 && (req.ContinuationProvider == "" || req.ContinuationProvider == "openai-responses")
	assistantIdx := -1
	if useContinuation {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "assistant" {
				assistantIdx = i
				break
			}
		}
	}
	for i, msg := range req.Messages {
		if msg.Role == "system" {
			continue
		}
		if i == assistantIdx {
			items, err := openAIResponsesContinuationItems(req.Continuation, names, omitReasoning)
			if err != nil {
				return openAIResponsesRequest{}, err
			}
			if len(items) > 0 {
				out.Input = append(out.Input, items...)
				continue
			}
		}
		var items []json.RawMessage
		var err error
		switch msg.Role {
		case "assistant":
			items, err = openAIResponsesAssistantItems(msg, names)
		case "tool":
			items, err = openAIResponsesToolOutputItems(msg)
		case "user":
			items, err = openAIResponsesUserItems(msg)
		default:
			if strings.TrimSpace(msg.Content) != "" {
				items, err = openAIResponsesUserItems(Message{Role: "user", Content: msg.Content, Images: msg.Images})
			}
		}
		if err != nil {
			return openAIResponsesRequest{}, err
		}
		out.Input = append(out.Input, items...)
	}
	if len(out.Input) == 0 {
		return openAIResponsesRequest{}, domain.E(domain.CodeInvalidArgument, "openai responses request has no input")
	}
	return out, nil
}

func openAIResponsesInstructions(messages []Message) string {
	var parts []string
	for _, msg := range messages {
		if msg.Role == "system" && strings.TrimSpace(msg.Content) != "" {
			parts = append(parts, msg.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

func openAIResponsesContinuationItems(items []domain.ResponseItem, names *wireToolNames, omitReasoning bool) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for _, item := range items {
		switch item.Kind {
		case domain.ItemText:
			if item.Text == "" {
				continue
			}
			raw, err := json.Marshal(openAIResponsesMessage{Type: "message", Role: "assistant", Content: []openAIResponsesContentPart{{Type: "output_text", Text: item.Text}}})
			if err != nil {
				return nil, domain.E(domain.CodeProtocol, "openai response input was not JSON")
			}
			out = append(out, raw)
		case domain.ItemToolCall:
			if item.ToolCall == nil {
				continue
			}
			args := string(item.ToolCall.Arguments)
			if args == "" {
				args = "{}"
			}
			if !json.Valid(item.ToolCall.Arguments) && len(item.ToolCall.Arguments) != 0 {
				return nil, domain.E(domain.CodeProtocol, "tool arguments were not JSON")
			}
			raw, err := json.Marshal(openAIResponsesFunctionCall{Type: "function_call", CallID: item.ToolCall.ProviderCallID, Name: names.wire(item.ToolCall.Name), Arguments: args, Status: "completed"})
			if err != nil {
				return nil, domain.E(domain.CodeProtocol, "openai response input was not JSON")
			}
			out = append(out, raw)
		case domain.ItemOpaque:
			if item.Opaque == nil || item.Opaque.Provider != "openai-responses" {
				continue
			}
			if omitReasoning && item.Opaque.Kind == "reasoning" {
				continue
			}
			payload := bytes.TrimSpace(item.Opaque.Payload)
			if len(payload) == 0 || !json.Valid(payload) {
				continue
			}
			out = append(out, json.RawMessage(payload))
		}
	}
	return out, nil
}

func openAIResponsesAssistantItems(msg Message, names *wireToolNames) ([]json.RawMessage, error) {
	var out []json.RawMessage
	if msg.Content != "" {
		raw, err := json.Marshal(openAIResponsesMessage{Type: "message", Role: "assistant", Content: []openAIResponsesContentPart{{Type: "output_text", Text: msg.Content}}})
		if err != nil {
			return nil, domain.E(domain.CodeProtocol, "openai response input was not JSON")
		}
		out = append(out, raw)
	}
	for _, call := range msg.ToolCalls {
		args := string(call.Arguments)
		if args == "" {
			args = "{}"
		}
		if !json.Valid(call.Arguments) && len(call.Arguments) != 0 {
			return nil, domain.E(domain.CodeProtocol, "tool arguments were not JSON")
		}
		raw, err := json.Marshal(openAIResponsesFunctionCall{Type: "function_call", CallID: call.ProviderCallID, Name: names.wire(call.Name), Arguments: args, Status: "completed"})
		if err != nil {
			return nil, domain.E(domain.CodeProtocol, "openai response input was not JSON")
		}
		out = append(out, raw)
	}
	return out, nil
}

func openAIResponsesToolOutputItems(msg Message) ([]json.RawMessage, error) {
	output := msg.Content
	if output == "" {
		output = "{}"
	}
	raw, err := json.Marshal(openAIResponsesFunctionOutput{Type: "function_call_output", CallID: msg.ToolCallID, Output: output})
	if err != nil {
		return nil, domain.E(domain.CodeProtocol, "openai response input was not JSON")
	}
	return []json.RawMessage{raw}, nil
}

func openAIResponsesUserItems(msg Message) ([]json.RawMessage, error) {
	var content []openAIResponsesContentPart
	if msg.Content != "" {
		content = append(content, openAIResponsesContentPart{Type: "input_text", Text: msg.Content})
	}
	for _, img := range msg.Images {
		mediaType := img.MediaType
		if mediaType == "" {
			mediaType = ImageMediaType(img.Data)
		}
		content = append(content, openAIResponsesContentPart{Type: "input_image", ImageURL: "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)})
	}
	if len(content) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(openAIResponsesMessage{Type: "message", Role: "user", Content: content})
	if err != nil {
		return nil, domain.E(domain.CodeProtocol, "openai response input was not JSON")
	}
	return []json.RawMessage{raw}, nil
}
