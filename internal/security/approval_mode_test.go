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
	for _, mode := range []string{"ask", "safe", "workspace-write", "full-auto"} {
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
			if mode == "full-auto" {
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

func TestFullAutoPromotesEveryAskAndPreservesOtherDecisions(t *testing.T) {
	p := &Policy{Workspace: t.TempDir()}
	requests := []PolicyRequest{
		{Tool: "run_command", Command: "uname -a"},
		{Tool: "run_command", Command: "cat /proc/cpuinfo"},
		{Tool: "run_command", Command: "cat /proc/meminfo"},
		{Tool: "run_command", Command: "echo never-auto-approve"},
		{Tool: "write_file", Paths: []string{"out.txt"}, FileWrite: true},
		{Tool: "edit_file", Paths: []string{"out.txt"}, FileWrite: true},
		{Tool: "mcp.fixture.send"},
		{Tool: "new_tool_without_a_mode_whitelist"},
	}
	for _, req := range requests {
		t.Run(req.Tool+"/"+req.Command, func(t *testing.T) {
			ask := p.Evaluate(req)
			if ask.Effect != EffectAsk {
				t.Fatalf("fixture did not ask: %+v", ask)
			}
			got := ApplyApprovalMode("full-auto", p, req, ask)
			want := ask
			want.Effect, want.Source, want.Rule = EffectAllow, "session_mode", "full-auto session mode"
			if got != want {
				t.Fatalf("promotion lost audit selectors: %+v, want %+v", got, want)
			}
		})
	}
	// A policy may explicitly Ask for an outside write before the built-in
	// fallback. The mode transforms that result too; it does not evaluate paths.
	p.GlobalRules = []Rule{{Tool: "write_file", Effect: EffectAsk}}
	req := PolicyRequest{Tool: "write_file", Paths: []string{filepath.Join(t.TempDir(), "out.txt")}, FileWrite: true}
	if ask := p.Evaluate(req); ask.Effect != EffectAsk || ApplyApprovalMode("full-auto", p, req, ask).Effect != EffectAllow {
		t.Fatal("explicit outside-write Ask was not promoted")
	}
	for _, effect := range []Effect{EffectAllow, EffectDeny, Effect("invalid")} {
		decision := PolicyDecision{Effect: effect, Source: "global", Tool: "run_command", Rule: "original rule", RuleIndex: 2, CommandSHA256: "original hash"}
		if got := ApplyApprovalMode("full-auto", p, PolicyRequest{Tool: "run_command"}, decision); got != decision {
			t.Fatalf("non-Ask decision changed: %+v", got)
		}
	}
	if got := ApplyApprovalMode("unknown", p, req, PolicyDecision{Effect: EffectAsk}); got.Effect != EffectAsk {
		t.Fatal("unknown mode granted permission")
	}
}

func TestFullAutoCannotOverrideEvaluatedDeny(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		req          PolicyRequest
		rule         *Rule
	}{
		{"global command prefix", "global", PolicyRequest{Tool: "run_command", Command: "echo denied"}, &Rule{Tool: "run_command", CommandPrefix: "echo", Effect: EffectDeny}},
		{"workspace command prefix", "workspace", PolicyRequest{Tool: "run_command", Command: "echo denied"}, &Rule{Tool: "run_command", CommandPrefix: "echo", Effect: EffectDeny}},
		{"global path", "global", PolicyRequest{Tool: "write_file", Paths: []string{"blocked.txt"}, FileWrite: true}, &Rule{Tool: "write_file", PathGlob: "blocked.txt", Effect: EffectDeny}},
		{"workspace path", "workspace", PolicyRequest{Tool: "edit_file", Paths: []string{"blocked.txt"}, FileWrite: true}, &Rule{Tool: "edit_file", PathGlob: "blocked.txt", Effect: EffectDeny}},
		{"sensitive read", "builtin", PolicyRequest{Tool: "read_file", Paths: []string{".env"}, FileRead: true}, nil},
		{"control write", "builtin", PolicyRequest{Tool: "write_file", Paths: []string{".cloud-harness/policy.json"}, FileWrite: true}, nil},
		{"outside write", "builtin", PolicyRequest{Tool: "write_file", Paths: []string{filepath.Join(t.TempDir(), "outside.txt")}, FileWrite: true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Policy{Workspace: t.TempDir()}
			if tc.rule != nil {
				if tc.source == "global" {
					p.GlobalRules = []Rule{*tc.rule}
				} else {
					p.WorkspaceRules = []Rule{*tc.rule}
				}
			}
			denied := p.Evaluate(tc.req)
			if denied.Effect != EffectDeny || denied.Source != tc.source {
				t.Fatalf("fixture did not deny: %+v", denied)
			}
			if got := ApplyApprovalMode("full-auto", p, tc.req, denied); got != denied {
				t.Fatalf("evaluated deny changed: %+v", got)
			}
		})
	}
}
