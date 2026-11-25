package domain

import (
	"bytes"
	"encoding/json"
)

// ItemKind is one ordered element of a model response.
type ItemKind string

const (
	ItemText       ItemKind = "text"
	ItemToolCall   ItemKind = "tool_call"
	ItemToolResult ItemKind = "tool_result"
	ItemOpaque     ItemKind = "opaque"
	ItemUsage      ItemKind = "usage"
)

// Usage is the provider-reported token accounting plus the call metadata the
// runtime captures when it persists a response. Fields the provider does not
// report stay zero. Model and cost metadata are optional so older usage_json
// rows remain readable; a false CostKnown means the cost is unknown, while a
// true CostKnown with CostUSD == 0 is a known zero-cost call.
type Usage struct {
	InputTokens       int     `json:"input_tokens,omitempty"`
	OutputTokens      int     `json:"output_tokens,omitempty"`
	CachedInputTokens int     `json:"cached_input_tokens,omitempty"`
	LatencyMS         int64   `json:"latency_ms,omitempty"`
	Model             string  `json:"model,omitempty"`
	CostUSD           float64 `json:"cost_usd,omitempty"`
	CostKnown         bool    `json:"cost_known,omitempty"`
}

// ToolCall is one provider tool call. Arguments is a raw JSON object.
type ToolCall struct {
	ProviderCallID string          `json:"provider_call_id"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
	Ordinal        int             `json:"ordinal"`
}

// OpaqueItem is provider continuation data. Payload and Signature are
// immutable bytes. They are never written to trace or to the search index.
type OpaqueItem struct {
	Provider  string `json:"provider"`
	Kind      string `json:"kind"`
	Payload   []byte `json:"payload"`
	Signature []byte `json:"signature,omitempty"`
}

// ResponseItem is one ordered item. Ordinals in a response are 0..n-1.
type ResponseItem struct {
	Ordinal    int             `json:"ordinal"`
	Kind       ItemKind        `json:"kind"`
	Text       string          `json:"text,omitempty"`
	ToolCall   *ToolCall       `json:"tool_call,omitempty"`
	ToolResult *ToolResultView `json:"tool_result,omitempty"`
	Opaque     *OpaqueItem     `json:"opaque,omitempty"`
	Usage      *Usage          `json:"usage,omitempty"`
}

// ToolResultView is the result item returned to the same provider.
type ToolResultView struct {
	ProviderCallID string          `json:"provider_call_id"`
	OK             bool            `json:"ok"`
	Data           json.RawMessage `json:"data,omitempty"`
	Error          *Error          `json:"error,omitempty"`
}

// ModelResponse is a complete provider response. Complete is false until the
// stream has finished and arguments have been validated; an incomplete
// response is not a tool batch.
type ModelResponse struct {
	Provider           string         `json:"provider"`
	ProviderResponseID string         `json:"provider_response_id,omitempty"`
	Complete           bool           `json:"complete"`
	Items              []ResponseItem `json:"items"`
}

// ToolCalls returns tool-call items in response order.
func (r ModelResponse) ToolCalls() []ToolCall {
	var out []ToolCall
	for _, it := range r.Items {
		if it.Kind == ItemToolCall && it.ToolCall != nil {
			out = append(out, *it.ToolCall)
		}
	}
	return out
}

// Text joins text items in order with no separator. Used for a pure ANSWER.
func (r ModelResponse) Text() string {
	var b bytes.Buffer
	for _, it := range r.Items {
		if it.Kind == ItemText {
			b.WriteString(it.Text)
		}
	}
	return b.String()
}

// ValidateItems checks ordinal continuity and the required body for each kind.
func ValidateItems(items []ResponseItem) error {
	for i, it := range items {
		if it.Ordinal != i {
			return E(CodeProtocol, "response item ordinals must be contiguous from 0")
		}
		switch it.Kind {
		case ItemText:
		case ItemToolCall:
			if it.ToolCall == nil || it.ToolCall.ProviderCallID == "" || it.ToolCall.Name == "" {
				return E(CodeProtocol, "tool call requires provider_call_id and name")
			}
		case ItemToolResult:
			if it.ToolResult == nil || it.ToolResult.ProviderCallID == "" {
				return E(CodeProtocol, "tool result requires provider_call_id")
			}
		case ItemOpaque:
			if it.Opaque == nil || it.Opaque.Provider == "" || len(it.Opaque.Payload) == 0 {
				return E(CodeProtocol, "opaque item requires provider and payload")
			}
		case ItemUsage:
			if it.Usage == nil {
				return E(CodeProtocol, "usage item requires a usage body")
			}
		default:
			return E(CodeProtocol, "unknown response item kind "+string(it.Kind))
		}
	}
	return nil
}

// CloneItems deep-copies items so a replay cannot mutate stored signatures.
func CloneItems(items []ResponseItem) []ResponseItem {
	out := make([]ResponseItem, len(items))
	for i, it := range items {
		out[i] = it
		if it.ToolCall != nil {
			cp := *it.ToolCall
			cp.Arguments = append(json.RawMessage(nil), it.ToolCall.Arguments...)
			out[i].ToolCall = &cp
		}
		if it.ToolResult != nil {
			cp := *it.ToolResult
			cp.Data = append(json.RawMessage(nil), it.ToolResult.Data...)
			if it.ToolResult.Error != nil {
				e := *it.ToolResult.Error
				cp.Error = &e
			}
			out[i].ToolResult = &cp
		}
		if it.Opaque != nil {
			cp := *it.Opaque
			cp.Payload = append([]byte(nil), it.Opaque.Payload...)
			cp.Signature = append([]byte(nil), it.Opaque.Signature...)
			out[i].Opaque = &cp
		}
		if it.Usage != nil {
			u := *it.Usage
			out[i].Usage = &u
		}
	}
	return out
}
