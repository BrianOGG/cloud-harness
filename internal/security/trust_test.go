package security

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func trustState(t *testing.T, workspace, configDir, name string) TrustStatus {
	t.Helper()
	statuses, err := WorkspaceTrustFrom(workspace, configDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.File == name {
			return status
		}
	}
	t.Fatalf("no status for %s: %+v", name, statuses)
	return TrustStatus{}
}

// An untrusted workspace policy may only restrict. Trust is bound to the
// content hash, so an edit needs a new decision and a revert does not.
func TestWorkspacePolicyRequiresTrust(t *testing.T) {
	workspace, global := t.TempDir(), t.TempDir()
	file := filepath.Join(workspace, ControlDir, TrustPolicyFile)
	body := `{"rules":[
		{"tool":"read_file","path_glob":".env","effect":"allow"},
		{"tool":"read_file","path_glob":"private/**","effect":"deny"},
		{"tool":"run_command","command_prefix":"rm","effect":"ask"},
		{"tool":"write_file","effect":"allow"}]}`
	writePolicyFile(t, file, body)
	writePolicyFile(t, filepath.Join(global, "policy.json"), `{"rules":[{"tool":"run_command","command_prefix":"rm","effect":"deny"}]}`)
	write := PolicyRequest{Tool: "write_file", Paths: []string{"out.txt"}, FileWrite: true}
	env := PolicyRequest{Tool: "read_file", Paths: []string{".env"}, FileRead: true}
	private := PolicyRequest{Tool: "read_file", Paths: []string{"private/a.txt"}, FileRead: true}
	remove := PolicyRequest{Tool: "run_command", Command: "rm -rf build"}
	check := func(state TrustState, want map[*PolicyRequest][2]string) {
		t.Helper()
		p, err := LoadPolicyFrom(workspace, global)
		if err != nil || p.WorkspaceTrust == nil || p.WorkspaceTrust.State != state || p.WorkspaceTrust.Path != file {
			t.Fatalf("load: %+v, %v", p, err)
		}
		// A resumed run evaluates the checkpointed snapshot, not the file.
		raw, _ := json.Marshal(p)
		var restored Policy
		if err := json.Unmarshal(raw, &restored); err != nil {
			t.Fatal(err)
		}
		for req, expect := range want {
			for _, policy := range []*Policy{p, &restored} {
				if got := policy.Evaluate(*req); string(got.Effect) != expect[0] || got.Source != expect[1] {
					t.Fatalf("%s %+v = %+v, want %v", state, *req, got, expect)
				}
			}
		}
	}
	untrusted := map[*PolicyRequest][2]string{
		&write: {"ask", "default"}, &env: {"deny", "builtin"}, &private: {"deny", "workspace"}, &remove: {"deny", "global"},
	}
	trusted := map[*PolicyRequest][2]string{
		&write: {"allow", "workspace"}, &env: {"allow", "workspace"}, &private: {"deny", "workspace"}, &remove: {"ask", "workspace"},
	}
	check(TrustUntrusted, untrusted)
	if err := TrustContent(workspace, global, map[string][]byte{TrustPolicyFile: []byte(body)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	check(TrustTrusted, trusted)
	writePolicyFile(t, file, body+"\n")
	check(TrustChanged, untrusted)
	writePolicyFile(t, file, body)
	check(TrustTrusted, trusted)

	// Checkpoints written before trust existed carry no decision.
	old := Policy{Workspace: workspace, WorkspaceRules: []Rule{{Tool: "write_file", Effect: EffectAllow}}}
	if got := old.Evaluate(write); got.Effect != EffectAsk || got.Source != "default" {
		t.Fatalf("snapshot without a trust decision applied an allow rule: %+v", got)
	}

	if removed, err := RevokeTrust(workspace, global); err != nil || removed != 1 {
		t.Fatalf("revoke = %d, %v", removed, err)
	}
	check(TrustUntrusted, untrusted)
	if removed, err := RevokeTrust(workspace, global); err != nil || removed != 0 {
		t.Fatalf("second revoke = %d, %v", removed, err)
	}
}

func TestTrustStoreRecordsHashesPerWorkspaceAndFile(t *testing.T) {
	first, second, global := t.TempDir(), t.TempDir(), t.TempDir()
	hooksBody, policyBody := `{"hooks":{"SessionStart":[{"type":"command","command":"echo private-hook-command"}]}}`, `{"rules":[]}`
	for _, workspace := range []string{first, second} {
		writePolicyFile(t, filepath.Join(workspace, ControlDir, TrustHooksFile), hooksBody)
		writePolicyFile(t, filepath.Join(workspace, ControlDir, TrustPolicyFile), policyBody)
	}
	if got := trustState(t, t.TempDir(), global, TrustHooksFile); got.State != TrustMissing || got.SHA256 != "" || got.Pending() || got.Effective() {
		t.Fatalf("missing file = %+v", got)
	}
	if err := TrustContent(first, global, map[string][]byte{TrustHooksFile: []byte(hooksBody)}, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	// The same content in another workspace, or another file in this one, is
	// a separate decision.
	for _, tc := range []struct {
		workspace, file string
		state           TrustState
	}{{first, TrustHooksFile, TrustTrusted}, {first, TrustPolicyFile, TrustUntrusted}, {second, TrustHooksFile, TrustUntrusted}} {
		if got := trustState(t, tc.workspace, global, tc.file); got.State != tc.state || got.SHA256 != policyHash(map[string]string{TrustHooksFile: hooksBody, TrustPolicyFile: policyBody}[tc.file]) {
			t.Fatalf("%s %s = %+v, want %s", tc.workspace, tc.file, got, tc.state)
		}
	}
	raw, err := os.ReadFile(TrustStorePath(global))
	if err != nil || strings.Contains(string(raw), "private-hook-command") || !strings.Contains(string(raw), policyHash(hooksBody)) {
		t.Fatalf("trust store = %s, %v", raw, err)
	}
	if info, err := os.Stat(TrustStorePath(global)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("trust store mode = %v, %v", info.Mode(), err)
	}
	// Re-trusting replaces the earlier hash instead of accumulating entries.
	if err := TrustContent(first, global, map[string][]byte{TrustHooksFile: []byte(`{}`)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := trustState(t, first, global, TrustHooksFile); got.State != TrustChanged {
		t.Fatalf("replaced decision = %+v", got)
	}
	// A run worktree shares the decision recorded for its workspace.
	worktree := filepath.Join(first, ControlDir, "worktrees", "run_1")
	writePolicyFile(t, filepath.Join(worktree, ControlDir, TrustHooksFile), `{}`)
	if got := trustState(t, worktree, global, TrustHooksFile); got.State != TrustTrusted || got.Workspace == worktree {
		t.Fatalf("worktree decision = %+v", got)
	}
	writePolicyFile(t, filepath.Join(worktree, ControlDir, TrustHooksFile), hooksBody)
	if got := trustState(t, worktree, global, TrustHooksFile); got.State != TrustChanged {
		t.Fatalf("worktree content differing from the trusted hash = %+v", got)
	}

	writePolicyFile(t, TrustStorePath(global), `{"version":1,"entries":[`)
	if _, err := WorkspaceTrustFrom(first, global); err == nil || !strings.Contains(err.Error(), TrustStorePath(global)) {
		t.Fatalf("corrupt trust store error = %v", err)
	}
	if _, err := LoadPolicyFrom(first, global); err == nil {
		t.Fatal("policy loaded with a corrupt trust store")
	}
}

// A trust store the agent could write with its workspace file tools is not
// evidence of a user decision.
func TestTrustStoreInsideWorkspaceIsNeverTrusted(t *testing.T) {
	workspace := t.TempDir()
	body := `{"rules":[{"tool":"*","effect":"allow"}]}`
	writePolicyFile(t, filepath.Join(workspace, ControlDir, TrustPolicyFile), body)
	for _, configDir := range []string{workspace, filepath.Join(workspace, "config")} {
		if err := TrustContent(workspace, configDir, map[string][]byte{TrustPolicyFile: []byte(body)}, time.Now()); err == nil || !strings.Contains(err.Error(), "inside the workspace") {
			t.Fatalf("recorded trust inside the workspace: %v", err)
		}
		// Even a forged store with the right hash is ignored.
		forged, _ := json.Marshal(trustStore{Version: 1, Entries: []trustEntry{{Workspace: workspace, File: TrustPolicyFile, SHA256: policyHash(body)}}})
		writePolicyFile(t, TrustStorePath(configDir), string(forged))
		got := trustState(t, workspace, configDir, TrustPolicyFile)
		if got.State != TrustUntrusted || !strings.Contains(got.Reason, "inside the workspace") {
			t.Fatalf("status = %+v", got)
		}
		p, err := LoadPolicyFrom(workspace, configDir)
		if err != nil {
			t.Fatal(err)
		}
		if d := p.Evaluate(PolicyRequest{Tool: "write_file", Paths: []string{"out.txt"}, FileWrite: true}); d.Effect != EffectAsk {
			t.Fatalf("forged trust applied an allow rule: %+v", d)
		}
	}
}

func TestControlPathFollowsSymlinks(t *testing.T) {
	workspace := t.TempDir()
	p, err := LoadPolicyFrom(workspace, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writePolicyFile(t, filepath.Join(workspace, ControlDir, "harness.db"), "sqlite")
	if err := os.Symlink(ControlDir, filepath.Join(workspace, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ControlDir, "harness.db"), filepath.Join(workspace, "data.txt")); err != nil {
		t.Fatal(err)
	}
	for _, req := range []PolicyRequest{
		{Tool: "read_file", Paths: []string{"alias/harness.db"}, FileRead: true},
		{Tool: "read_file", Paths: []string{"data.txt"}, FileRead: true},
		{Tool: "write_file", Paths: []string{"alias/hooks.json"}, FileWrite: true},
		{Tool: "write_file", Paths: []string{filepath.Join(workspace, ControlDir, "new", "file")}, FileWrite: true},
		{Tool: "read_files", Paths: []string{"README.md", "src/../.cloud-harness/policy.json"}, FileRead: true},
	} {
		if got := p.Evaluate(req); got.Effect != EffectDeny || got.Source != "builtin" || got.RuleIndex != 3 {
			t.Fatalf("%+v = %+v", req, got)
		}
	}
}

func TestBehaviorFile(t *testing.T) {
	workspace := t.TempDir()
	for _, dir := range []string{".agents/skills", "sub", "plain"} {
		if err := os.MkdirAll(filepath.Join(workspace, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(workspace, ".agents"), filepath.Join(workspace, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(workspace, "AGENTS.md"), filepath.Join(workspace, "plain", "notes.md")); err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]string{
		".agents/skills/deploy/SKILL.md":              BehaviorAgents,
		".agents/agents/reviewer.md":                  BehaviorAgents,
		".AGENTS/skills/x.md":                         BehaviorAgents,
		"alias/skills/x.md":                           BehaviorAgents,
		filepath.Join(workspace, ".agents", "new.md"): BehaviorAgents,
		"sub/../.agents/x":                            BehaviorAgents,
		"AGENTS.md":                                   BehaviorRules,
		"agents.md":                                   BehaviorRules,
		"sub/AGENTS.md":                               BehaviorRules,
		"plain/notes.md":                              BehaviorRules,
		"sub/.agents/x.md":                            "",
		"AGENTS.md.bak":                               "",
		"plain/other.md":                              "",
		"agents/readme.md":                            "",
		filepath.Join(filepath.Dir(workspace), "AGENTS.md"): "",
	} {
		if got := BehaviorFile(workspace, target); got != want {
			t.Errorf("BehaviorFile(%q) = %q, want %q", target, got, want)
		}
	}
}

// A control directory that is itself a symlink keeps its protection under
// the real name of the directory it points at.
func TestControlPathFollowsSymlinkedControlDirectory(t *testing.T) {
	workspace := t.TempDir()
	writePolicyFile(t, filepath.Join(workspace, "state", "harness.db"), "sqlite")
	writePolicyFile(t, filepath.Join(workspace, "src", "main.go"), "package main")
	if err := os.Symlink("state", filepath.Join(workspace, ControlDir)); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicyFrom(workspace, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []PolicyRequest{
		{Tool: "read_file", Paths: []string{"state/harness.db"}, FileRead: true},
		{Tool: "write_file", Paths: []string{"state/hooks.json"}, FileWrite: true},
		{Tool: "read_file", Paths: []string{filepath.Join(workspace, "state")}, FileRead: true},
		{Tool: "read_file", Paths: []string{".cloud-harness/harness.db"}, FileRead: true},
	} {
		if got := p.Evaluate(req); got.Effect != EffectDeny || got.Source != "builtin" || got.RuleIndex != 3 {
			t.Fatalf("%+v = %+v", req, got)
		}
	}
	for _, req := range []PolicyRequest{
		{Tool: "read_file", Paths: []string{"src/main.go"}, FileRead: true},
		{Tool: "read_file", Paths: []string{"state-notes.txt"}, FileRead: true},
		{Tool: "grep", Paths: []string{workspace}, FileRead: true},
	} {
		if got := p.Evaluate(req); got.Effect == EffectDeny {
			t.Fatalf("%+v = %+v", req, got)
		}
	}
}
