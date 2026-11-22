package provider

import (
	"bytes"
	"encoding/json"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// ParseAnthropicMessage maps a Messages API body onto ordered response items.
// Thinking and redacted thinking blocks keep their original JSON bytes.
func ParseAnthropicMessage(raw []byte) (domain.ModelResponse, error) {
	var env struct {
		ID         string            `json:"id"`
		Content    []json.RawMessage `json:"content"`
		StopReason string            `json:"stop_reason"`
		Usage      *anthropicUsage   `json:"usage"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return domain.ModelResponse{}, domain.E(domain.CodeProtocol, "anthropic message was not JSON")
	}
	resp := domain.ModelResponse{Provider: "anthropic", ProviderResponseID: env.ID, Complete: anthropicResponseComplete(env.StopReason)}
	for _, block := range env.Content {
		var head struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			Signature string          `json:"signature"`
		}
		if err := json.Unmarshal(block, &head); err != nil {
			return domain.ModelResponse{}, domain.E(domain.CodeProtocol, "anthropic content block was not JSON")
		}
		item := domain.ResponseItem{Ordinal: len(resp.Items)}
		switch head.Type {
		case "text":
			item.Kind = domain.ItemText
			item.Text = head.Text
		case "thinking", "redacted_thinking":
			item.Kind = domain.ItemOpaque
			item.Opaque = &domain.OpaqueItem{
				Provider:  "anthropic",
				Kind:      head.Type,
				Payload:   append([]byte(nil), bytes.TrimSpace(block)...),
				Signature: []byte(head.Signature),
			}
		case "tool_use":
			args := head.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			item.Kind = domain.ItemToolCall
			item.ToolCall = &domain.ToolCall{ProviderCallID: head.ID, Name: head.Name, Arguments: args}
		default:
			item.Kind = domain.ItemOpaque
			item.Opaque = &domain.OpaqueItem{Provider: "anthropic", Kind: head.Type, Payload: append([]byte(nil), bytes.TrimSpace(block)...)}
		}
		resp.Items = append(resp.Items, item)
	}
	if env.Usage != nil {
		if u := env.Usage.domain(); u.InputTokens > 0 || u.OutputTokens > 0 || u.CachedInputTokens > 0 {
			resp.Items = append(resp.Items, domain.ResponseItem{Ordinal: len(resp.Items), Kind: domain.ItemUsage, Usage: &u})
		}
	}
	if err := domain.ValidateItems(resp.Items); err != nil {
		return domain.ModelResponse{}, err
	}
	return resp, nil
}

func anthropicResponseComplete(stopReason string) bool {
	switch stopReason {
	case "", "end_turn", "stop_sequence", "tool_use":
		return true
	default:
		return false
	}
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func (u anthropicUsage) domain() domain.Usage {
	return domain.Usage{
		InputTokens:       u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
	}
}

// ParseOpenAIResponses maps a Responses API body onto ordered response items.
// Each reasoning item's original JSON is the opaque payload.
func ParseOpenAIResponses(raw []byte) (domain.ModelResponse, error) {
	var env struct {
		ID     string                `json:"id"`
		Status string                `json:"status"`
		Output []json.RawMessage     `json:"output"`
		Usage  *openAIResponsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return domain.ModelResponse{}, domain.E(domain.CodeProtocol, "openai response was not JSON")
	}
	resp := domain.ModelResponse{
		Provider:           "openai-responses",
		ProviderResponseID: env.ID,
		Complete:           env.Status == "" || env.Status == "completed",
	}
	for _, block := range env.Output {
		var head struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		}
		if err := json.Unmarshal(block, &head); err != nil {
			return domain.ModelResponse{}, domain.E(domain.CodeProtocol, "openai output item was not JSON")
		}
		item := domain.ResponseItem{Ordinal: len(resp.Items)}
		switch head.Type {
		case "reasoning":
			item.Kind = domain.ItemOpaque
			item.Opaque = &domain.OpaqueItem{
				Provider: "openai-responses",
				Kind:     "reasoning",
				Payload:  append([]byte(nil), bytes.TrimSpace(block)...),
			}
		case "function_call":
			args := head.Arguments
			if args == "" {
				args = "{}"
			}
			item.Kind = domain.ItemToolCall
			item.ToolCall = &domain.ToolCall{ProviderCallID: head.CallID, Name: head.Name, Arguments: json.RawMessage(args)}
		case "message":
			var text string
			for _, c := range head.Content {
				if c.Type == "output_text" || c.Type == "text" {
					text += c.Text
				} else if c.Type == "refusal" {
					text += c.Refusal
				}
			}
			item.Kind = domain.ItemText
			item.Text = text
		default:
			item.Kind = domain.ItemOpaque
			item.Opaque = &domain.OpaqueItem{Provider: "openai-responses", Kind: head.Type, Payload: append([]byte(nil), bytes.TrimSpace(block)...)}
		}
		resp.Items = append(resp.Items, item)
	}
	if env.Usage != nil {
		if u := env.Usage.domain(); u.InputTokens > 0 || u.OutputTokens > 0 || u.CachedInputTokens > 0 {
			resp.Items = append(resp.Items, domain.ResponseItem{Ordinal: len(resp.Items), Kind: domain.ItemUsage, Usage: &u})
		}
	}
	if err := domain.ValidateItems(resp.Items); err != nil {
		return domain.ModelResponse{}, err
	}
	return resp, nil
}

type openAIResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	InputDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (u openAIResponsesUsage) domain() domain.Usage {
	return domain.Usage{
		InputTokens:       u.InputTokens,
		OutputTokens:      u.OutputTokens,
		CachedInputTokens: u.InputDetails.CachedTokens,
	}
}
