package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

const (
	nestedRulesTask = "nested-project-rules"
	catalogTask     = "large-mcp-catalog"
	catalogTool     = "mcp.inventory.payments_queue_depth"
)

var ruleFixture = map[string]string{
	"AGENTS.md":                "Release label files contain one line: label=<value>. Use SHARED unless a more specific project rule supplies the value.\n",
	"frontend/AGENTS.md":       "Frontend release labels use the value CANVAS.\n",
	"backend/api/AGENTS.md":    "API release labels use the value LEDGER.\n",
	"frontend/src/release.txt": "label=pending\n",
	"backend/api/release.txt":  "label=pending\n",
}

func rulesScript() provider.Provider {
	return &provider.Mock{Responses: []domain.ModelResponse{
		toolCalls(call("r1", "read_file", `{"path":"frontend/src/release.txt"}`),
			call("r2", "read_file", `{"path":"backend/api/release.txt"}`)),
		toolCalls(call("e1", "edit_file", `{"path":"frontend/src/release.txt","old_string":"pending","new_string":"CANVAS"}`),
			call("e2", "edit_file", `{"path":"backend/api/release.txt","old_string":"pending","new_string":"LEDGER"}`)),
		textResp("Updated both release labels."),
	}}
}

func scoreNestedRules(root string, status domain.RunStatus) []string {
	var failures []string
	if status != domain.RunCompleted {
		failures = append(failures, "run status is "+string(status))
	}
	for _, want := range []struct{ path, body string }{
		{"frontend/src/release.txt", "label=CANVAS\n"},
		{"backend/api/release.txt", "label=LEDGER\n"},
	} {
		body, err := os.ReadFile(filepath.Join(root, want.path))
		if err != nil || string(body) != want.body {
			failures = append(failures, want.path+" does not match its local release rule")
		}
	}
	return failures
}

// Scripted responses know the fixture answers. Check the actual model inputs
// as well, so a missing rule or undiscovered schema cannot yield a false pass.
func scoreScriptedContext(task string, requests []provider.Request) []string {
	if task != nestedRulesTask && task != catalogTask {
		return nil
	}
	if len(requests) < 2 {
		return []string{"missing model requests for context check"}
	}
	if task == catalogTask {
		hasTool := func(req provider.Request, name string) bool {
			return slices.ContainsFunc(req.Tools, func(tool provider.ToolDefinition) bool { return tool.Name == name })
		}
		if !hasTool(requests[0], "search_mcp_tools") || hasTool(requests[0], catalogTool) || !hasTool(requests[1], catalogTool) {
			return []string{"model did not receive the target MCP schema after catalog discovery"}
		}
		return nil
	}
	hasRule := func(req provider.Request, path string) bool {
		return slices.ContainsFunc(req.Prefix, func(msg provider.Message) bool {
			return strings.Contains(msg.Content, strings.TrimSpace(ruleFixture[path]))
		})
	}
	if !hasRule(requests[0], "AGENTS.md") {
		return []string{"root rule missing from initial model prefix"}
	}
	for _, path := range []string{"frontend/AGENTS.md", "backend/api/AGENTS.md"} {
		if hasRule(requests[0], path) || !hasRule(requests[1], path) {
			return []string{"incorrect model rule scope for " + path}
		}
	}
	return nil
}

// In-process tools keep the catalog, descriptors and answers identical between
// runs. This exercises tool selection through the broker without a subprocess.
func registerCatalog(ctx context.Context, broker *tools.Broker) ([]domain.Grant, error) {
	var catalog tools.StaticMCP
	catalog.Label = "inventory"
	services := []string{"accounts", "billing", "catalog", "delivery", "inventory", "payments", "returns", "search"}
	metrics := []string{"cpu", "errors", "latency", "memory", "queue_depth", "replicas", "requests", "uptime"}
	for i, service := range services {
		for j, metric := range metrics {
			value := 100*(i+1) + j
			catalog.Specs = append(catalog.Specs, tools.Spec{
				Name: "mcp.inventory." + service + "_" + metric, Description: "Read " + service + " " + strings.ReplaceAll(metric, "_", " ") + ".",
				Readonly: true, Idempotent: true, SideEffectClass: "none",
				Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
				Execute: func(context.Context, json.RawMessage) (json.RawMessage, error) {
					return json.Marshal(map[string]any{"service": service, "metric": metric, "value": value})
				},
			})
		}
	}
	specs, err := catalog.Discover(ctx)
	if err != nil {
		return nil, err
	}
	var grants []domain.Grant
	for _, spec := range specs {
		broker.Register(spec)
		grants = append(grants, domain.Grant{Tool: spec.Name, Op: "call", State: "active", Subject: "eval"})
	}
	return grants, nil
}

func scoreCatalog(status domain.RunStatus, answer string, calls []Call) []string {
	if status != domain.RunCompleted {
		return []string{"run status is " + string(status)}
	}
	for _, c := range calls {
		if c.Name == catalogTool && c.OK {
			var observation struct{ Value int }
			if json.Unmarshal([]byte(c.ResultText), &observation) == nil && observation.Value == 604 &&
				strings.TrimSpace(answer) == fmt.Sprint(observation.Value) {
				return nil
			}
		}
	}
	return []string{"expected a successful payments queue depth lookup and its numeric value as the answer"}
}
