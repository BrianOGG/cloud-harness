package security

import "testing"

func TestCommandPrefixDenyMatchesShellStages(t *testing.T) {
	for _, command := range []string{
		"blocked arg", "blocked; true", "true;blocked", "true && blocked arg", "false || blocked", "echo x | blocked", "true & blocked", "true\nblocked", "echo $(blocked)", "echo $(true;blocked)", "echo `blocked`", "(blocked)",
	} {
		t.Run(command, func(t *testing.T) {
			p := &Policy{Workspace: t.TempDir(), GlobalRules: []Rule{{Tool: "run_command", CommandPrefix: "blocked", Effect: EffectDeny}}}
			req := PolicyRequest{Tool: "run_command", Command: command}
			d := ApplyApprovalMode("full-auto", p, req, p.Evaluate(req))
			if d.Effect != EffectDeny || d.Source != "global" {
				t.Fatalf("deny lost: %+v", d)
			}
		})
	}
}

func TestCommandPrefixSplittingDoesNotWidenAllowOrAsk(t *testing.T) {
	for _, effect := range []Effect{EffectAllow, EffectAsk} {
		p := &Policy{Workspace: t.TempDir(), GlobalRules: []Rule{{Tool: "run_command", CommandPrefix: "approved", Effect: effect}}}
		for _, command := range []string{"true; approved", "true && approved", "echo x | approved", "echo $(approved)", "echo `approved`", "approvedevil", "approved; true"} {
			d := p.Evaluate(PolicyRequest{Tool: "run_command", Command: command})
			if d.Effect != EffectAsk || d.Source != "default" {
				t.Fatalf("effect=%s command=%q widened matching: %+v", effect, command, d)
			}
		}
		if d := p.Evaluate(PolicyRequest{Tool: "run_command", Command: "approved arg"}); d.Effect != effect || d.Source != "global" {
			t.Fatalf("whole-command matching changed: %+v", d)
		}
	}
	p := &Policy{Workspace: t.TempDir(), GlobalRules: []Rule{{Tool: "run_command", CommandPrefix: "blocked", PathGlob: "docs/**", Effect: EffectDeny}}}
	for _, req := range []PolicyRequest{
		{Tool: "run_command", Command: "true && blockedevil", Paths: []string{"docs/a"}},
		{Tool: "run_command", Command: "true && blocked", Paths: []string{"src/a"}},
		{Tool: "read_file", Command: "true && blocked", Paths: []string{"docs/a"}},
	} {
		if d := p.Evaluate(req); d.Effect != EffectAsk {
			t.Fatalf("selectors no longer conjunctive: %+v", d)
		}
	}
}

func TestCommandPrefixDenyIsTerminalBeforeOuterAllow(t *testing.T) {
	for _, effect := range []Effect{EffectAllow, EffectAsk} {
		p := &Policy{Workspace: t.TempDir(), GlobalRules: []Rule{
			{Tool: "run_command", CommandPrefix: "echo", Effect: effect},
			{Tool: "run_command", CommandPrefix: "blocked", Effect: EffectDeny},
		}}
		for _, command := range []string{"blocked", "echo x | blocked"} {
			if d := p.Evaluate(PolicyRequest{Tool: "run_command", Command: command}); d.Effect != EffectDeny || d.RuleIndex != 2 {
				t.Fatalf("outer %s hid deny: %+v", effect, d)
			}
		}
	}
}
