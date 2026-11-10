package provider

import (
	"bytes"
	"encoding/json"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

// ParseAnthropicMessage maps a Messages API body onto ordered response items.
// Thinking and redacted thinking blocks keep their original JSON bytes.
func ParseAnthropicMessage(raw []byte) (domain.ModelResponse, error) {
	var env struct {
		ID      string            `json:"id"`
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return domain.ModelResponse{}, domain.E(domain.CodeProtocol, "anthropic message was not JSON")
	}
	resp := domain.ModelResponse{Provider: "anthropic", ProviderResponseID: env.ID, Complete: true}
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
	if err := domain.ValidateItems(resp.Items); err != nil {
		return domain.ModelResponse{}, err
	}
	return resp, nil
}

// ParseOpenAIResponses maps a Responses API body onto ordered response items.
// Each reasoning item's original JSON is the opaque payload.
func ParseOpenAIResponses(raw []byte) (domain.ModelResponse, error) {
	var env struct {
		ID     string            `json:"id"`
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return domain.ModelResponse{}, domain.E(domain.CodeProtocol, "openai response was not JSON")
	}
	resp := domain.ModelResponse{Provider: "openai", ProviderResponseID: env.ID, Complete: true}
	for _, block := range env.Output {
		var head struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
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
				Provider: "openai",
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
				}
			}
			item.Kind = domain.ItemText
			item.Text = text
		default:
			item.Kind = domain.ItemOpaque
			item.Opaque = &domain.OpaqueItem{Provider: "openai", Kind: head.Type, Payload: append([]byte(nil), bytes.TrimSpace(block)...)}
		}
		resp.Items = append(resp.Items, item)
	}
	if err := domain.ValidateItems(resp.Items); err != nil {
		return domain.ModelResponse{}, err
	}
	return resp, nil
}
