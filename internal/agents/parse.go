package agents

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func parseDefinition(name, text string, known map[string]bool) (Definition, error) {
	def := Definition{Name: name, Source: "file"}
	lines := strings.Split(text, "\n")
	if strings.TrimSuffix(lines[0], "\r") != "---" {
		return Definition{}, fmt.Errorf("missing frontmatter: file must start with a --- line")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSuffix(lines[i], "\r") == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return Definition{}, fmt.Errorf("unclosed frontmatter: missing closing --- line")
	}
	seen := make(map[string]bool)
	for i := 1; i < end; i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" {
			return Definition{}, fmt.Errorf("line %d: expected single-line key: value", i+1)
		}
		if seen[key] {
			return Definition{}, fmt.Errorf("line %d: duplicate field %q", i+1, key)
		}
		seen[key] = true
		var err error
		switch key {
		case "description":
			def.Description, err = stringValue(value)
		case "tools":
			def.Tools, err = parseTools(value, known)
		case "model":
			def.Model, err = stringValue(value)
		case "max_model_calls", "max_tool_calls":
			var n int
			n, err = strconv.Atoi(value)
			if err != nil || n <= 0 {
				err = fmt.Errorf("must be an integer > 0, got %q", value)
			} else if key == "max_model_calls" {
				def.MaxModelCalls = n
			} else {
				def.MaxToolCalls = n
			}
		case "name":
			var declared string
			declared, err = stringValue(value)
			if err == nil && declared != name {
				err = fmt.Errorf("name %q must match filename stem %q exactly", declared, name)
			}
		default:
			return Definition{}, fmt.Errorf("line %d: unknown field %q", i+1, key)
		}
		if err != nil {
			return Definition{}, fmt.Errorf("line %d: %s: %w", i+1, key, err)
		}
	}
	if strings.TrimSpace(def.Description) == "" {
		return Definition{}, fmt.Errorf("description is required and must not be empty")
	}
	def.System = strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
	return def, nil
}

// stringValue supports plain text and optional matching outer quotes. Contents
// are literal: this is deliberately not YAML and performs no escape expansion.
func stringValue(value string) (string, error) {
	if value != "" && (value[0] == '\'' || value[0] == '"') {
		if len(value) < 2 || value[len(value)-1] != value[0] {
			return "", fmt.Errorf("unterminated quoted string")
		}
		return value[1 : len(value)-1], nil
	}
	return value, nil
}

func parseTools(value string, known map[string]bool) ([]string, error) {
	if strings.HasPrefix(value, "[") || strings.HasSuffix(value, "]") {
		if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
			return nil, fmt.Errorf("tools list must have matching [ and ]")
		}
		value = strings.TrimSpace(value[1 : len(value)-1])
		if value == "" {
			return []string{}, nil
		}
	}
	if value == "" {
		return nil, fmt.Errorf("empty tools value: use [] for no tools, or omit the field to inherit")
	}
	names := strings.Split(value, ",")
	for i, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("tools contains an empty tool name")
		}
		if name == domain.ToolAskUser || name == domain.ToolDelegateTask {
			return nil, fmt.Errorf("tool %q is forbidden for child agents", name)
		}
		if !known[name] {
			return nil, fmt.Errorf("unknown tool %q", name)
		}
		names[i] = name
	}
	return names, nil
}
