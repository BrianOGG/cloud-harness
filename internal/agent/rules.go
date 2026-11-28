package agent

import (
	"context"
	"log"
	"path/filepath"
	"slices"
	"strings"

	contextpkg "github.com/BrianOGG/cloud-harness/internal/context"
	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/provider"
	"github.com/BrianOGG/cloud-harness/internal/security"
	"github.com/BrianOGG/cloud-harness/internal/storage"
	"github.com/BrianOGG/cloud-harness/internal/tools"
)

// ruleTargets uses the same resolved resources as built-in tool preflight.
// MCP resource strings and arbitrary shell text do not name rule directories.
func (m *Manager) ruleTargets(batch domain.Batch, st *loopState) []string {
	if m.Broker == nil {
		return nil
	}
	var paths []string
	for _, inv := range batch.Invocations {
		switch inv.Name {
		case "read_file", "write_file", "edit_file", "git_diff", "list_dir", "list_files", "grep", "run_command":
		default:
			continue
		}
		if st.planTools.unknown(inv.Name) != nil || (m.Depth > 0 && (m.definitionToolLimit || len(m.AllowedTools) > 0) && !slices.Contains(m.AllowedTools, inv.Name)) {
			continue
		}
		if st.policy != nil && st.policy.Evaluate(m.policyRequest(inv)).Effect == security.EffectDeny {
			continue
		}
		resources, err := m.Broker.ExtractRequestResources(tools.Request{Tool: inv.Name, Args: inv.Arguments, ApprovalMode: st.approvalMode})
		if err != nil {
			continue
		}
		if inv.Name == "run_command" && len(resources) == 1 && resources[0] == strings.TrimSpace(jsonString(inv.Arguments, "command")) {
			// An exact-command grant contains shell text, even when it starts
			// with a slash. Only the validated cwd identifies a directory.
			if root, err := filepath.Abs(m.Workspace); err == nil {
				if cwd, err := tools.CommandWorkDir(root, jsonString(inv.Arguments, "cwd"), st.approvalMode); err == nil {
					paths = append(paths, cwd)
				}
			}
			continue
		}
		for _, resource := range resources {
			if filepath.IsAbs(resource) {
				paths = append(paths, resource)
			}
		}
	}
	return paths
}

func (m *Manager) injectProjectRules(ctx context.Context, run storage.Run, messages []provider.Message, st *loopState, paths []string) []provider.Message {
	loaded := st.ruleScope.Next(m.Workspace, paths, func(path string) bool {
		return st.policy == nil || st.policy.Evaluate(security.PolicyRequest{Tool: "read_file", Paths: []string{path}, FileRead: true}).Effect != security.EffectDeny
	})
	for _, diagnostic := range loaded.Diagnostics {
		log.Printf("project rules skipped: %s: %s", diagnostic.Path, diagnostic.Error)
		_ = m.event(ctx, run.ID, "rules.skipped", diagnostic)
	}
	if len(loaded.Omitted) > 0 {
		_ = m.event(ctx, run.ID, "rules.truncated", map[string]any{"files": loaded.Omitted})
	}
	if loaded.Content == "" {
		return messages
	}
	asm := contextpkg.WithProjectRules(contextpkg.Assembly{
		StaticPrefix: toContext(messages[:st.prefixN]),
	}, loaded.Content)
	assembled := append(fromContext(asm.StaticPrefix), messages[st.prefixN:]...)
	st.prefixN++
	st.userAt++
	_ = m.event(ctx, run.ID, "rules.loaded", map[string]any{"files": loaded.Loaded})
	// Existing wait/budget checkpoints persist these messages and prefix count;
	// no extra checkpoint is needed (or allowed to compete at the same version).
	return assembled
}
