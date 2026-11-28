package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

type listedMCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func DiscoverMCPTools(ctx context.Context, server string, readonly, remote bool, timeout time.Duration, list func(context.Context, map[string]any) (json.RawMessage, error), execute func(context.Context, string, json.RawMessage) (json.RawMessage, error)) ([]Spec, error) {
	var listed []listedMCPTool
	// tools/list is paginated: keep asking while the server returns a cursor.
	totalBytes := 0
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxMCPListPages {
			return nil, domain.E(domain.CodeToolError, fmt.Sprintf("mcp tools/list did not end within %d pages", maxMCPListPages))
		}
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		listResp, err := list(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("mcp tools/list failed: %w", err)
		}
		totalBytes += len(listResp)
		if totalBytes > 32<<20 {
			return nil, domain.E(domain.CodeToolError, "MCP inventory exceeds 32 MiB")
		}
		var listResult struct {
			Tools      []listedMCPTool `json:"tools"`
			NextCursor string          `json:"nextCursor"`
		}
		if err := json.Unmarshal(listResp, &listResult); err != nil {
			return nil, domain.E(domain.CodeToolError, "invalid tools/list response: "+err.Error())
		}
		if listResult.Tools == nil || len(listed)+len(listResult.Tools) > 10000 {
			return nil, domain.E(domain.CodeToolError, "invalid or oversized MCP tool inventory")
		}
		listed = append(listed, listResult.Tools...)
		if listResult.NextCursor == "" {
			break
		}
		cursor = listResult.NextCursor
	}

	specs := make([]Spec, 0, len(listed))
	for _, t := range listed {
		origName := t.Name
		toolName := fmt.Sprintf("mcp.%s.%s", server, origName)

		var reqFields []string
		if len(t.InputSchema) > 0 {
			var schemaMap map[string]any
			if err := json.Unmarshal(t.InputSchema, &schemaMap); err == nil {
				if reqArr, ok := schemaMap["required"].([]any); ok {
					for _, r := range reqArr {
						if str, ok := r.(string); ok {
							reqFields = append(reqFields, str)
						}
					}
				}
			}
		}

		sideEffect := "reversible"
		egressScope := server
		if readonly {
			sideEffect = "none"
			if !remote {
				egressScope = ""
			}
		}

		schema := CleanSchema(t.InputSchema)
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}

		spec := Spec{
			Name:            toolName,
			Description:     t.Description,
			Required:        reqFields,
			Timeout:         timeout,
			Readonly:        readonly,
			Idempotent:      readonly,
			SideEffectClass: sideEffect,
			EgressScope:     egressScope,
			Parameters:      schema,
			Execute: func(execCtx context.Context, args json.RawMessage) (json.RawMessage, error) {
				return execute(execCtx, origName, args)
			},
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func FormatMCPResult(respRaw json.RawMessage) (json.RawMessage, error) {
	var callRes struct {
		Content []json.RawMessage `json:"content"`
		IsError bool              `json:"isError"`
	}
	_ = json.Unmarshal(respRaw, &callRes)

	truncated := false
	totalLen := 0
	var truncatedContent []MCPText

	if len(callRes.Content) > 0 {
		for _, raw := range callRes.Content {
			block := mcpContentText(raw)
			if rem := MaxMCPResultBytes - totalLen; len(block.Text) > rem {
				if rem > 0 {
					block.Text = block.Text[:rem]
					truncatedContent = append(truncatedContent, block)
				}
				truncated = true
				break
			}
			truncatedContent = append(truncatedContent, block)
			totalLen += len(block.Text)
		}
	} else if len(respRaw) > 0 {
		text := string(respRaw)
		if len(text) > MaxMCPResultBytes {
			text = text[:MaxMCPResultBytes]
			truncated = true
		}
		truncatedContent = []MCPText{{Type: "text", Text: text}}
	}

	out := map[string]any{
		"content":   truncatedContent,
		"isError":   callRes.IsError,
		"untrusted": true,
		"truncated": truncated,
	}
	if truncated {
		out["notice"] = fmt.Sprintf("truncated: mcp output capped at %d bytes (64KiB limit)", MaxMCPResultBytes)
	}

	outRaw, _ := json.Marshal(out)
	if callRes.IsError {
		errMsg := "mcp tool reported error"
		if len(truncatedContent) > 0 && truncatedContent[0].Text != "" {
			errMsg = truncatedContent[0].Text
		}
		return outRaw, domain.E(domain.CodeToolError, errMsg)
	}
	return outRaw, nil
}
