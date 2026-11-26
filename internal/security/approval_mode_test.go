package security

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApprovalModeBoundaries(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	p := &Policy{Workspace: root}
	for _, mode := range []string{"ask", "safe", "workspace-write"} {
		for _, tool := range []string{"read_file", "list_dir", "list_files", "grep", "git_status", "git_diff", "git_log", "run_command", "write_file", "edit_file", "unknown"} {
			req := PolicyRequest{Tool: tool, Paths: []string{"file.txt"}, FileWrite: tool == "write_file" || tool == "edit_file"}
			got := ApplyApprovalMode(mode, p, req, PolicyDecision{Effect: EffectAsk})
			want := EffectAsk
			if mode != "ask" && tool != "run_command" && tool != "write_file" && tool != "edit_file" && tool != "unknown" {
				want = EffectAllow
			}
			if mode == "workspace-write" && req.FileWrite {
				want = EffectAllow
			}
			if got.Effect != want {
				t.Errorf("%s/%s=%s, want %s", mode, tool, got.Effect, want)
			}
			for _, source := range []string{"builtin", "workspace", "global"} {
				denied := PolicyDecision{Effect: EffectDeny, Source: source}
				if got := ApplyApprovalMode(mode, p, req, denied); got != denied {
					t.Errorf("%s overwrote %s deny for %s", mode, source, tool)
				}
			}
		}
	}
	for _, target := range []string{filepath.Join(outside, "file"), "../outside.txt", "escape/file", ".cloud-harness/policy.json"} {
		req := PolicyRequest{Tool: "write_file", Paths: []string{target}, FileWrite: true}
		if got := ApplyApprovalMode("workspace-write", p, req, PolicyDecision{Effect: EffectAsk}); got.Effect != EffectAsk {
			t.Errorf("mode authorized escaped/control path %q", target)
		}
		if got := ApplyApprovalMode("workspace-write", p, req, p.Evaluate(req)); got.Effect != EffectDeny {
			t.Errorf("mode bypassed built-in deny %q", target)
		}
	}
	for _, target := range []string{".env", ".env.local", ".cloud-harness/state"} {
		req := PolicyRequest{Tool: "read_file", Paths: []string{target}, FileRead: true}
		if got := ApplyApprovalMode("safe", p, req, p.Evaluate(req)); got.Effect != EffectDeny {
			t.Errorf("mode bypassed protected read %q", target)
		}
	}
	req := PolicyRequest{Tool: "run_command", Command: "echo allowed"}
	if got := ApplyApprovalMode("safe", p, req, PolicyDecision{Effect: EffectAllow, Source: "global"}); got.Effect != EffectAllow {
		t.Fatal("existing policy allow was lost")
	}
}
