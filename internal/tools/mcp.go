package tools

import (
	"context"
	"encoding/json"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

// MCPTool is the Model Context Protocol tool descriptor mapped from a ToolSpec.
// This process does not open an MCP session.
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCPResult is the error or text shape a host would return to an MCP client.
type MCPResult struct {
	Content []MCPText `json:"content"`
	IsError bool      `json:"isError"`
	Code    string    `json:"code,omitempty"`
}

// MCPText is one text content block.
type MCPText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToMCPTool maps a local spec to an MCP tool. Execution stays on the broker.
func ToMCPTool(spec Spec) MCPTool {
	schema := CleanSchema(spec.Parameters)
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return MCPTool{Name: spec.Name, Description: spec.Description, InputSchema: schema}
}

// MCPErrorResult converts a domain error into an MCP tool result.
// A permission denial stays auth_denied and does not include a success payload.
func MCPErrorResult(err *domain.Error) MCPResult {
	if err == nil {
		return MCPResult{IsError: true, Code: domain.CodeToolError, Content: []MCPText{{Type: "text", Text: "tool failed"}}}
	}
	return MCPResult{
		IsError: true,
		Code:    err.Code,
		Content: []MCPText{{Type: "text", Text: err.Message}},
	}
}

// StaticMCP is a ToolSpec mapping with no network client.
// Discover returns the specs it was given. It does not dial a server.
type StaticMCP struct {
	Label string
	Specs []Spec
}

func (StaticMCP) Protocol() string { return "mcp" }

func (s StaticMCP) ServerLabel() string {
	if s.Label == "" {
		return "unconnected"
	}
	return s.Label
}

func (s StaticMCP) Discover(context.Context) ([]Spec, error) {
	out := make([]Spec, len(s.Specs))
	copy(out, s.Specs)
	return out, nil
}
