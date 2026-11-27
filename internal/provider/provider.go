// Package provider adapts model APIs to domain responses.
// SDK and HTTP types stay in this package.
package provider

import (
	"context"
	"encoding/json"

	"github.com/BrianOGG/cloud-harness/internal/domain"
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
	Images     []Image           `json:"images,omitempty"`
	ToolCalls  []domain.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	Ref        string            `json:"ref,omitempty"`
}

// Image is an image attached to one user message. Data is transient: checkpoints
// retain the path and metadata but never serialize the image bytes.
type Image struct {
	Path      string `json:"path,omitempty"`
	MediaType string `json:"media_type"`
	SizeBytes int    `json:"size_bytes"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	SHA256    string `json:"sha256,omitempty"`
	Data      []byte `json:"-"`
}

const (
	MaxImageBytes       = 10 << 20
	MaxImagesTotalBytes = 20 << 20
)

// Request is one completion. Continuation is the previous response items in order,
// including opaque bytes that must be replayed unchanged for the same provider.
type Request struct {
	Model               string
	ReasoningEffort     string
	OnReasoningFallback func(ReasoningFallbackEvent) `json:"-"`
	Messages            []Message
	// Prefix is the stable head of Messages. Compaction must not change it.
	Prefix               []Message
	Tools                []ToolDefinition
	ContinuationProvider string
	Continuation         []domain.ResponseItem
	// OnDelta, when non-nil, receives text deltas incrementally as they arrive.
	OnDelta func(string) `json:"-"`

	singleAttempt bool // ProfileProvider owns retries and the budget for these calls.
	oracle        bool // Keep consultation text out of the main model's delta callback.
}

// Provider completes one model call and returns domain items only.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (domain.ModelResponse, error)
}

// ImageInputProvider marks providers that can accept image content blocks.
type ImageInputProvider interface {
	SupportsImages() bool
}

// SupportsImages reports whether p explicitly supports image input.
func SupportsImages(p Provider) bool {
	capable, ok := p.(ImageInputProvider)
	return ok && capable.SupportsImages()
}
