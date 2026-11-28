package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

const searchMCPName = "search_mcp_tools"
const mcpCatalogThreshold = 32
const mcpPageSize = 8

type mcpCatalogKey struct{}
type mcpPage struct {
	Names      []string `json:"names"`
	NextOffset *int     `json:"next_offset,omitempty"`
}

func searchMCPSpec() tools.Spec {
	return tools.Spec{Name: searchMCPName, Readonly: true, Idempotent: true, SideEffectClass: "none",
		Description: "Search available MCP tool names and descriptions. Matching tools on this page are offered with their schemas on the next step. Use an empty query to browse or offset to continue.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"offset":{"type":"integer","minimum":0}},"additionalProperties":false}`),
		Execute:     searchMCPTools,
	}
}
func searchMCPTools(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	specs, ok := ctx.Value(mcpCatalogKey{}).([]tools.PublicSpec)
	if !ok {
		return nil, domain.E(domain.CodeAuthDenied, "MCP catalog requires the current run registry")
	}
	var input struct {
		Query  string
		Offset int
	}
	if json.Unmarshal(raw, &input) != nil || input.Offset < 0 || len(input.Query) > 512 {
		return nil, domain.E(domain.CodeInvalidArgument, "invalid MCP search query or offset")
	}
	terms := strings.Fields(strings.ToLower(input.Query))
	matched := 0
	page := mcpPage{Names: []string{}}
	for _, spec := range specs {
		if !strings.HasPrefix(spec.Name, "mcp.") {
			continue
		}
		text := strings.ToLower(spec.Name + " " + spec.Description)
		if slices.ContainsFunc(terms, func(term string) bool { return !strings.Contains(text, term) }) {
			continue
		}
		if matched >= input.Offset {
			if len(page.Names) == mcpPageSize {
				page.NextOffset = &matched
				break
			}
			page.Names = append(page.Names, spec.Name)
		}
		matched++
	}
	return json.Marshal(page)
}

// Page only the model-facing schema list. The search executor receives this
// same filtered registry, and never calls an MCP server. Selection is bounded
// to the most recent page, restored from checkpoints, and re-filtered here.
func (m *Manager) offerMCPTools(plan planToolRegistry, selected []string) []provider.ToolDefinition {
	specs := m.filteredRegistry(plan)
	count := 0
	canSearch := false
	for _, spec := range specs {
		if strings.HasPrefix(spec.Name, "mcp.") {
			count++
		}
		if spec.Name == searchMCPName {
			canSearch = true
		}
	}
	deferred := canSearch && count > mcpCatalogThreshold
	var out []provider.ToolDefinition
	for _, spec := range specs {
		if spec.Name == searchMCPName && !deferred {
			continue
		}
		if deferred && strings.HasPrefix(spec.Name, "mcp.") && !slices.Contains(selected, spec.Name) {
			continue
		}
		params := tools.CleanSchema(spec.Parameters)
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, provider.ToolDefinition{Name: spec.Name, Description: spec.Description, Parameters: params})
	}
	return out
}
