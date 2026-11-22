// Package agents loads declarative child-agent definitions. It does not grant
// tools, run agents, or change the delegate_task contract.
package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

// Definition is a role definition, not an authorization to execute its tools.
// Tools == nil means no additional restriction; a non-nil empty slice means no
// tools. Zero budgets and an empty Model mean those options were not declared.
type Definition struct {
	Name             string   `json:"name"`
	Description      string   `json:"description"`
	Tools            []string `json:"tools"`
	Model            string   `json:"model"`
	MaxModelCalls    int      `json:"max_model_calls"`
	MaxToolCalls     int      `json:"max_tool_calls"`
	System           string   `json:"system"`
	Source           string   `json:"source"`
	OverridesBuiltin bool     `json:"overrides_builtin"`
}

// Registry is an immutable set of definitions keyed by name. Its zero value is
// empty; use Load to include the built-in definitions.
type Registry struct {
	definitions map[string]Definition
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Load merges built-ins with regular .md files directly in .agents/agents.
// Files are visited in name order and the first error stops loading. Symlinks
// and subdirectories are skipped; a missing definitions directory is valid.
// An empty workspace deliberately loads only built-ins, rather than using cwd.
func Load(workspace string) (Registry, error) {
	r := builtinRegistry()
	if workspace == "" {
		return r, nil
	}
	dir := filepath.Join(workspace, ".agents", "agents")
	// Do not follow definition-directory symlinks either.
	for _, path := range []string{filepath.Join(workspace, ".agents"), dir} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return r, nil
		}
		if err != nil {
			return Registry{}, fmt.Errorf("agents: %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return r, nil
		}
		if !info.IsDir() {
			return Registry{}, fmt.Errorf("agents: %s: not a directory", path)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Registry{}, fmt.Errorf("agents: %s: %w", dir, err)
	}
	known, err := knownChildTools(workspace)
	if err != nil {
		return Registry{}, fmt.Errorf("agents: %s: tool catalog: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return Registry{}, fmt.Errorf("agents: %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".md")
		if !validName.MatchString(name) {
			return Registry{}, fmt.Errorf("agents: %s: invalid agent name %q (must match [a-z0-9][a-z0-9-_]*)", path, name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return Registry{}, fmt.Errorf("agents: %s: %w", path, err)
		}
		def, err := parseDefinition(name, string(data), known)
		if err != nil {
			return Registry{}, fmt.Errorf("agents: %s: %w", path, err)
		}
		_, def.OverridesBuiltin = r.definitions[name]
		r.definitions[name] = def
	}
	return r, nil
}

// Get returns a copy of the named definition, if any.
func (r Registry) Get(name string) (Definition, bool) {
	def, ok := r.definitions[name]
	def.Tools = slices.Clone(def.Tools)
	return def, ok
}

// List returns independent copies in ascending name order.
func (r Registry) List() []Definition {
	names := make([]string, 0, len(r.definitions))
	for name := range r.definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	defs := make([]Definition, 0, len(names))
	for _, name := range names {
		def, _ := r.Get(name)
		defs = append(defs, def)
	}
	return defs
}

func builtinRegistry() Registry {
	readonly := []string{"read_file", "list_dir", "list_files", "grep", "git_status", "git_diff", "git_log"}
	return Registry{definitions: map[string]Definition{
		"explorer": {
			Name: "explorer", Description: "Read-only code exploration and location (只读代码探索/定位).",
			Tools: slices.Clone(readonly), Source: "builtin",
			System: "Quickly explore the codebase and locate relevant code. Use only read-only tools and report findings with file references.",
		},
		"reviewer": {
			Name: "reviewer", Description: "Read-only code review (只读代码审查).",
			Tools: slices.Clone(readonly), Source: "builtin",
			System: "Review code using only read-only tools. Report actionable findings with file references and do not modify files.",
		},
	}}
}

func knownChildTools(workspace string) (map[string]bool, error) {
	// Construct specifications only: no broker, store, executor, or process is
	// started. This keeps workspace names aligned with the actual tool catalog.
	specs, err := tools.LocalSpecs(nil, workspace)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(specs)+6)
	for _, spec := range specs {
		known[spec.Name] = true
	}
	// These are the child-eligible built-ins registered by agent.ensureTools.
	for _, name := range []string{
		domain.ToolFinishTurn, domain.ToolUpdatePlan, domain.ToolRetrieveHistory,
		domain.ToolRetrieveResource, domain.ToolExecuteProgram, domain.ToolPoll,
	} {
		known[name] = true
	}
	return known, nil
}
