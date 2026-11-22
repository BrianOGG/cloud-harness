package agent

import (
	"context"
	"fmt"

	"github.com/BrianOGG/cloud-harness/internal/domain"
	"github.com/BrianOGG/cloud-harness/internal/hooks"
	"github.com/BrianOGG/cloud-harness/internal/security"
	"github.com/BrianOGG/cloud-harness/internal/storage"
)

// EventConfigUntrusted records that a workspace control file was present at
// run start but ignored because the user has not trusted its content.
const EventConfigUntrusted = "config.untrusted"

// UntrustedNotice is the event payload. It never contains file content.
type UntrustedNotice struct {
	File    string              `json:"file"`
	Path    string              `json:"path"`
	State   security.TrustState `json:"state"`
	SHA256  string              `json:"sha256"`
	Reason  string              `json:"reason,omitempty"`
	Message string              `json:"message"`
}

// UntrustedMessage states what is being ignored and how to change that.
func UntrustedMessage(status security.TrustStatus) string {
	effect := "its hooks are not run"
	if status.File == security.TrustPolicyFile {
		effect = "its allow and ask rules are not applied (deny rules still are)"
	}
	why := "is not trusted"
	if status.State == security.TrustChanged {
		why = "changed since it was trusted"
	}
	msg := "workspace " + status.File + " " + why + ": " + effect + ". Review " + status.Path + ", then run `cloud-harness trust approve`."
	if status.Reason != "" {
		msg += " (" + status.Reason + ")"
	}
	return msg
}

func (m *Manager) noteUntrusted(ctx context.Context, runID string, statuses ...*security.TrustStatus) error {
	for _, status := range statuses {
		if status == nil || !status.Pending() {
			continue
		}
		notice := UntrustedNotice{File: status.File, Path: status.Path, State: status.State, SHA256: status.SHA256, Reason: status.Reason, Message: UntrustedMessage(*status)}
		if err := m.event(ctx, runID, EventConfigUntrusted, notice); err != nil {
			return err
		}
	}
	return nil
}

// legacyHooks reports a hook snapshot that carries no trust decision: either
// none was saved, or it was saved before workspace hooks needed trust and may
// hold hooks nobody approved. Such a snapshot is reloaded under the check.
func legacyHooks(cfg *hooks.Config) bool {
	return cfg == nil || cfg.WorkspaceTrust == nil
}

// approvalWarnings describes what approving a pending file write changes
// beyond the write itself. AGENTS.md and .agents/** are loaded into later
// runs, so one approval there keeps steering the agent afterwards.
func (m *Manager) approvalWarnings(run storage.Run, inv domain.Invocation) []string {
	if m.Broker == nil {
		return nil
	}
	if fact, ok := m.Broker.Lookup(inv.Name); !ok || fact.Readonly {
		return nil
	}
	workspace := m.Workspace
	if run.WorktreePath != "" {
		workspace = run.WorktreePath
	}
	var warnings []string
	seen := make(map[string]bool)
	for _, key := range []string{"path", "file_path", "target_path"} {
		target := jsonString(inv.Arguments, key)
		if target == "" {
			continue
		}
		kind := security.BehaviorFile(workspace, target)
		if kind == "" || seen[kind] {
			continue
		}
		seen[kind] = true
		warnings = append(warnings, BehaviorWarning(kind, target))
	}
	return warnings
}

// BehaviorWarning is the approval warning for one behavior file.
func BehaviorWarning(kind, target string) string {
	what := "AGENTS.md files are project instructions loaded into every later run"
	if kind == security.BehaviorAgents {
		what = ".agents/ holds the skills and agent definitions that later runs load and follow"
	}
	return fmt.Sprintf("WARNING: this writes %s. %s, so approving it changes how later runs in this workspace behave. Review the full content before approving.", target, what)
}
