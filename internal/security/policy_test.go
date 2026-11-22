package security

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BrianOGG/cloud-harness/internal/domain"
)

func writePolicyFile(t *testing.T, file, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// trustPolicyFile writes the workspace policy.json and records the user's
// trust in that exact content, as `cloud-harness trust approve` does.
func trustPolicyFile(t *testing.T, workspace, configDir, body string) {
	t.Helper()
	writePolicyFile(t, filepath.Join(workspace, ControlDir, TrustPolicyFile), body)
	if err := TrustContent(workspace, configDir, map[string][]byte{TrustPolicyFile: []byte(body)}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyEvaluationMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, workspace, global string
		req                     PolicyRequest
		effect                  Effect
		source                  string
	}{
		{"workspace overrides global", `{"rules":[{"tool":"write_file","effect":"allow"}]}`, `{"rules":[{"tool":"*","effect":"deny"}]}`, PolicyRequest{Tool: "write_file", Paths: []string{"out.txt"}, FileWrite: true}, EffectAllow, "workspace"},
		{"first workspace match wins", `{"rules":[{"tool":"read_file","effect":"ask"},{"tool":"*","effect":"deny"}]}`, "", PolicyRequest{Tool: "read_file", Paths: []string{".env"}, FileRead: true}, EffectAsk, "workspace"},
		{"global match", `{"rules":[{"tool":"other","effect":"allow"}]}`, `{"rules":[{"tool":"read_file","path_glob":"docs/**","effect":"allow"}]}`, PolicyRequest{Tool: "read_file", Paths: []string{"docs/deep/a.md"}, FileRead: true}, EffectAllow, "global"},
		{"explicit env allow", `{"rules":[{"tool":"read_file","path_glob":".env","effect":"allow"}]}`, "", PolicyRequest{Tool: "read_file", Paths: []string{".env"}, FileRead: true}, EffectAllow, "workspace"},
		{"env baseline", "", "", PolicyRequest{Tool: "read_file", Paths: []string{".env"}, FileRead: true}, EffectDeny, "builtin"},
		{"nested env variant", "", "", PolicyRequest{Tool: "read_file", Paths: []string{"nested/.env.production"}, FileRead: true}, EffectDeny, "builtin"},
		{"env example", "", "", PolicyRequest{Tool: "read_file", Paths: []string{"nested/.env.example"}, FileRead: true}, EffectAsk, "default"},
		{"env example variant remains protected", "", "", PolicyRequest{Tool: "read_file", Paths: []string{".env.example.local"}, FileRead: true}, EffectDeny, "builtin"},
		{"write outside", "", "", PolicyRequest{Tool: "write_file", Paths: []string{"../out.txt"}, FileWrite: true}, EffectDeny, "builtin"},
		{"write inside", "", "", PolicyRequest{Tool: "write_file", Paths: []string{"new/deep/out.txt"}, FileWrite: true}, EffectAsk, "default"},
		{"control read", "", "", PolicyRequest{Tool: "read_file", Paths: []string{".cloud-harness/policy.json"}, FileRead: true}, EffectDeny, "builtin"},
		{"control database read", "", "", PolicyRequest{Tool: "read_file", Paths: []string{".cloud-harness/harness.db"}, FileRead: true}, EffectDeny, "builtin"},
		{"control write", "", "", PolicyRequest{Tool: "write_file", Paths: []string{".cloud-harness/hooks.json"}, FileWrite: true}, EffectDeny, "builtin"},
		{"control edit of worktree metadata", "", "", PolicyRequest{Tool: "edit_file", Paths: []string{"./.cloud-harness/worktrees/run_1/a.go"}, FileWrite: true}, EffectDeny, "builtin"},
		{"control directory listing", "", "", PolicyRequest{Tool: "list_files", Paths: []string{".cloud-harness"}, FileRead: true}, EffectDeny, "builtin"},
		{"control name ignores case", "", "", PolicyRequest{Tool: "read_file", Paths: []string{".Cloud-Harness/policy.json"}, FileRead: true}, EffectDeny, "builtin"},
		{"control baseline cannot be allowed", `{"rules":[{"tool":"*","effect":"allow"}]}`, `{"rules":[{"tool":"*","path_glob":".cloud-harness/**","effect":"allow"}]}`, PolicyRequest{Tool: "write_file", Paths: []string{".cloud-harness/policy.json"}, FileWrite: true}, EffectDeny, "builtin"},
		{"control lookalike is ordinary", "", "", PolicyRequest{Tool: "read_file", Paths: []string{".cloud-harness-notes/a.md"}, FileRead: true}, EffectAsk, "default"},
		{"nested control name is ordinary", "", "", PolicyRequest{Tool: "read_file", Paths: []string{"vendor/.cloud-harness/a.md"}, FileRead: true}, EffectAsk, "default"},
		{"command cwd is not a file access", "", "", PolicyRequest{Tool: "run_command", Paths: []string{".cloud-harness"}, Command: "ls"}, EffectAsk, "default"},
		{"command prefix", `{"rules":[{"tool":"run_command","command_prefix":"go test","effect":"allow"}]}`, "", PolicyRequest{Tool: "run_command", Command: "  go test ./..."}, EffectAllow, "workspace"},
		{"command boundary", `{"rules":[{"tool":"run_command","command_prefix":"go test","effect":"allow"}]}`, "", PolicyRequest{Tool: "run_command", Command: "go testevil ./..."}, EffectAsk, "default"},
		{"command tool required", `{"rules":[{"tool":"*","command_prefix":"go test","effect":"allow"}]}`, "", PolicyRequest{Tool: "read_file", Command: "go test ./..."}, EffectAsk, "default"},
		{"program prefix", `{"rules":[{"tool":"execute_program","command_prefix":"def main():","effect":"deny"}]}`, "", PolicyRequest{Tool: "execute_program", Command: "def main():\n  return 1"}, EffectDeny, "workspace"},
		{"selectors conjunctive", `{"rules":[{"tool":"run_command","path_glob":"docs/**","command_prefix":"go test","effect":"allow"}]}`, "", PolicyRequest{Tool: "run_command", Paths: []string{"src"}, Command: "go test ./..."}, EffectAsk, "default"},
		{"allow cannot widen multiple paths", `{"rules":[{"tool":"*","path_glob":"docs/**","effect":"allow"}]}`, "", PolicyRequest{Tool: "read_files", Paths: []string{"docs/a.md", "secrets/a.md"}, FileRead: true}, EffectAsk, "default"},
		{"deny any targeted path", `{"rules":[{"tool":"*","path_glob":"secrets/**","effect":"deny"}]}`, "", PolicyRequest{Tool: "read_files", Paths: []string{"docs/a.md", "secrets/a.md"}, FileRead: true}, EffectDeny, "workspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, global := t.TempDir(), t.TempDir()
			if tc.workspace != "" {
				trustPolicyFile(t, workspace, global, tc.workspace)
			}
			if tc.global != "" {
				writePolicyFile(t, filepath.Join(global, "policy.json"), tc.global)
			}
			p, err := LoadPolicyFrom(workspace, global)
			if err != nil {
				t.Fatal(err)
			}
			got := p.Evaluate(tc.req)
			if got.Effect != tc.effect || got.Source != tc.source || got.Tool != tc.req.Tool || got.Rule == "" {
				t.Fatalf("decision = %+v, want %s/%s", got, tc.effect, tc.source)
			}
		})
	}
}

func TestPolicyStrictLoading(t *testing.T) {
	for _, tc := range []struct{ name, body, problem string }{
		{"bad JSON", `{"rules":[`, "EOF"},
		{"bad effect", `{"rules":[{"tool":"*","effect":"permit"}]}`, "effect"},
		{"missing tool", `{"rules":[{"effect":"ask"}]}`, "tool"},
		{"tool wildcard", `{"rules":[{"tool":"read_*","effect":"allow"}]}`, "tool"},
		{"bad optional type", `{"rules":[{"tool":"*","effect":"ask","path_glob":42}]}`, "path_glob"},
		{"null optional", `{"rules":[{"tool":"*","effect":"ask","command_prefix":null}]}`, "command_prefix"},
		{"empty prefix", `{"rules":[{"tool":"*","effect":"ask","command_prefix":" "}]}`, "command_prefix"},
		{"prefix for non-command tool", `{"rules":[{"tool":"read_file","effect":"ask","command_prefix":"echo"}]}`, "command_prefix"},
		{"bad glob", `{"rules":[{"tool":"*","effect":"ask","path_glob":"a/["}]}`, "path_glob"},
		{"bad doublestar", `{"rules":[{"tool":"*","effect":"ask","path_glob":"a/**x"}]}`, "path_glob"},
		{"unknown rule field", `{"rules":[{"tool":"*","effect":"ask","typo":true}]}`, "typo"},
		{"unknown document field", `{"rulez":[]}`, "rulez"},
		{"missing rules", `{}`, "rules"},
		{"null rules", `{"rules":null}`, "rules"},
		{"null rule", `{"rules":[null]}`, "tool"},
		{"trailing JSON", `{"rules":[]} {}`, "one JSON"},
	} {
		for _, layer := range []string{"workspace", "global"} {
			t.Run(tc.name+"/"+layer, func(t *testing.T) {
				workspace, global := t.TempDir(), t.TempDir()
				file := filepath.Join(workspace, ".cloud-harness", "policy.json")
				if layer == "global" {
					file = filepath.Join(global, "policy.json")
				}
				writePolicyFile(t, file, tc.body)
				_, err := LoadPolicyFrom(workspace, global)
				if err == nil || !strings.Contains(err.Error(), file) || !strings.Contains(err.Error(), tc.problem) {
					t.Fatalf("load error = %v, want file %s and %q", err, file, tc.problem)
				}
			})
		}
	}
}

func TestPolicyMissingFilesAndConfigDirectory(t *testing.T) {
	workspace, global := t.TempDir(), t.TempDir()
	p, err := LoadPolicyFrom(workspace, global)
	if err != nil || len(p.WorkspaceRules) != 0 || len(p.GlobalRules) != 0 {
		t.Fatalf("missing policies = %+v, %v", p, err)
	}
	// A failed snapshot-directory setup must not make a nonexistent policy a
	// fatal configuration error (the snapshot subsystem reports its own error).
	writePolicyFile(t, filepath.Join(workspace, ".cloud-harness"), "blocked")
	if _, err := LoadPolicyFrom(workspace, global); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUD_HARNESS_CONFIG", filepath.Join(global, "settings.json"))
	if got, err := ConfigDirectory(); err != nil || got != global {
		t.Fatalf("configuration directory = %q, %v", got, err)
	}
	t.Setenv("CLOUD_HARNESS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", global)
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ConfigDirectory(); err != nil || got != filepath.Join(dir, "cloud-harness") {
		t.Fatalf("default configuration directory = %q, %v", got, err)
	}
}

func TestPolicySymlinksAndPathBoundaries(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	p, err := LoadPolicyFrom(workspace, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writePolicyFile(t, filepath.Join(workspace, ".env"), "not recorded")
	if err := os.Symlink(".env", filepath.Join(workspace, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, req := range []PolicyRequest{
		{Tool: "read_file", Paths: []string{"alias.txt"}, FileRead: true},
		{Tool: "write_file", Paths: []string{"escape/new/deep.txt"}, FileWrite: true},
		{Tool: "write_file", Paths: []string{workspace + "-sibling/out.txt"}, FileWrite: true},
	} {
		if got := p.Evaluate(req); got.Effect != EffectDeny || got.Source != "builtin" {
			t.Fatalf("symlink/boundary request %+v = %+v", req, got)
		}
	}
	if err := os.Symlink("new/inside.txt", filepath.Join(workspace, "new-alias")); err != nil {
		t.Fatal(err)
	}
	if got := p.Evaluate(PolicyRequest{Tool: "write_file", Paths: []string{"new-alias"}, FileWrite: true}); got.Effect != EffectAsk || got.Source != "default" {
		t.Fatalf("symlink to a new in-workspace file should retain the approval path: %+v", got)
	}
	if err := os.Symlink(filepath.Join(outside, "missing"), filepath.Join(workspace, "outside-alias")); err != nil {
		t.Fatal(err)
	}
	if got := p.Evaluate(PolicyRequest{Tool: "write_file", Paths: []string{"outside-alias/deep/new.txt"}, FileWrite: true}); got.Effect != EffectDeny || got.Source != "builtin" {
		t.Fatalf("symlink to a new outside file should be denied: %+v", got)
	}
	for _, tc := range []struct{ pattern, target string }{
		{"**/.env", ".env"}, {"**/.env", "a/b/.env"},
		{"docs/**/*.md", "docs/a.md"}, {"docs/**/*.md", "docs/a/b.md"},
		{"docs/[ab]?.md", "docs/a1.md"},
	} {
		if !matchPolicyGlob(tc.pattern, tc.target) {
			t.Fatalf("%q did not match %q", tc.pattern, tc.target)
		}
	}
}

func TestPolicySnapshotAndEventRedaction(t *testing.T) {
	workspace, global := t.TempDir(), t.TempDir()
	file := filepath.Join(workspace, ".cloud-harness", "policy.json")
	writePolicyFile(t, file, `{"rules":[{"tool":"run_command","command_prefix":"echo","effect":"deny"}]}`)
	p, err := LoadPolicyFrom(workspace, global)
	if err != nil {
		t.Fatal(err)
	}
	writePolicyFile(t, file, `{"rules":[{"tool":"*","effect":"allow"}]}`)
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restored Policy
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	decision := restored.Evaluate(PolicyRequest{Tool: "run_command", Command: "echo private-command-argument"})
	event, _ := json.Marshal(decision)
	if decision.Effect != EffectDeny || decision.CommandSHA256 == "" || strings.Contains(string(event), "echo") || strings.Contains(string(event), "private-command-argument") {
		t.Fatalf("unsafe or changed decision: %s", event)
	}
}

func TestPolicyPermissionHardConstraintsAndChildGrants(t *testing.T) {
	allow := PolicyDecision{Effect: EffectAllow, Source: "workspace", Rule: "tool=* effect=allow"}
	base := Request{Tool: "write_file", Resources: []string{"/ws/a.txt"}, SideEffectClass: SideReversible, ApprovalPossible: true, Policy: &allow}
	if got := Decide(nil, base); !got.Allow || got.NeedsApproval {
		t.Fatal(got)
	}
	base.RestrictPolicyAllow = true
	if got := Decide(nil, base); got.Allow || !got.NeedsApproval {
		t.Fatalf("policy widened child grants: %+v", got)
	}
	base.UnresolvedResources = true
	if got := Decide(nil, base); got.Allow || got.NeedsApproval {
		t.Fatalf("unresolved scope allowed: %+v", got)
	}
	base.UnresolvedResources, base.RestrictPolicyAllow = false, false
	base.SensitiveSeen, base.Egress = true, true
	if got := Decide(nil, base); got.Allow || got.NeedsApproval {
		t.Fatalf("policy bypassed egress constraint: %+v", got)
	}
	deny := PolicyDecision{Effect: EffectDeny, Source: "global", Rule: "tool=* effect=deny"}
	base.SensitiveSeen, base.Egress, base.Policy = false, false, &deny
	if got := Decide([]domain.Grant{{Tool: "write_file", ResourceScope: "/ws"}}, base); got.Allow || got.NeedsApproval || !strings.Contains(got.Reason, "global") {
		t.Fatalf("grant bypassed policy deny: %+v", got)
	}
}

func TestPolicyGrantCannotWidenInvocationOrMultipleResources(t *testing.T) {
	g := domain.Grant{
		Subject: "policy", Tool: "write_files", Op: "call", RunID: "run", SessionID: "session", InvocationID: "inv",
		ResourceScope: `["/ws/a.txt","/ws/b.txt"]`, State: "active",
	}
	req := Request{RunID: "run", SessionID: "session", InvocationID: "inv", Tool: "write_files", Resources: []string{"/ws/a.txt", "/ws/b.txt"}}
	if got := Decide([]domain.Grant{g}, req); !got.Allow {
		t.Fatalf("exact policy grant denied: %+v", got)
	}
	for _, tc := range []struct {
		name  string
		alter func(*Request)
	}{
		{"another invocation", func(req *Request) { req.InvocationID = "other" }},
		{"another run", func(req *Request) { req.RunID = "other" }},
		{"another session", func(req *Request) { req.SessionID = "other" }},
		{"another tool", func(req *Request) { req.Tool = "read_files" }},
		{"another target", func(req *Request) { req.Resources = []string{"/ws/a.txt", "/ws/c.txt"} }},
		{"extra target", func(req *Request) { req.Resources = []string{"/ws/a.txt", "/ws/b.txt", "/ws/c.txt"} }},
		{"fewer targets", func(req *Request) { req.Resources = []string{"/ws/a.txt"} }},
		{"no invocation", func(req *Request) { req.InvocationID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := req
			tc.alter(&changed)
			if got := Decide([]domain.Grant{g}, changed); got.Allow || got.NeedsApproval {
				t.Fatalf("policy grant widened: %+v", got)
			}
		})
	}
}
