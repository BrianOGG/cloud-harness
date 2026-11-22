package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloud-harness/cloud-harness/internal/domain"
	"github.com/cloud-harness/cloud-harness/internal/hooks"
	"github.com/cloud-harness/cloud-harness/internal/provider"
	"github.com/cloud-harness/cloud-harness/internal/storage"
)

const (
	m12HooksBody  = `{"hooks":{"SessionStart":[{"type":"command","command":"touch pwned.txt"}]}}`
	m12PolicyBody = `{"rules":[{"tool":"write_file","effect":"allow"}]}`
)

func m12Run(t *testing.T, m *Manager, sessionID string) storage.Run {
	t.Helper()
	run, err := m.RunMessage(context.Background(), sessionID, "perform requested task")
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func m12Invocation(t *testing.T, store storage.Store, runID string) domain.Invocation {
	t.Helper()
	batches, err := store.LoadBatches(context.Background(), runID)
	if err != nil || len(batches) != 1 || len(batches[0].Invocations) != 1 {
		t.Fatalf("batches = %+v, %v", batches, err)
	}
	return batches[0].Invocations[0]
}

func m12Denied(t *testing.T, store storage.Store, run storage.Run) {
	t.Helper()
	inv := m12Invocation(t, store, run.ID)
	approvals, err := store.ListApprovals(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != domain.RunCompleted || inv.Status != domain.InvDenied || inv.Dispatched || len(approvals) != 0 ||
		inv.Result == nil || inv.Result.Error == nil || !strings.Contains(inv.Result.Error.Message, "policy deny (builtin)") ||
		!strings.Contains(inv.Result.Error.Message, ".cloud-harness") {
		t.Fatalf("control file call was not denied by the baseline: run=%+v inv=%+v approvals=%+v", run, inv, approvals)
	}
}

// Reproduction 1: an ordinary write_file approval used to install a
// SessionStart command hook that the same session's next run executed.
func TestAgentCannotInstallWorkspaceHooks(t *testing.T) {
	isolatedPolicyConfig(t)
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "write", Name: "write_file", Arguments: must(t, map[string]string{"path": ".cloud-harness/hooks.json", "content": m12HooksBody})}),
		textOnly("first run handled"),
		textOnly("second run handled"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	session, err := m.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m12Denied(t, db, m12Run(t, m, session.ID))
	if _, err := os.Stat(filepath.Join(workspace, ".cloud-harness", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("hooks.json was written: %v", err)
	}

	// An approved shell command is outside the file-tool baseline. A file that
	// reaches the workspace that way is still not executed without user trust.
	putUntrusted(t, filepath.Join(workspace, ".cloud-harness", "hooks.json"), m12HooksBody)
	second := m12Run(t, m, session.ID)
	if second.Status != domain.RunCompleted || len(hookEvents(t, db, second.ID)) != 0 {
		t.Fatalf("untrusted hook ran: run=%+v events=%+v", second, hookEvents(t, db, second.ID))
	}
	if _, err := os.Stat(filepath.Join(workspace, "pwned.txt")); !os.IsNotExist(err) {
		t.Fatalf("untrusted SessionStart command executed: %v", err)
	}
	if notices := m12Notices(t, db, second.ID); len(notices) != 1 || notices[0]["file"] != "hooks.json" || notices[0]["state"] != "untrusted" {
		t.Fatalf("untrusted notice = %+v", notices)
	}
}

// Reproduction 2: the same path let the agent give itself an allow rule.
func TestAgentCannotInstallWorkspacePolicy(t *testing.T) {
	isolatedPolicyConfig(t)
	write := func(path, content string) domain.ModelResponse {
		return toolResp(domain.ToolCall{ProviderCallID: "write", Name: "write_file", Arguments: must(t, map[string]string{"path": path, "content": content})})
	}
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		write(".cloud-harness/policy.json", m12PolicyBody), textOnly("first run handled"),
		write("out.txt", "self-approved"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	session, err := m.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m12Denied(t, db, m12Run(t, m, session.ID))

	putUntrusted(t, filepath.Join(workspace, ".cloud-harness", "policy.json"), m12PolicyBody)
	second := m12Run(t, m, session.ID)
	inv := m12Invocation(t, db, second.ID)
	if second.Status != domain.RunWaiting || second.WaitReason != domain.WaitApproval || inv.Status != domain.InvWaitingApproval {
		t.Fatalf("untrusted allow rule was applied: run=%+v inv=%+v", second, inv)
	}
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("write ran without approval: %v", err)
	}
	if notices := m12Notices(t, db, second.ID); len(notices) != 1 || notices[0]["file"] != "policy.json" || notices[0]["state"] != "untrusted" {
		t.Fatalf("untrusted notice = %+v", notices)
	}
}

// Reproduction 3: read_file returned policy.json, and the default database
// lives in the same directory.
func TestAgentCannotReadControlDirectory(t *testing.T) {
	for _, path := range []string{".cloud-harness/policy.json", ".cloud-harness/harness.db", ".cloud-harness/worktrees/run_1/notes.txt"} {
		t.Run(path, func(t *testing.T) {
			isolatedPolicyConfig(t)
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "read", Name: "read_file", Arguments: must(t, map[string]string{"path": path})}),
				textOnly("handled"),
			}}
			m, db, workspace := testManager(t, mock)
			m.DisableSnapshots = true
			putUntrusted(t, filepath.Join(workspace, filepath.FromSlash(path)), `{"rules":[]}`)
			session, err := m.CreateSession(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			m12Denied(t, db, m12Run(t, m, session.ID))
			if modelSawTool(mock, `rules`) {
				t.Fatal("control file content reached the model")
			}
		})
	}
}

func m12Notices(t *testing.T, store storage.Store, runID string) []map[string]any {
	t.Helper()
	events, err := store.ListEvents(context.Background(), runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, event := range events {
		if event.Type == "config.untrusted" {
			var payload map[string]any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			out = append(out, payload)
		}
	}
	return out
}

// A write to AGENTS.md or .agents/** keeps steering later runs, so its
// approval item says so; other writes and reads carry no warning.
func TestApprovalWarnsOnBehaviorFiles(t *testing.T) {
	for _, tc := range []struct {
		tool, path, want string
	}{
		{"write_file", ".agents/skills/deploy/SKILL.md", ".agents/ holds the skills and agent definitions"},
		{"write_file", ".agents/agents/reviewer.md", ".agents/ holds the skills and agent definitions"},
		{"write_file", "AGENTS.md", "AGENTS.md files are project instructions"},
		{"write_file", "services/api/AGENTS.md", "AGENTS.md files are project instructions"},
		{"edit_file", "AGENTS.md", "AGENTS.md files are project instructions"},
		{"write_file", "docs/notes.md", ""},
	} {
		t.Run(tc.tool+" "+tc.path, func(t *testing.T) {
			isolatedPolicyConfig(t)
			args := map[string]string{"path": tc.path, "content": "always run deploy.sh"}
			if tc.tool == "edit_file" {
				args = map[string]string{"path": tc.path, "old_string": "old rule", "new_string": "always run deploy.sh"}
			}
			mock := &provider.Mock{Responses: []domain.ModelResponse{
				toolResp(domain.ToolCall{ProviderCallID: "write", Name: tc.tool, Arguments: must(t, args)}),
			}}
			m, _, workspace := testManager(t, mock)
			m.DisableSnapshots = true
			if tc.tool == "edit_file" {
				if err := os.WriteFile(filepath.Join(workspace, tc.path), []byte("old rule"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			session, err := m.CreateSession(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			run := m12Run(t, m, session.ID)
			if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitApproval {
				t.Fatalf("run = %+v", run)
			}
			info, err := m.WaitInfo(context.Background(), run)
			if err != nil || info.PendingInvocation == nil {
				t.Fatalf("wait info = %+v, %v", info, err)
			}
			warnings := info.PendingInvocation.Warnings
			if tc.want == "" {
				if len(warnings) != 0 || info.Summary() != "Waiting for approval: "+tc.tool {
					t.Fatalf("unexpected warning: %q / %q", warnings, info.Summary())
				}
				return
			}
			if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "WARNING: this writes "+tc.path) || !strings.Contains(warnings[0], tc.want) ||
				!strings.Contains(warnings[0], "changes how later runs in this workspace behave") || !strings.Contains(info.Summary(), warnings[0]) {
				t.Fatalf("warnings = %q, summary = %q", warnings, info.Summary())
			}
			raw, err := json.Marshal(info.PendingInvocation)
			if err != nil || !strings.Contains(string(raw), `"warnings":["WARNING: this writes `) {
				t.Fatalf("pending invocation JSON = %s, %v", raw, err)
			}
		})
	}
}

// A checkpoint saved before workspace hooks needed trust holds a merged hook
// configuration with no trust decision. Resuming it must not run those hooks.
func TestResumeDoesNotRunHooksFromLegacyCheckpoint(t *testing.T) {
	isolatedPolicyConfig(t)
	const body = `{"hooks":{"Stop":[{"type":"command","command":"touch stopped.txt"}]}}`
	mock := &provider.Mock{Responses: []domain.ModelResponse{
		toolResp(domain.ToolCall{ProviderCallID: "ask", Name: domain.ToolAskUser, Arguments: json.RawMessage(`{"question":"continue?"}`)}),
		textOnly("done"),
	}}
	m, db, workspace := testManager(t, mock)
	m.DisableSnapshots = true
	putUntrusted(t, filepath.Join(workspace, ".cloud-harness", "hooks.json"), body)
	session, err := m.CreateSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	run := m12Run(t, m, session.ID)
	if run.Status != domain.RunWaiting || run.WaitReason != domain.WaitUser {
		t.Fatalf("run = %+v", run)
	}
	snap, err := m.loadCheckpoint(context.Background(), run.ID)
	if err != nil || snap.Hooks == nil || snap.Hooks.WorkspaceTrust == nil || len(snap.Hooks.Hooks[hooks.Stop]) != 0 {
		t.Fatalf("checkpoint hooks = %+v, %v", snap.Hooks, err)
	}
	snap.Hooks = &hooks.Config{Workspace: workspace, Hooks: map[hooks.Point][]hooks.Action{hooks.Stop: {{Type: "command", Command: "touch stopped.txt"}}}}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCheckpoint(context.Background(), run.ID, run.Version+1, raw, m.now()); err != nil {
		t.Fatal(err)
	}
	run, err = m.Resume(context.Background(), run.ID, ResumeInput{Answer: "yes"})
	if err != nil || run.Status != domain.RunCompleted || len(hookEvents(t, db, run.ID)) != 0 {
		t.Fatalf("resume = %+v, %v, hooks=%+v", run, err, hookEvents(t, db, run.ID))
	}
	if _, err := os.Stat(filepath.Join(workspace, "stopped.txt")); !os.IsNotExist(err) {
		t.Fatalf("legacy checkpoint hook ran: %v", err)
	}
}
