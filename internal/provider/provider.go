// Package provider adapts model APIs to domain responses.
// SDK and HTTP types stay in this package.
package provider

import (
	"context"
	"encoding/json"

	"github.com/cloud-harness/cloud-harness/internal/domain"
)

// ToolDefinition is a function the model may call. It is not an SDK type.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Message is one chat message. ToolCalls and ToolCallID carry direct tool use.
type Message struct {
	Role       string            `json:"role"`
	Content    string            `json:"content"`
	ToolCalls  []domain.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

// Request is one completion. Continuation is the previous response items in order,
// including opaque bytes that must be replayed unchanged for the same provider.
type Request struct {
	Model    string
	Messages []Message
	// Prefix is the stable head of Messages. Compaction must not change it.
	Prefix               []Message
	Tools                []ToolDefinition
	ContinuationProvider string
	Continuation         []domain.ResponseItem
}

// Provider completes one model call and returns domain items only.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (domain.ModelResponse, error)
}
